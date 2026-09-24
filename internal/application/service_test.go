package application

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/sqlite"
	"github.com/freezeChen/frz-tools/internal/domain"

	_ "modernc.org/sqlite"
)

type fakeExec struct {
	mu    sync.Mutex
	calls []domain.CommandSpec
	run   func(ctx context.Context, spec domain.CommandSpec) (domain.Result, error)
}

func (f *fakeExec) Run(ctx context.Context, spec domain.CommandSpec) (domain.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, spec)
	f.mu.Unlock()
	if f.run != nil {
		return f.run(ctx, spec)
	}
	return domain.Result{Executed: !spec.DryRun, ExitCode: 0}, nil
}

func (f *fakeExec) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func allowAll(string) bool { return true }

func newTestRuntime(t *testing.T, exec Executor, allow func(string) bool) (*Runtime, *sqlite.Store) {
	return newTestRuntimeWith(t, Options{Executor: exec, AllowExecutable: allow})
}

func newTestRuntimeWith(t *testing.T, opts Options) (*Runtime, *sqlite.Store) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "opsd.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	opts.Repo = store
	if opts.Executor == nil {
		opts.Executor = &fakeExec{}
	}
	if opts.AllowExecutable == nil {
		opts.AllowExecutable = allowAll
	}
	if opts.Defaults.Timeout == 0 {
		opts.Defaults = Defaults{Timeout: 5 * time.Second, MaxOutputBytes: 4096, SensitiveEnvKeys: []string{"TOKEN"}}
	}
	if opts.Workers == 0 {
		opts.Workers = 1
	}
	if opts.Idle == 0 {
		opts.Idle = 20 * time.Millisecond
	}
	if opts.Logger == nil {
		opts.Logger = discardLogger()
	}

	return NewRuntime(opts), store
}

func submitRequest(resource string) v1.CreateOperationRequest {
	return v1.CreateOperationRequest{
		Kind:     v1.KindExecutorCommand,
		Resource: resource,
		Spec:     json.RawMessage(`{"argv":["/usr/bin/true"]}`),
	}
}

func TestCreateValidatesRequest(t *testing.T) {
	rt, _ := newTestRuntime(t, &fakeExec{}, allowAll)
	ctx := context.Background()

	cases := map[string]v1.CreateOperationRequest{
		"missing kind":     {Resource: "r", Spec: json.RawMessage(`{"argv":["/usr/bin/true"]}`)},
		"missing resource": {Kind: v1.KindExecutorCommand, Spec: json.RawMessage(`{"argv":["/usr/bin/true"]}`)},
		"missing spec":     {Kind: v1.KindExecutorCommand, Resource: "r"},
		"empty argv":       {Kind: v1.KindExecutorCommand, Resource: "r", Spec: json.RawMessage(`{"argv":[]}`)},
		"relative argv":    {Kind: v1.KindExecutorCommand, Resource: "r", Spec: json.RawMessage(`{"argv":["true"]}`)},
		"unknown kind":     {Kind: "nope", Resource: "r", Spec: json.RawMessage(`{"argv":["/usr/bin/true"]}`)},
		"unknown field":    {Kind: v1.KindExecutorCommand, Resource: "r", Spec: json.RawMessage(`{"argv":["/usr/bin/true"],"bogus":1}`)},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := rt.Service.Create(ctx, req); domain.CodeOf(err) != v1.CodeInvalidRequest {
				t.Fatalf("want INVALID_REQUEST, got %v", err)
			}
		})
	}
}

func TestCreateRejectsDisallowedExecutable(t *testing.T) {
	rt, _ := newTestRuntime(t, &fakeExec{}, func(string) bool { return false })
	_, _, err := rt.Service.Create(context.Background(), submitRequest("r"))
	if domain.CodeOf(err) != v1.CodePermissionDenied {
		t.Fatalf("want PERMISSION_DENIED, got %v", err)
	}
}

func TestCreateIdempotency(t *testing.T) {
	rt, _ := newTestRuntime(t, &fakeExec{}, allowAll)
	ctx := context.Background()

	req := submitRequest("demo")
	req.IdempotencyKey = "k1"

	first, created, err := rt.Service.Create(ctx, req)
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}

	second, created, err := rt.Service.Create(ctx, req)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created || second.ID != first.ID {
		t.Fatalf("duplicate request must reuse %s, got %s (created=%v)", first.ID, second.ID, created)
	}

	conflict := submitRequest("demo")
	conflict.IdempotencyKey = "k1"
	conflict.Spec = json.RawMessage(`{"argv":["/usr/bin/false"]}`)
	if _, _, err := rt.Service.Create(ctx, conflict); domain.CodeOf(err) != v1.CodeIdempotencyConflict {
		t.Fatalf("want IDEMPOTENCY_CONFLICT, got %v", err)
	}
}

func TestRequestHashIgnoresKeyOrder(t *testing.T) {
	a, err := requestHash(v1.KindExecutorCommand, "r", false, nil, json.RawMessage(`{"argv":["/usr/bin/true"],"timeoutSeconds":5}`))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	b, err := requestHash(v1.KindExecutorCommand, "r", false, nil, json.RawMessage(`{"timeoutSeconds":5,"argv":["/usr/bin/true"]}`))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if a != b {
		t.Fatalf("hash must be order independent: %s != %s", a, b)
	}
}

func TestWorkerExecutesOperationAndRedactsArgv(t *testing.T) {
	exec := &fakeExec{}
	rt, store := newTestRuntime(t, exec, allowAll)
	ctx := context.Background()

	req := submitRequest("run")
	req.Spec = json.RawMessage(`{"argv":["/usr/bin/tool","--token","secret-value"],"sensitiveArgIndexes":[2]}`)
	op, _, err := rt.Service.Create(ctx, req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	worked, err := rt.Pool.ProcessNext(ctx)
	if err != nil || !worked {
		t.Fatalf("process next: worked=%v err=%v", worked, err)
	}

	stored, err := store.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status != domain.StatusSucceeded {
		t.Fatalf("want succeeded, got %s", stored.Status)
	}
	if stored.ExitCode == nil || *stored.ExitCode != 0 {
		t.Fatalf("want exit code 0, got %v", stored.ExitCode)
	}

	logs, err := store.ListLogs(ctx, op.ID, 0, 100)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	joined := ""
	for _, entry := range logs {
		joined += entry.Message + " " + entry.Fields["argv"]
	}
	if contains(joined, "secret-value") {
		t.Fatalf("sensitive argv must not reach the logs: %s", joined)
	}
	if !contains(joined, "[redacted]") {
		t.Fatalf("expected a redaction marker in logs: %s", joined)
	}
}

func TestWorkerRecordsFailureAndExitCode(t *testing.T) {
	exec := &fakeExec{run: func(context.Context, domain.CommandSpec) (domain.Result, error) {
		return domain.Result{Executed: true, ExitCode: 7, Stderr: "boom"}, domain.NewError(v1.CodeExecExitNonZero, "command exited with code 7")
	}}
	rt, store := newTestRuntime(t, exec, allowAll)
	ctx := context.Background()

	op, _, err := rt.Service.Create(ctx, submitRequest("fail"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("process next: %v", err)
	}

	stored, err := store.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status != domain.StatusFailed {
		t.Fatalf("want failed, got %s", stored.Status)
	}
	if stored.ErrorCode != string(v1.CodeExecExitNonZero) {
		t.Fatalf("want EXEC_EXIT_NONZERO, got %q", stored.ErrorCode)
	}
	if stored.ExitCode == nil || *stored.ExitCode != 7 {
		t.Fatalf("want exit code 7, got %v", stored.ExitCode)
	}
}

func TestWorkerTimeoutIsFailure(t *testing.T) {
	exec := &fakeExec{run: func(context.Context, domain.CommandSpec) (domain.Result, error) {
		return domain.Result{Executed: true}, domain.NewError(v1.CodeExecTimeout, "timed out")
	}}
	rt, store := newTestRuntime(t, exec, allowAll)
	ctx := context.Background()

	op, _, _ := rt.Service.Create(ctx, submitRequest("slow"))
	if _, err := rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("process next: %v", err)
	}

	stored, _ := store.GetOperation(ctx, op.ID)
	if stored.Status != domain.StatusFailed || stored.ErrorCode != string(v1.CodeExecTimeout) {
		t.Fatalf("want failed/EXEC_TIMEOUT, got %s/%s", stored.Status, stored.ErrorCode)
	}
}

func TestDryRunPropagatesAndDoesNotSpawn(t *testing.T) {
	exec := &fakeExec{}
	rt, store := newTestRuntime(t, exec, allowAll)
	ctx := context.Background()

	req := submitRequest("plan")
	req.DryRun = true
	op, _, err := rt.Service.Create(ctx, req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("process next: %v", err)
	}

	if len(exec.calls) != 1 || !exec.calls[0].DryRun {
		t.Fatalf("dry-run must be propagated to the executor: %+v", exec.calls)
	}

	stored, _ := store.GetOperation(ctx, op.ID)
	if stored.Status != domain.StatusSucceeded {
		t.Fatalf("dry run should succeed, got %s", stored.Status)
	}
	if stored.ExitCode != nil {
		t.Fatalf("dry run must not report an exit code, got %v", *stored.ExitCode)
	}
}

func TestCancelPendingOperationThroughService(t *testing.T) {
	rt, store := newTestRuntime(t, &fakeExec{}, allowAll)
	ctx := context.Background()

	op, _, err := rt.Service.Create(ctx, submitRequest("cancel"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cancelled, err := rt.Service.Cancel(ctx, op.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.Status != domain.StatusCancelled {
		t.Fatalf("want cancelled, got %s", cancelled.Status)
	}

	if worked, _ := rt.Pool.ProcessNext(ctx); worked {
		t.Fatal("a cancelled operation must not be processed")
	}
	stored, _ := store.GetOperation(ctx, op.ID)
	if stored.Status != domain.StatusCancelled {
		t.Fatalf("want cancelled, got %s", stored.Status)
	}
}

func TestCancelRunningOperationSignalsWorker(t *testing.T) {
	blocking := &fakeExec{run: func(ctx context.Context, spec domain.CommandSpec) (domain.Result, error) {
		<-ctx.Done()
		return domain.Result{Executed: true}, domain.NewError(v1.CodeExecCancelled, "operation was cancelled")
	}}
	rt, store := newTestRuntime(t, blocking, allowAll)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	op, _, err := rt.Service.Create(ctx, submitRequest("long"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rt.Pool.Start(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for {
		stored, err := store.GetOperation(ctx, op.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if stored.Status == domain.StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation never started, status=%s", stored.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := rt.Service.Cancel(ctx, op.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	for {
		stored, err := store.GetOperation(ctx, op.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if stored.Status == domain.StatusCancelled {
			if stored.ErrorCode != string(v1.CodeExecCancelled) {
				t.Fatalf("want EXEC_CANCELLED, got %q", stored.ErrorCode)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation was not cancelled, status=%s", stored.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRetryRequiresFailedOrCancelledStatus(t *testing.T) {
	rt, store := newTestRuntime(t, &fakeExec{}, allowAll)
	ctx := context.Background()

	op, _, _ := rt.Service.Create(ctx, submitRequest("retry"))
	if _, err := rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("process next: %v", err)
	}
	if _, err := rt.Service.Retry(ctx, op.ID); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("retrying a succeeded operation must be rejected, got %v", err)
	}

	failed := &fakeExec{run: func(context.Context, domain.CommandSpec) (domain.Result, error) {
		return domain.Result{Executed: true, ExitCode: 1}, domain.NewError(v1.CodeExecExitNonZero, "failed")
	}}
	rt2, store2 := newTestRuntime(t, failed, allowAll)
	op2, _, _ := rt2.Service.Create(ctx, submitRequest("retry"))
	if _, err := rt2.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("process next: %v", err)
	}

	retried, err := rt2.Service.Retry(ctx, op2.ID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retried.ID == op2.ID {
		t.Fatal("retry must create a new operation id")
	}
	if retried.RetryOf != op2.ID {
		t.Fatalf("want retryOf %s, got %s", op2.ID, retried.RetryOf)
	}
	if retried.Status != domain.StatusPending {
		t.Fatalf("want pending, got %s", retried.Status)
	}
	if retried.IdempotencyKey != "" {
		t.Fatal("retry must not reuse the original idempotency key")
	}
	_ = store
	_ = store2
}

func TestRecoverFailsInterruptedOperation(t *testing.T) {
	rt, store := newTestRuntime(t, &fakeExec{}, allowAll)
	ctx := context.Background()

	op, _, _ := rt.Service.Create(ctx, submitRequest("interrupted"))
	if _, err := store.ClaimNextPending(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("claim: %v", err)
	}

	count, err := Recover(ctx, store, discardLogger(), time.Now().UTC(), RecoveryOptions{})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if count != 1 {
		t.Fatalf("want 1 recovered operation, got %d", count)
	}

	stored, _ := store.GetOperation(ctx, op.ID)
	if stored.Status != domain.StatusFailed || stored.ErrorCode != string(v1.CodeDaemonRestarted) {
		t.Fatalf("want failed/DAEMON_RESTARTED, got %s/%s", stored.Status, stored.ErrorCode)
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

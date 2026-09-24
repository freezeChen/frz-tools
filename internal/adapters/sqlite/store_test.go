package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/idgen"
	"github.com/freezeChen/frz-tools/migrations"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "opsd.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func newOperation(resource string) *domain.Operation {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &domain.Operation{
		ID:          idgen.NewOperationID(),
		Kind:        v1.KindExecutorCommand,
		Resource:    resource,
		Status:      domain.StatusPending,
		RequestHash: idgen.New("rh"),
		Spec:        json.RawMessage(`{"argv":["/usr/bin/true"]}`),
		CreatedAt:   now,
		CreatedBy:   "test",
	}
}

func TestMigrateConfiguresWALAndForeignKeys(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	mode, err := store.JournalMode(ctx)
	if err != nil {
		t.Fatalf("journal mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("want journal_mode wal, got %q", mode)
	}
	enabled, err := store.ForeignKeysEnabled(ctx)
	if err != nil {
		t.Fatalf("foreign keys: %v", err)
	}
	if !enabled {
		t.Fatal("foreign_keys must be enabled")
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	want, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}

	var applied int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != len(want) {
		t.Fatalf("want %d applied migrations, got %d", len(want), applied)
	}
}

func TestCreateOperationIdempotency(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	op := newOperation("demo")
	op.IdempotencyKey = "key-1"

	first, err := store.CreateOperation(ctx, op)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !first.Created {
		t.Fatal("first create must report Created=true")
	}

	duplicate := newOperation("demo")
	duplicate.IdempotencyKey = "key-1"
	duplicate.RequestHash = op.RequestHash

	second, err := store.CreateOperation(ctx, duplicate)
	if err != nil {
		t.Fatalf("duplicate create: %v", err)
	}
	if second.Created {
		t.Fatal("duplicate request must reuse the original operation")
	}
	if second.Operation.ID != first.Operation.ID {
		t.Fatalf("want reuse of %s, got %s", first.Operation.ID, second.Operation.ID)
	}

	conflicting := newOperation("demo")
	conflicting.IdempotencyKey = "key-1"
	conflicting.RequestHash = "different-hash"

	if _, err := store.CreateOperation(ctx, conflicting); domain.CodeOf(err) != v1.CodeIdempotencyConflict {
		t.Fatalf("want IDEMPOTENCY_CONFLICT, got %v", err)
	}
}

func TestSubmitRejectedWhileResourceLocked(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	running := newOperation("shared")
	if _, err := store.CreateOperation(ctx, running); err != nil {
		t.Fatalf("create: %v", err)
	}
	claimed, err := store.ClaimNextPending(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected to claim the pending operation")
	}

	if _, err := store.CreateOperation(ctx, newOperation("shared")); domain.CodeOf(err) != v1.CodeLockBusy {
		t.Fatalf("want LOCK_BUSY, got %v", err)
	}
}

func TestCreateRejectsResourceWithIncompleteOperation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first := newOperation("serial")
	first.CreatedAt = now.Add(-time.Second)
	if _, err := store.CreateOperation(ctx, first); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := store.CreateOperation(ctx, newOperation("serial")); domain.CodeOf(err) != v1.CodeLockBusy {
		t.Fatalf("want LOCK_BUSY while a pending operation exists, got %v", err)
	}

	claimed, err := store.ClaimNextPending(ctx, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ID != first.ID {
		t.Fatalf("expected to claim the pending operation, got %+v", claimed)
	}
	if _, err := store.CreateOperation(ctx, newOperation("serial")); domain.CodeOf(err) != v1.CodeLockBusy {
		t.Fatalf("want LOCK_BUSY while a running operation exists, got %v", err)
	}

	if _, err := store.Finish(ctx, domain.FinishInput{
		OperationID: first.ID,
		Status:      domain.StatusSucceeded,
	}, now); err != nil {
		t.Fatalf("finish: %v", err)
	}

	if _, err := store.CreateOperation(ctx, newOperation("serial")); err != nil {
		t.Fatalf("resource must be reusable once the operation is terminal: %v", err)
	}
}

func TestFinishReleasesLock(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	op := newOperation("release")
	if _, err := store.CreateOperation(ctx, op); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.ClaimNextPending(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if lock, err := store.ActiveLock(ctx, "release"); err != nil || lock == nil {
		t.Fatalf("expected an active lock, got %+v (%v)", lock, err)
	}

	exit := 0
	finished, err := store.Finish(ctx, domain.FinishInput{
		OperationID: op.ID,
		Status:      domain.StatusSucceeded,
		ExitCode:    &exit,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if finished.Status != domain.StatusSucceeded {
		t.Fatalf("want succeeded, got %s", finished.Status)
	}
	if lock, err := store.ActiveLock(ctx, "release"); err != nil || lock != nil {
		t.Fatalf("lock must be released, got %+v (%v)", lock, err)
	}
}

func TestRecoverRunningMarksFailedAndReleasesLocks(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	op := newOperation("crash")
	if _, err := store.CreateOperation(ctx, op); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.ClaimNextPending(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("claim: %v", err)
	}

	recovered, err := store.RecoverRunning(ctx, time.Now().UTC(), nil)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(recovered) != 1 || recovered[0].ID != op.ID {
		t.Fatalf("want the running operation recovered, got %+v", recovered)
	}

	stored, err := store.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status != domain.StatusFailed {
		t.Fatalf("want failed, got %s", stored.Status)
	}
	if stored.ErrorCode != string(v1.CodeDaemonRestarted) {
		t.Fatalf("want DAEMON_RESTARTED, got %q", stored.ErrorCode)
	}
	if lock, err := store.ActiveLock(ctx, "crash"); err != nil || lock != nil {
		t.Fatalf("lock must be released after recovery, got %+v (%v)", lock, err)
	}

	events, err := store.ListAudit(ctx, op.ID)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	found := false
	for _, e := range events {
		if e.EventType == domain.EventDaemonRecovered {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a daemon.recovered audit event")
	}
}

func TestGetOperationNotFound(t *testing.T) {
	store := newTestStore(t)
	_, err := store.GetOperation(context.Background(), "op_missing")
	if domain.CodeOf(err) != v1.CodeOperationNotFound {
		t.Fatalf("want OPERATION_NOT_FOUND, got %v", err)
	}
}

func TestCancelPendingOperation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	op := newOperation("cancel-me")
	if _, err := store.CreateOperation(ctx, op); err != nil {
		t.Fatalf("create: %v", err)
	}

	cancelled, ok, err := store.CancelPending(ctx, op.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !ok || cancelled.Status != domain.StatusCancelled {
		t.Fatalf("want cancelled=true and status cancelled, got %v %s", ok, cancelled.Status)
	}

	if _, ok, err := store.CancelPending(ctx, op.ID, time.Now().UTC()); err != nil || ok {
		t.Fatalf("second cancel must be a no-op, got ok=%v err=%v", ok, err)
	}
}

func TestLogsAreOrderedAndCursorable(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	op := newOperation("logs")
	now := time.Now().UTC()

	if _, err := store.CreateOperation(ctx, op); err != nil {
		t.Fatalf("create: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := store.AppendLog(ctx, domain.LogEntry{
			OperationID: op.ID,
			Level:       "info",
			Message:     "line",
			Phase:       domain.PhaseExecute,
			Time:        now,
		}); err != nil {
			t.Fatalf("append log: %v", err)
		}
	}

	all, err := store.ListLogs(ctx, op.ID, 0, 10)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 logs, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatal("logs must be ordered by id")
		}
	}

	tail, err := store.ListLogs(ctx, op.ID, all[0].ID, 10)
	if err != nil {
		t.Fatalf("list logs after cursor: %v", err)
	}
	if len(tail) != 2 {
		t.Fatalf("want 2 logs after cursor, got %d", len(tail))
	}
}

func TestUnknownErrorMapsToInternal(t *testing.T) {
	if domain.CodeOf(errors.New("boom")) != v1.CodeInternal {
		t.Fatal("uncoded error must map to INTERNAL")
	}
}

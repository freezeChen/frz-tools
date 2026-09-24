package application

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/sqlite"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// fakeRuntimeAdapter 是应用层用例用的最小 RuntimeAdapter：它只记录调用序列与错误注入，
// 让「start 必须先 prepare」「取消要落成 cancelled」这类语义可以被断言，
// 而不需要真机上的 systemd（真机是 A7 的 Linux 容器证据）。
type fakeRuntimeAdapter struct {
	mu    sync.Mutex
	calls []string
	last  *domain.ApplicationSpec

	validateErr error
	prepareErr  error
	startErr    error
	stopErr     error
	// prepareErrWhen 非 nil 时先问它，用来构造「只让某一版失败」的场景：回滚要成功，
	// 就必须让上一个版本的 Prepare 照常通过，否则测的就成了「回滚也失败」那条路径。
	prepareErrWhen func(*domain.ApplicationSpec) error
	health         domain.RuntimeHealth
	healthErr      error

	// startEntered 非 nil 时 Start 会先关闭它再阻塞到 ctx 结束，用于驱动取消路径。
	startEntered chan struct{}
}

func (f *fakeRuntimeAdapter) record(name string, spec *domain.ApplicationSpec) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	f.last = spec
}

func (f *fakeRuntimeAdapter) callNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRuntimeAdapter) lastSpec() *domain.ApplicationSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func (f *fakeRuntimeAdapter) Validate(_ context.Context, spec *domain.ApplicationSpec) error {
	f.record("validate", spec)
	return f.validateErr
}

func (f *fakeRuntimeAdapter) Prepare(_ context.Context, spec *domain.ApplicationSpec) error {
	f.record("prepare", spec)
	if f.prepareErrWhen != nil {
		if err := f.prepareErrWhen(spec); err != nil {
			return err
		}
	}
	return f.prepareErr
}

func (f *fakeRuntimeAdapter) Start(ctx context.Context, spec *domain.ApplicationSpec) error {
	f.record("start", spec)
	if f.startEntered != nil {
		close(f.startEntered)
		<-ctx.Done()
		return ctx.Err()
	}
	return f.startErr
}

func (f *fakeRuntimeAdapter) Stop(_ context.Context, spec *domain.ApplicationSpec) error {
	f.record("stop", spec)
	return f.stopErr
}

func (f *fakeRuntimeAdapter) Health(_ context.Context, spec *domain.ApplicationSpec) (domain.RuntimeHealth, error) {
	f.record("health", spec)
	return f.health, f.healthErr
}

func (f *fakeRuntimeAdapter) Status(_ context.Context, spec *domain.ApplicationSpec) (domain.RuntimeStatus, error) {
	f.record("status", spec)
	return domain.RuntimeActive, nil
}

type fakePrepareReporter struct {
	decision RuntimeDecision
	ok       bool
	calls    int
}

func (f *fakePrepareReporter) ReportRuntimePrepare(_ context.Context, _ *domain.ApplicationSpec) (RuntimeDecision, bool) {
	f.calls++
	if !f.ok {
		return RuntimeDecision{}, false
	}
	return f.decision, true
}

// seedApplicationWithSpec 建一个应用并提交一份合法规格，返回应用记录。
func seedApplicationWithSpec(t *testing.T, rt *Runtime, name string) *domain.Application {
	t.Helper()
	ctx := context.Background()

	app, err := rt.Catalogs.CreateApplication(ctx, name, nil)
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	if _, err := rt.Specs.PutSpec(ctx, app.Name, validSpec(app.Name), "tester"); err != nil {
		t.Fatalf("put spec: %v", err)
	}
	return app
}

func submitRuntime(t *testing.T, rt *Runtime, kind, appRef string) *domain.Operation {
	t.Helper()
	op, created, err := rt.Service.Create(context.Background(), v1.CreateOperationRequest{
		Kind:      kind,
		Resource:  appRef,
		CreatedBy: "tester",
	})
	if err != nil {
		t.Fatalf("create %s for %s: %v", kind, appRef, err)
	}
	if !created {
		t.Fatalf("want a newly created operation for %s", kind)
	}
	return op
}

func operationLogs(t *testing.T, rt *Runtime, id string) []domain.LogEntry {
	t.Helper()
	logs, err := rt.Service.Logs(context.Background(), id, 0, 0)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	return logs
}

func countOperations(t *testing.T, store *sqlite.Store, kind string) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM operations WHERE kind = ?`, kind).Scan(&count); err != nil {
		t.Fatalf("count operations: %v", err)
	}
	return count
}

func TestRuntimeStartPreparesThenStartsAndRecordsDecision(t *testing.T) {
	adapter := &fakeRuntimeAdapter{}
	reporter := &fakePrepareReporter{ok: true, decision: RuntimeDecision{
		UnitPath:       "/etc/systemd/system/billing-api.service",
		Tier:           "strict",
		SystemdVersion: 255,
		Degradations:   []string{"日志改为 journal"},
		DecidedAt:      time.Now().UTC(),
	}}
	exec := &fakeExec{}
	rt, store := newTestRuntimeWith(t, Options{Executor: exec, RuntimeAdapter: adapter, PrepareReporter: reporter})
	app := seedApplicationWithSpec(t, rt, "billing-api")

	op := submitRuntime(t, rt, v1.KindRuntimeStart, app.Name)

	// 资源被规范成应用 ID：用名称与用 ID 提交必须命中同一把锁。
	if op.Resource != app.ID {
		t.Fatalf("resource want the application id %s, got %s", app.ID, op.Resource)
	}
	if len(op.Spec) != 0 {
		t.Fatalf("runtime 操作不携带 spec 快照，执行时读当前规格，got %s", string(op.Spec))
	}
	if op.Status != domain.StatusPending {
		t.Fatalf("want pending, got %s", op.Status)
	}
	// 创建侧只做无副作用的校验：此时还没有 prepare/start。
	if calls := adapter.callNames(); len(calls) != 1 || calls[0] != "validate" {
		t.Fatalf("创建阶段只应 Validate 一次，got %v", calls)
	}

	worked, err := rt.Pool.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("process next: %v", err)
	}
	if !worked {
		t.Fatal("want the runtime.start operation to be claimed")
	}

	got, err := rt.Service.Get(context.Background(), op.ID)
	if err != nil {
		t.Fatalf("get operation: %v", err)
	}
	if got.Status != domain.StatusSucceeded {
		t.Fatalf("want succeeded, got %s (%s: %s)", got.Status, got.ErrorCode, got.ErrorMessage)
	}
	// 顺序是语义的一部分：适配器合约要求未 Prepare 的规格不能被 Start 拒绝掉。
	if calls := adapter.callNames(); strings.Join(calls, ",") != "validate,prepare,start" {
		t.Fatalf("want validate,prepare,start, got %v", calls)
	}
	if spec := adapter.lastSpec(); spec == nil || spec.Systemd.UnitName != "billing-api.service" {
		t.Fatalf("适配器拿到的规格不对：%+v", spec)
	}
	// runtime.* 必须落在适配器上，不能顺手把它交给本机执行器。
	if exec.callCount() != 0 {
		t.Fatalf("runtime 操作不应调用执行器，got %d 次", exec.callCount())
	}

	// 档位决策必须在 Operation 日志里可查：同一份 manifest 在不同主机上生成的 unit 不同。
	logs := operationLogs(t, rt, op.ID)
	var decisionLog *domain.LogEntry
	started := false
	for i := range logs {
		entry := &logs[i]
		if entry.Phase == domain.PhasePrepare && strings.Contains(entry.Message, "档位") {
			decisionLog = entry
		}
		if entry.Message == "应用已启动" {
			started = true
		}
	}
	if decisionLog == nil {
		t.Fatalf("日志里缺少档位记录：%+v", logs)
	}
	if decisionLog.Fields["tier"] != "strict" || decisionLog.Fields["systemdVersion"] != "255" {
		t.Fatalf("档位字段不完整：%+v", decisionLog.Fields)
	}
	if decisionLog.Fields["unitPath"] != "/etc/systemd/system/billing-api.service" {
		t.Fatalf("unitPath 缺失：%+v", decisionLog.Fields)
	}
	if !strings.Contains(decisionLog.Fields["degradations"], "journal") {
		t.Fatalf("降级说明缺失：%+v", decisionLog.Fields)
	}
	if !started {
		t.Fatalf("日志里缺少启动成功记录：%+v", logs)
	}

	// 审计里也要有这份决策（规格 §6：档位必须写进 unit 注释与审计）。
	var (
		details string
		unit    string
	)
	if err := store.DB().QueryRow(
		`SELECT details_json, resource FROM audit_events WHERE event_type = ? AND operation_id = ?`,
		domain.EventRuntimePrepared, op.ID).Scan(&details, &unit); err != nil {
		t.Fatalf("query runtime.prepared audit: %v", err)
	}
	if unit != app.ID {
		t.Fatalf("审计的 resource 应为应用 ID，got %q", unit)
	}
	if !strings.Contains(details, `"tier":"strict"`) || !strings.Contains(details, `"systemdVersion":"255"`) {
		t.Fatalf("审计里缺少档位信息：%s", details)
	}
	if reporter.calls != 1 {
		t.Fatalf("reporter 应只被调用一次，got %d", reporter.calls)
	}
}

func TestRuntimeStopOnlyStops(t *testing.T) {
	adapter := &fakeRuntimeAdapter{}
	rt, _ := newTestRuntimeWith(t, Options{RuntimeAdapter: adapter})
	app := seedApplicationWithSpec(t, rt, "billing-api")

	op := submitRuntime(t, rt, v1.KindRuntimeStop, app.ID)
	if _, err := rt.Pool.ProcessNext(context.Background()); err != nil {
		t.Fatalf("process next: %v", err)
	}

	got, err := rt.Service.Get(context.Background(), op.ID)
	if err != nil {
		t.Fatalf("get operation: %v", err)
	}
	if got.Status != domain.StatusSucceeded {
		t.Fatalf("want succeeded, got %s (%s)", got.Status, got.ErrorCode)
	}
	if calls := adapter.callNames(); strings.Join(calls, ",") != "validate,stop" {
		t.Fatalf("stop 不应触发 prepare，got %v", calls)
	}
}

func TestRuntimeOperationRequiresManifest(t *testing.T) {
	adapter := &fakeRuntimeAdapter{}
	rt, store := newTestRuntimeWith(t, Options{RuntimeAdapter: adapter})
	if _, err := rt.Catalogs.CreateApplication(context.Background(), "empty-app", nil); err != nil {
		t.Fatalf("create application: %v", err)
	}

	_, _, err := rt.Service.Create(context.Background(), v1.CreateOperationRequest{
		Kind:     v1.KindRuntimeStart,
		Resource: "empty-app",
	})
	if domain.CodeOf(err) != v1.CodeSpecNotFound {
		t.Fatalf("want SPEC_NOT_FOUND, got %s (%v)", domain.CodeOf(err), err)
	}
	// 校验失败不得留下一条注定失败的 Operation。
	if count := countOperations(t, store, v1.KindRuntimeStart); count != 0 {
		t.Fatalf("want no operation row, got %d", count)
	}
	if calls := adapter.callNames(); len(calls) != 0 {
		t.Fatalf("未登记 manifest 时不应碰适配器，got %v", calls)
	}
}

func TestRuntimeOperationRequiresAdapter(t *testing.T) {
	// 非 Linux 的部署形态：options 里没有 RuntimeAdapter。
	rt, store := newTestRuntimeWith(t, Options{})
	app := seedApplicationWithSpec(t, rt, "billing-api")

	_, _, err := rt.Service.Create(context.Background(), v1.CreateOperationRequest{
		Kind:     v1.KindRuntimeStart,
		Resource: app.Name,
	})
	if domain.CodeOf(err) != v1.CodeRuntimeUnsupport {
		t.Fatalf("want RUNTIME_UNSUPPORTED, got %s (%v)", domain.CodeOf(err), err)
	}
	// 选择「快速失败、不建 Operation」：适配器不可用是部署属性，排队执行没有意义，
	// 失败的操作行只会污染审计与 operation 列表。
	if count := countOperations(t, store, v1.KindRuntimeStart); count != 0 {
		t.Fatalf("want no operation row, got %d", count)
	}

	if _, _, err := rt.Runtimes.Validate(context.Background(), app.Name); domain.CodeOf(err) != v1.CodeRuntimeUnsupport {
		t.Fatalf("sync validate want RUNTIME_UNSUPPORTED, got %s", domain.CodeOf(err))
	}
	if _, _, _, _, err := rt.Runtimes.Prepare(context.Background(), app.Name); domain.CodeOf(err) != v1.CodeRuntimeUnsupport {
		t.Fatalf("sync prepare want RUNTIME_UNSUPPORTED, got %s", domain.CodeOf(err))
	}
	if _, _, _, err := rt.Runtimes.Health(context.Background(), app.Name); domain.CodeOf(err) != v1.CodeRuntimeUnsupport {
		t.Fatalf("health want RUNTIME_UNSUPPORTED, got %s", domain.CodeOf(err))
	}
}

func TestRuntimeOperationRejectsDryRun(t *testing.T) {
	adapter := &fakeRuntimeAdapter{}
	rt, store := newTestRuntimeWith(t, Options{RuntimeAdapter: adapter})
	app := seedApplicationWithSpec(t, rt, "billing-api")

	_, _, err := rt.Service.Create(context.Background(), v1.CreateOperationRequest{
		Kind:     v1.KindRuntimeStart,
		Resource: app.Name,
		DryRun:   true,
	})
	if domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST, got %s (%v)", domain.CodeOf(err), err)
	}
	// dry-run 无法阻止适配器真的启停进程，因此宁可拒绝也不接受一个说谎的开关。
	if !strings.Contains(domain.MessageOf(err), "runtime validate") {
		t.Fatalf("错误信息应指路 runtime validate：%s", domain.MessageOf(err))
	}
	if count := countOperations(t, store, v1.KindRuntimeStart); count != 0 {
		t.Fatalf("want no operation row, got %d", count)
	}
}

func TestRuntimeOperationKindIsRestricted(t *testing.T) {
	adapter := &fakeRuntimeAdapter{}
	rt, _ := newTestRuntimeWith(t, Options{RuntimeAdapter: adapter})
	app := seedApplicationWithSpec(t, rt, "billing-api")

	// 只有 start/stop 属于运行时操作；别的 kind 不进这条路径。
	if _, err := rt.Runtimes.ResolveRuntimeOperation(context.Background(), "runtime.restart", app.Name); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
}

func TestRuntimeOperationIsIdempotentByKey(t *testing.T) {
	adapter := &fakeRuntimeAdapter{}
	rt, store := newTestRuntimeWith(t, Options{RuntimeAdapter: adapter})
	app := seedApplicationWithSpec(t, rt, "billing-api")

	request := v1.CreateOperationRequest{
		Kind:           v1.KindRuntimeStart,
		Resource:       app.Name,
		IdempotencyKey: "start-once",
	}
	first, created, err := rt.Service.Create(context.Background(), request)
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}
	second, created, err := rt.Service.Create(context.Background(), request)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created || second.ID != first.ID {
		t.Fatalf("相同幂等键应复用既有操作：created=%v id=%s/%s", created, first.ID, second.ID)
	}
	if count := countOperations(t, store, v1.KindRuntimeStart); count != 1 {
		t.Fatalf("want exactly one operation row, got %d", count)
	}
}

func TestRuntimeAdapterFailureIsRecorded(t *testing.T) {
	adapter := &fakeRuntimeAdapter{
		startErr: domain.NewError(v1.CodeRuntimeNotReady, "unit 处于 failed 状态"),
	}
	rt, _ := newTestRuntimeWith(t, Options{RuntimeAdapter: adapter})
	app := seedApplicationWithSpec(t, rt, "billing-api")

	op := submitRuntime(t, rt, v1.KindRuntimeStart, app.Name)
	if _, err := rt.Pool.ProcessNext(context.Background()); err != nil {
		t.Fatalf("process next: %v", err)
	}

	got, err := rt.Service.Get(context.Background(), op.ID)
	if err != nil {
		t.Fatalf("get operation: %v", err)
	}
	if got.Status != domain.StatusFailed {
		t.Fatalf("want failed, got %s", got.Status)
	}
	// 适配器的错误码必须原样保留：RUNTIME_NOT_READY 是调用方能据以行动的码。
	if got.ErrorCode != string(v1.CodeRuntimeNotReady) {
		t.Fatalf("want errorCode %s, got %q", v1.CodeRuntimeNotReady, got.ErrorCode)
	}
	// 失败原因要留在 Operation 日志里，否则排查只能靠猜。
	logs := operationLogs(t, rt, op.ID)
	found := false
	for _, entry := range logs {
		if entry.Fields["errorCode"] == string(v1.CodeRuntimeNotReady) {
			found = true
		}
	}
	if !found {
		t.Fatalf("日志里缺少 errorCode：%+v", logs)
	}
}

func TestRuntimeCancellationIsRecorded(t *testing.T) {
	adapter := &fakeRuntimeAdapter{startEntered: make(chan struct{})}
	rt, _ := newTestRuntimeWith(t, Options{RuntimeAdapter: adapter})
	app := seedApplicationWithSpec(t, rt, "billing-api")

	op := submitRuntime(t, rt, v1.KindRuntimeStart, app.Name)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := rt.Pool.ProcessNext(context.Background()); err != nil {
			t.Errorf("process next: %v", err)
		}
	}()

	// 等 Start 真的进入阻塞，再取消，避免竞态。
	select {
	case <-adapter.startEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("适配器没有被调用")
	}
	if _, err := rt.Service.Cancel(context.Background(), op.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	<-done

	got, err := rt.Service.Get(context.Background(), op.ID)
	if err != nil {
		t.Fatalf("get operation: %v", err)
	}
	if got.Status != domain.StatusCancelled {
		t.Fatalf("want cancelled, got %s", got.Status)
	}
	if got.ErrorCode != string(v1.CodeExecCancelled) {
		t.Fatalf("want errorCode %s, got %q", v1.CodeExecCancelled, got.ErrorCode)
	}
}

func TestRuntimeSyncUseCases(t *testing.T) {
	adapter := &fakeRuntimeAdapter{
		health: domain.RuntimeHealth{Ready: true, CheckedAt: time.Now().UTC(), Detail: "127.0.0.1:8080 可连接"},
	}
	reporter := &fakePrepareReporter{ok: true, decision: RuntimeDecision{
		UnitName: "billing-api.service", Tier: "legacy", SystemdVersion: 232, DecidedAt: time.Now().UTC(),
	}}
	rt, store := newTestRuntimeWith(t, Options{RuntimeAdapter: adapter, PrepareReporter: reporter})
	app := seedApplicationWithSpec(t, rt, "billing-api")

	// validate 只校验，无副作用：不得触发 prepare/start。
	validated, spec, err := rt.Runtimes.Validate(context.Background(), app.Name)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if validated.ID != app.ID || spec.Systemd.UnitName != "billing-api.service" {
		t.Fatalf("validate 返回的应用/规格不对：%+v", spec)
	}
	if calls := adapter.callNames(); strings.Join(calls, ",") != "validate" {
		t.Fatalf("validate 应只调用 Validate，got %v", calls)
	}

	// prepare 幂等可重复，并回传档位决策。
	for i := 0; i < 2; i++ {
		_, _, decision, ok, err := rt.Runtimes.Prepare(context.Background(), app.Name)
		if err != nil {
			t.Fatalf("prepare #%d: %v", i+1, err)
		}
		if !ok || decision.Tier != "legacy" || decision.SystemdVersion != 232 {
			t.Fatalf("prepare 的档位决策不对：%+v ok=%v", decision, ok)
		}
	}

	// health 把未就绪当作快照而不是错误；确定性失败才由适配器返回 RUNTIME_NOT_READY。
	_, _, health, err := rt.Runtimes.Health(context.Background(), app.Name)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !health.Ready || health.CheckedAt.IsZero() {
		t.Fatalf("unexpected health: %+v", health)
	}
	adapter.healthErr = domain.NewError(v1.CodeRuntimeNotReady, "unit 处于 failed 状态")
	if _, _, _, err := rt.Runtimes.Health(context.Background(), app.Name); domain.CodeOf(err) != v1.CodeRuntimeNotReady {
		t.Fatalf("want RUNTIME_NOT_READY, got %v", err)
	}

	// 同步用例不产生 Operation：它们不是「需要排队执行」的工作。
	if count := countOperations(t, store, v1.KindRuntimeStart); count != 0 {
		t.Fatalf("sync 用例不应建 Operation，got %d", count)
	}
}

func TestRuntimePrepareWithoutReporterOmitsDecision(t *testing.T) {
	adapter := &fakeRuntimeAdapter{}
	rt, _ := newTestRuntimeWith(t, Options{RuntimeAdapter: adapter})
	app := seedApplicationWithSpec(t, rt, "billing-api")

	_, _, _, ok, err := rt.Runtimes.Prepare(context.Background(), app.Name)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if ok {
		t.Fatal("没有 reporter 时不应声称有档位决策")
	}
}

func TestRuntimeOperationRequiresApplication(t *testing.T) {
	adapter := &fakeRuntimeAdapter{}
	rt, _ := newTestRuntimeWith(t, Options{RuntimeAdapter: adapter})

	_, _, err := rt.Service.Create(context.Background(), v1.CreateOperationRequest{
		Kind:     v1.KindRuntimeStop,
		Resource: "no-such-app",
	})
	if domain.CodeOf(err) != v1.CodeApplicationNotFound {
		t.Fatalf("want APPLICATION_NOT_FOUND, got %s (%v)", domain.CodeOf(err), err)
	}
}

// 回归：executor.command 的校验与提交路径不受 runtime 分派的影响。
func TestExecutorCommandStillRejectsInvalidSpec(t *testing.T) {
	rt, _ := newTestRuntimeWith(t, Options{RuntimeAdapter: &fakeRuntimeAdapter{}})

	_, _, err := rt.Service.Create(context.Background(), v1.CreateOperationRequest{
		Kind:     v1.KindExecutorCommand,
		Resource: "r",
		Spec:     json.RawMessage(`{"argv":["true"]}`),
	})
	if domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST, got %s (%v)", domain.CodeOf(err), err)
	}
	if !strings.Contains(domain.MessageOf(err), "absolute path") {
		t.Fatalf("错误信息应说明绝对路径要求：%s", domain.MessageOf(err))
	}
}

package application

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// ==== 迭代 4b：蓝绿部署与切流 ====
//
// 这一组用例走的是**编排**：起新槽位 → 就绪 → 切流 → 观察 → promote → 排空停旧槽位，
// 以及每一步失败时流量在哪一侧。真实的 Nginx 行为由容器 harness 覆盖（真 `nginx -t`、
// 真 reload、真 curl）；这里用假 Nginx 把它在编排里当作一个可注入成败的部件。

// fakeNginxAdapter 记录「切到过哪几侧」，并可按脚本失败。
type fakeNginxAdapter struct {
	mu       sync.Mutex
	applied  []domain.Slot
	attempts []domain.Slot
	// applyErrWhen 按槽位决定这次切流的成败（nil = 成功）。
	applyErrWhen func(domain.Slot) error
	// onApply 在成功切流之后调用一次：测试用它制造「切流之后新槽位退化了」。
	onApply func()
	// current 是 Current 的返回值（本组用例不涉及对账）。
	current domain.Slot
}

func (f *fakeNginxAdapter) Validate(_ context.Context, _ *domain.ApplicationSpec) error { return nil }

func (f *fakeNginxAdapter) CheckLoaded(_ context.Context, _ *domain.ApplicationSpec) error {
	return nil
}

func (f *fakeNginxAdapter) Apply(_ context.Context, _ *domain.ApplicationSpec, slot domain.Slot) error {
	f.mu.Lock()
	f.attempts = append(f.attempts, slot)
	f.mu.Unlock()
	if f.applyErrWhen != nil {
		if err := f.applyErrWhen(slot); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.applied = append(f.applied, slot)
	hook := f.onApply
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (f *fakeNginxAdapter) Current(_ context.Context, _ *domain.ApplicationSpec) (domain.Slot, error) {
	return f.current, nil
}

// appliedSlots 返回成功切流过的槽位序列。
func (f *fakeNginxAdapter) appliedSlots() []domain.Slot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.Slot(nil), f.applied...)
}

// ==== 夹具 ====

// blueGreenSpec 造一份蓝绿规格：两个槽位各一个端口 + 一个对外端口。
//
// 观察窗口与排空时间默认设成 0：这一组用例要的是**编排的形状**，让每次部署都白等
// 三十秒毫无意义。要测观察窗口的那条用例自己设 1 秒。
func (f *deployFixture) blueGreenSpec(artifact *domain.Artifact, version string, mutate ...func(*domain.ApplicationSpec)) *domain.ApplicationSpec {
	return f.spec(artifact, version, append([]func(*domain.ApplicationSpec){
		func(spec *domain.ApplicationSpec) {
			spec.Exec.Slots = map[domain.Slot]domain.SpecSlot{
				domain.SlotBlue: {Ports: []int{18081},
					Readiness: domain.SpecReadiness{Type: domain.ReadinessTCP, Target: "127.0.0.1:18081", ConsecutiveSuccesses: 1}},
				domain.SlotGreen: {Ports: []int{18082},
					Readiness: domain.SpecReadiness{Type: domain.ReadinessTCP, Target: "127.0.0.1:18082", ConsecutiveSuccesses: 1}},
			}
			spec.Exec.Ports = nil
			spec.Health.Readiness = domain.SpecReadiness{}
			spec.Systemd.UnitName = ""
			spec.Nginx = domain.SpecNginx{Listen: 8080, ObservationSeconds: 0, DrainSeconds: 0}
		},
	}, mutate...)...)
}

// slotRows 读回某个应用的槽位行。
func (f *deployFixture) slotRows(t *testing.T, app string) map[domain.Slot]domain.ApplicationSlot {
	t.Helper()
	application, err := f.rt.Catalogs.GetApplication(context.Background(), app)
	if err != nil {
		t.Fatalf("取应用: %v", err)
	}
	rows, err := f.store.ApplicationSlots(context.Background(), application.ID)
	if err != nil {
		t.Fatalf("ApplicationSlots: %v", err)
	}
	out := map[domain.Slot]domain.ApplicationSlot{}
	for _, row := range rows {
		out[row.Slot] = row
	}
	return out
}

func (f *deployFixture) servingSlot(t *testing.T, app string) domain.Slot {
	t.Helper()
	application, err := f.rt.Catalogs.GetApplication(context.Background(), app)
	if err != nil {
		t.Fatalf("取应用: %v", err)
	}
	slot, err := f.store.ServingSlot(context.Background(), application.ID)
	if err != nil {
		t.Fatalf("ServingSlot: %v", err)
	}
	return slot
}

// rollback 走完整的一遍「解析回滚目标 → 建 Operation → worker 执行」。
func (f *deployFixture) rollback(t *testing.T, app string) *domain.Operation {
	t.Helper()
	ctx := context.Background()
	target, err := f.rt.Deploys.PrepareRollback(ctx, app, "")
	if err != nil {
		t.Fatalf("PrepareRollback: %v", err)
	}
	raw, err := json.Marshal(struct {
		ReleaseID string `json:"releaseId"`
	}{ReleaseID: target.Release.ID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	op, _, err := f.rt.Service.Create(ctx, v1.CreateOperationRequest{
		Kind: v1.KindAppRollback, Resource: app, Spec: raw,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("worker: %v", err)
	}
	stored, err := f.rt.Service.Get(ctx, op.ID)
	if err != nil {
		t.Fatalf("get operation: %v", err)
	}
	return stored
}

// ==== 用例 ====

// 第一次部署落在 blue：没有 serving 槽位时用 blue，而不是「哪个槽位恰好有目录」。
func TestBlueGreenFirstDeployLandsOnBlue(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	artifact := f.upload(t, "orders-1.0.0", []byte("v1"))

	op := f.deploy(t, app.Name, f.blueGreenSpec(artifact, "1.0.0"))
	if op.Status != domain.StatusSucceeded {
		t.Fatalf("第一次部署应当成功，got %s (%s: %s)", op.Status, op.ErrorCode, op.ErrorMessage)
	}

	if got := f.nginx.appliedSlots(); len(got) != 1 || got[0] != domain.SlotBlue {
		t.Fatalf("第一次部署应当把流量切到 blue，got %v", got)
	}
	if slot := f.servingSlot(t, app.Name); slot != domain.SlotBlue {
		t.Fatalf("serving_slot want blue, got %q", slot)
	}
	rows := f.slotRows(t, app.Name)
	if rows[domain.SlotBlue].State != domain.SlotServing {
		t.Fatalf("blue 的状态应当 serving，got %+v", rows[domain.SlotBlue])
	}
	// 这一版记在它落的槽位上（排查「当初上到了哪一侧」要看它）。
	active := f.activeRelease(t, app.Name)
	if active.Slot != domain.SlotBlue {
		t.Fatalf("release 的 slot want blue, got %q", active.Slot)
	}
	// 单槽那条路的「先停后起」不该出现在蓝绿里：一次 Stop 都没有。
	for _, name := range f.adapter.callNames() {
		if name == "stop" {
			t.Fatalf("第一次部署不该停任何东西，调用序列：%v", f.adapter.callNames())
		}
	}
}

// 第二次部署切到 green，并把还在跑的 blue **排空之后**停掉。
func TestBlueGreenSecondDeploySwitchesAndDrains(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")

	first := f.upload(t, "orders-1.0.0", []byte("v1"))
	if op := f.deploy(t, app.Name, f.blueGreenSpec(first, "1.0.0")); op.Status != domain.StatusSucceeded {
		t.Fatalf("v1 部署应当成功: %s", op.ErrorMessage)
	}
	before := len(f.adapter.callNames())

	second := f.upload(t, "orders-2.0.0", []byte("v2"))
	if op := f.deploy(t, app.Name, f.blueGreenSpec(second, "2.0.0")); op.Status != domain.StatusSucceeded {
		t.Fatalf("v2 部署应当成功: %s", op.ErrorMessage)
	}

	if got := f.nginx.appliedSlots(); len(got) != 2 || got[0] != domain.SlotBlue || got[1] != domain.SlotGreen {
		t.Fatalf("两次切流应当是 blue → green，got %v", got)
	}
	if slot := f.servingSlot(t, app.Name); slot != domain.SlotGreen {
		t.Fatalf("serving_slot want green, got %q", slot)
	}
	rows := f.slotRows(t, app.Name)
	if rows[domain.SlotGreen].State != domain.SlotServing {
		t.Fatalf("green 应当 serving，got %+v", rows[domain.SlotGreen])
	}
	// 旧槽位排空之后被停掉——**这是蓝绿与单槽最实质的差别**：先切流，再停旧的。
	if rows[domain.SlotBlue].State != domain.SlotStopped {
		t.Fatalf("blue 排空后应当 stopped，got %+v", rows[domain.SlotBlue])
	}
	calls := f.adapter.callNames()[before:]
	if !containsString(calls, "stop") {
		t.Fatalf("换版本之后应当停掉旧槽位，调用序列：%v", calls)
	}
	// **顺序**：停旧槽位必须发生在切流之后。调用序列里最后一次 stop 之前必须已经有
	// 一次 nginx 切流——用「切流发生的时刻」间接判断：这里只能看调用序列，
	// 因此断言 stop 是这一段的**最后**一个动作（切流不在这个假适配器的调用序列里）。
	if last := calls[len(calls)-1]; last != "stop" {
		t.Fatalf("停旧槽位应当是最后一步，got %v", calls)
	}
}

// 新槽位没就绪：**流量一点没动**，报原因码并说明流量未受影响。
func TestBlueGreenNotReadyKeepsTrafficOnOldSlot(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	first := f.upload(t, "orders-1.0.0", []byte("v1"))
	if op := f.deploy(t, app.Name, f.blueGreenSpec(first, "1.0.0")); op.Status != domain.StatusSucceeded {
		t.Fatalf("v1 部署应当成功: %s", op.ErrorMessage)
	}
	f.adapter.health = domain.RuntimeHealth{Ready: false, Detail: "端口没有监听"}
	f.adapter.healthErr = domain.NewError(v1.CodeRuntimeNotReady, "unit 未就绪")

	second := f.upload(t, "orders-2.0.0", []byte("v2"))
	op := f.deploy(t, app.Name, f.blueGreenSpec(second, "2.0.0"))
	if op.Status != domain.StatusFailed {
		t.Fatalf("部署应当失败，got %s", op.Status)
	}
	if op.ErrorCode != string(v1.CodeRuntimeNotReady) {
		t.Fatalf("want RUNTIME_NOT_READY（原因码），got %s：%s", op.ErrorCode, op.ErrorMessage)
	}
	if !strings.Contains(op.ErrorMessage, "流量未受影响") {
		t.Fatalf("失败信息必须说清流量在哪一侧：%q", op.ErrorMessage)
	}
	// 最关键的一条：**切流一次都没发生过**。
	if got := f.nginx.appliedSlots(); len(got) != 1 || got[0] != domain.SlotBlue {
		t.Fatalf("新槽位没起来时不该切流，切流记录：%v", got)
	}
	if slot := f.servingSlot(t, app.Name); slot != domain.SlotBlue {
		t.Fatalf("流量应当仍在 blue，got %q", slot)
	}
	// 新槽位被收干净（停了进程），并被记成 failed。
	if rows := f.slotRows(t, app.Name); rows[domain.SlotGreen].State != domain.SlotFailed {
		t.Fatalf("green 应当被记成 failed，got %+v", rows[domain.SlotGreen])
	}
	if last := f.adapter.callNames(); last[len(last)-1] != "stop" {
		t.Fatalf("失败收尾应当停掉新槽位，调用序列：%v", last)
	}
}

// 切流本身失败（配置没通过校验）：流量未受影响，报 NGINX_CONFIG_INVALID。
func TestBlueGreenSwitchFailureKeepsTrafficOnOldSlot(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	first := f.upload(t, "orders-1.0.0", []byte("v1"))
	if op := f.deploy(t, app.Name, f.blueGreenSpec(first, "1.0.0")); op.Status != domain.StatusSucceeded {
		t.Fatalf("v1 部署应当成功: %s", op.ErrorMessage)
	}
	f.nginx.applyErrWhen = func(slot domain.Slot) error {
		if slot == domain.SlotGreen {
			return domain.NewError(v1.CodeNginxConfigInvalid, "配置没通过 nginx -t")
		}
		return nil
	}

	second := f.upload(t, "orders-2.0.0", []byte("v2"))
	op := f.deploy(t, app.Name, f.blueGreenSpec(second, "2.0.0"))
	if op.Status != domain.StatusFailed || op.ErrorCode != string(v1.CodeNginxConfigInvalid) {
		t.Fatalf("want NGINX_CONFIG_INVALID，got %s / %s：%s", op.Status, op.ErrorCode, op.ErrorMessage)
	}
	if !strings.Contains(op.ErrorMessage, "流量未受影响") {
		t.Fatalf("配置没通过校验时流量确实没动，失败信息应当这么说：%q", op.ErrorMessage)
	}
	if slot := f.servingSlot(t, app.Name); slot != domain.SlotBlue {
		t.Fatalf("流量应当仍在 blue，got %q", slot)
	}
}

// 切流环节**动过线上但结果不明**（reload 失败且换不回来）：不能说「流量未受影响」。
func TestBlueGreenReloadFailureReportsUncertainState(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	first := f.upload(t, "orders-1.0.0", []byte("v1"))
	if op := f.deploy(t, app.Name, f.blueGreenSpec(first, "1.0.0")); op.Status != domain.StatusSucceeded {
		t.Fatalf("v1 部署应当成功: %s", op.ErrorMessage)
	}
	f.nginx.applyErrWhen = func(domain.Slot) error {
		return domain.NewError(v1.CodeNginxReloadFailed, "reload 失败且换回也失败")
	}

	second := f.upload(t, "orders-2.0.0", []byte("v2"))
	op := f.deploy(t, app.Name, f.blueGreenSpec(second, "2.0.0"))
	if op.Status != domain.StatusFailed || op.ErrorCode != string(v1.CodeNginxReloadFailed) {
		t.Fatalf("want NGINX_RELOAD_FAILED，got %s / %s", op.Status, op.ErrorCode)
	}
	if strings.Contains(op.ErrorMessage, "流量未受影响") {
		t.Fatalf("切流环节失败时流量状态是**不确定**的，不该说未受影响：%q", op.ErrorMessage)
	}
	if !strings.Contains(op.ErrorMessage, "不确定") {
		t.Fatalf("失败信息必须说清「不确定，需要人工确认」：%q", op.ErrorMessage)
	}
}

// 观察窗口内新槽位退化：切回旧槽位，报 DEPLOY_ROLLED_BACK。
func TestBlueGreenObservationFailureSwitchesBack(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	first := f.upload(t, "orders-1.0.0", []byte("v1"))
	if op := f.deploy(t, app.Name, f.blueGreenSpec(first, "1.0.0", func(spec *domain.ApplicationSpec) {
		spec.Nginx.ObservationSeconds = 1
	})); op.Status != domain.StatusSucceeded {
		t.Fatalf("v1 部署应当成功: %s", op.ErrorMessage)
	}

	// 新槽位在**切流之后**才退化（切流之前它是好的，否则失败会发生在更早的一步）。
	// 而旧槽位一直健康——这才是「切回去能救回来」的场景；如果两侧都挂了，正确的结论
	// 就是「服务状态不确定」而不是「已回滚」。
	degraded := false
	f.adapter.healthWhen = func(slot domain.Slot) (domain.RuntimeHealth, error) {
		if slot == domain.SlotGreen && degraded {
			return domain.RuntimeHealth{Ready: false, Detail: "起来之后又挂了"}, nil
		}
		return domain.RuntimeHealth{Ready: true, Detail: "健康"}, nil
	}
	f.nginx.onApply = func() { degraded = true }
	second := f.upload(t, "orders-2.0.0", []byte("v2"))
	op := f.deploy(t, app.Name, f.blueGreenSpec(second, "2.0.0", func(spec *domain.ApplicationSpec) {
		spec.Nginx.ObservationSeconds = 1
	}))
	if op.Status != domain.StatusFailed {
		t.Fatalf("观察窗口内退化应当按失败处理，got %s", op.Status)
	}
	if op.ErrorCode != string(v1.CodeDeployRolledBack) {
		t.Fatalf("流量动过并撤销了 → want DEPLOY_ROLLED_BACK，got %s：%s", op.ErrorCode, op.ErrorMessage)
	}
	// 切流序列：blue（第一次部署）→ green（这一次）→ blue（切回来）。
	if got := f.nginx.appliedSlots(); len(got) != 3 || got[2] != domain.SlotBlue {
		t.Fatalf("应当把流量切回 blue，切流记录：%v", got)
	}
	if slot := f.servingSlot(t, app.Name); slot != domain.SlotBlue {
		t.Fatalf("服务侧应当仍是 blue，got %q", slot)
	}
}

// 回滚：把流量切回上一个可回滚的版本所在的槽位。
func TestBlueGreenRollbackSwitchesBackToPreviousSlot(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")

	first := f.upload(t, "orders-1.0.0", []byte("v1"))
	if op := f.deploy(t, app.Name, f.blueGreenSpec(first, "1.0.0")); op.Status != domain.StatusSucceeded {
		t.Fatalf("v1 部署应当成功: %s", op.ErrorMessage)
	}
	stable := f.activeRelease(t, app.Name)

	second := f.upload(t, "orders-2.0.0", []byte("v2"))
	if op := f.deploy(t, app.Name, f.blueGreenSpec(second, "2.0.0")); op.Status != domain.StatusSucceeded {
		t.Fatalf("v2 部署应当成功: %s", op.ErrorMessage)
	}
	if slot := f.servingSlot(t, app.Name); slot != domain.SlotGreen {
		t.Fatalf("v2 之后应当在 green 服务，got %q", slot)
	}

	op := f.rollback(t, app.Name)
	if op.Status != domain.StatusSucceeded {
		t.Fatalf("回滚应当成功，got %s (%s: %s)", op.Status, op.ErrorCode, op.ErrorMessage)
	}
	if got := f.nginx.appliedSlots(); got[len(got)-1] != domain.SlotBlue {
		t.Fatalf("回滚应当把流量切回 blue，切流记录：%v", got)
	}
	if slot := f.servingSlot(t, app.Name); slot != domain.SlotBlue {
		t.Fatalf("回滚后应当在 blue 服务，got %q", slot)
	}
	active := f.activeRelease(t, app.Name)
	if active == nil || active.ID != stable.ID {
		t.Fatalf("回滚后接流量的应当仍是 %s，got %+v", stable.ID, active)
	}
	// 回滚之后 green 被排空停掉。
	if rows := f.slotRows(t, app.Name); rows[domain.SlotGreen].State != domain.SlotStopped {
		t.Fatalf("green 回滚后应当 stopped，got %+v", rows[domain.SlotGreen])
	}
}

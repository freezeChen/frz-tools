package application

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// ==== 迭代 4c：槽位视图、时间线、对账 ====
//
// 这一组用例走两条路：先用**真实部署**（deployFixture 的假适配器 + 真 ReleaseAdapter）
// 把状态造出来，再断言 List / History / Reconcile 读出的东西。
//
// 「两份事实」是这一片的全部主题，因此每个用例都在问同一个形状的问题：
// **库里那份与线上那份一致吗？不一致时工具说的是哪一句？**

func (f *deployFixture) slotEvents(t *testing.T, app string) []domain.SlotEvent {
	t.Helper()
	application, err := f.rt.Catalogs.GetApplication(context.Background(), app)
	if err != nil {
		t.Fatalf("取应用: %v", err)
	}
	events, err := f.store.SlotEvents(context.Background(), application.ID, 0)
	if err != nil {
		t.Fatalf("SlotEvents: %v", err)
	}
	return events
}

// eventKinds 按「新的在前」返回时间线里的事件种类，便于断言整条序列。
func eventKinds(events []domain.SlotEvent) []string {
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, string(event.Slot)+"/"+string(event.Kind))
	}
	return kinds
}

// blueGreenSpecFor 造一份最小可用的蓝绿规格。List/History 只读规格、不碰制品，
// 因此不依赖真实上传（对账的用例仍然走完整部署，见下面的用例）。
func blueGreenSpecFor(application, artifactID string) *domain.ApplicationSpec {
	spec := validSpec(application)
	spec.Artifact.ID = artifactID
	spec.Exec.Ports = nil
	spec.Exec.Slots = map[domain.Slot]domain.SpecSlot{
		domain.SlotBlue: {Ports: []int{18081},
			Readiness: domain.SpecReadiness{Type: domain.ReadinessTCP, Target: "127.0.0.1:18081", ConsecutiveSuccesses: 1}},
		domain.SlotGreen: {Ports: []int{18082},
			Readiness: domain.SpecReadiness{Type: domain.ReadinessTCP, Target: "127.0.0.1:18082", ConsecutiveSuccesses: 1}},
	}
	spec.Health.Readiness = domain.SpecReadiness{}
	spec.Systemd.UnitName = ""
	spec.Nginx = domain.SpecNginx{Listen: 8080}
	return spec
}

// deployV1 把 1.0.0 部署到 blue，返回 fixture —— 几乎每条用例的第一步都是它。
func (f *deployFixture) deployV1(t *testing.T, app string) {
	t.Helper()
	artifact := f.upload(t, "orders-1.0.0", []byte("v1"))
	if op := f.deploy(t, app, f.blueGreenSpec(artifact, "1.0.0")); op.Status != domain.StatusSucceeded {
		t.Fatalf("v1 部署应当成功: %s (%s)", op.ErrorCode, op.ErrorMessage)
	}
}

// 部署之后两份事实是一致的，且两侧的信息都齐（没部署过的那一侧也有端口与 unit 名）。
func TestSlotListReportsBothFacts(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	f.deployV1(t, app.Name)

	view, err := f.rt.Slots.List(context.Background(), app.Name)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !view.OnlineKnown {
		t.Fatal("装配了 Nginx 适配器时线上事实应当是已知的")
	}
	if view.ServingSlot != domain.SlotBlue || view.RecordedSlot != domain.SlotBlue {
		t.Fatalf("两份事实都应当是 blue，got 线上 %q / 库里 %q", view.ServingSlot, view.RecordedSlot)
	}
	if view.Inconsistent {
		t.Fatalf("刚部署完不该报不一致：%s", view.Detail)
	}
	if len(view.Slots) != 2 {
		t.Fatalf("两侧都要出现（没部署过的那侧也要），got %d", len(view.Slots))
	}

	blue, green := view.Slots[0], view.Slots[1]
	if blue.Slot != domain.SlotBlue || green.Slot != domain.SlotGreen {
		t.Fatalf("槽位顺序应当是固定的 blue → green，got %s → %s", blue.Slot, green.Slot)
	}
	if !blue.Serving || blue.State != domain.SlotServing {
		t.Fatalf("blue 应当在服务，got %+v", blue)
	}
	if blue.Version != "1.0.0" || blue.ReleaseID == "" {
		t.Fatalf("blue 应当带着版本与 release id，got %+v", blue)
	}
	if strings.Join(itoaSlice(blue.Ports), ",") != "18081" {
		t.Fatalf("blue 的端口应当是它自己声明的那个，got %v", blue.Ports)
	}
	if blue.UnitName != "orders-api-blue.service" {
		t.Fatalf("unit 名是派生的，got %q", blue.UnitName)
	}
	// 进程在跑 → 探了就绪；两层事实都拿得到。
	if !blue.ProcessKnown || blue.ProcessState != domain.RuntimeActive {
		t.Fatalf("blue 的进程状态应当是 active，got %+v", blue)
	}
	if !blue.ReadyProbed || !blue.Ready {
		t.Fatalf("blue 应当在跑且就绪，got %+v", blue)
	}

	if green.Serving || green.State != "" || green.ReleaseID != "" {
		t.Fatalf("green 从没部署过，不该有任何状态，got %+v", green)
	}
	if strings.Join(itoaSlice(green.Ports), ",") != "18082" {
		t.Fatalf("没部署过的那一侧也要报出它声明的端口，got %v", green.Ports)
	}
	// **没部署过 ≠ 不健康**：进程都不在，探活没有意义，因此是「没探」而不是 false。
	if green.ReadyProbed {
		t.Fatalf("停着/没部署过的槽位不该探活，got %+v", green)
	}
	// 但进程状态照样问：inactive 是一条事实，unknown 只是「我们没问」。
	if !green.ProcessKnown || green.ProcessState != domain.RuntimeInactive {
		t.Fatalf("没部署过的那一侧也应当问出 inactive，got %+v", green)
	}
}

// 库里那份与线上不一致时：`slot list` **不写库**，而是把它标出来。
func TestSlotListFlagsLibraryDisagreeingWithNginx(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	f.deployV1(t, app.Name)

	// 模拟「切流成功、还没落库就被杀」或「有人手工改了 Nginx」：线上在 green，
	// 库里还记着 blue。
	f.nginx.setCurrent(domain.SlotGreen)

	view, err := f.rt.Slots.List(context.Background(), app.Name)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if view.ServingSlot != domain.SlotGreen || view.RecordedSlot != domain.SlotBlue {
		t.Fatalf("线上应当报 green、库里应当报 blue，got %q / %q", view.ServingSlot, view.RecordedSlot)
	}
	if !view.Inconsistent {
		t.Fatal("两份事实不一致时必须标出来")
	}
	if !strings.Contains(view.Detail, "Nginx 实际指向 green") {
		t.Fatalf("说明里要点出线上是哪一侧：%q", view.Detail)
	}
	// 只读命令不得改库：库里的 serving_slot 要原样留着（纠正交给启动对账）。
	if slot := f.servingSlot(t, app.Name); slot != domain.SlotBlue {
		t.Fatalf("slot list 不该写库，serving_slot 应当仍是 blue，got %q", slot)
	}
	// 每一行的「接流量」跟着**线上事实**走，而不是跟着库里那份。
	if !view.Slots[1].Serving || view.Slots[0].Serving {
		t.Fatalf("接流量那一列应当跟着 Nginx 走，got %+v", view.Slots)
	}
}

// 没有 Nginx 适配器（非 Linux 的部署形态）：这不是错误，但**必须说出来**——
// 否则「没有不一致」会被读成「一致」。
func TestSlotListWithoutNginxAdapterSaysOnlineUnknown(t *testing.T) {
	rt, _ := newTestRuntimeWith(t, Options{RuntimeAdapter: &fakeRuntimeAdapter{}})
	ctx := context.Background()
	app, err := rt.Catalogs.CreateApplication(ctx, "orders-api", nil)
	if err != nil {
		t.Fatalf("创建应用: %v", err)
	}
	if _, err := rt.Specs.PutSpec(ctx, app.Name, blueGreenSpecFor(app.Name, "art_0123456789"), "tester"); err != nil {
		t.Fatalf("登记规格: %v", err)
	}

	view, err := rt.Slots.List(ctx, app.Name)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if view.OnlineKnown {
		t.Fatal("没有 Nginx 适配器时线上事实不该报成已知")
	}
	if !strings.Contains(view.Detail, "Nginx") {
		t.Fatalf("说明里要点出为什么读不到：%q", view.Detail)
	}
	if view.Inconsistent {
		t.Fatal("读不到线上事实时不该声称「不一致」——那是两件不同的事")
	}
}

// 单槽应用没有槽位这回事：报 INVALID_REQUEST，而不是渲染一份两侧都空的行。
func TestSlotListRejectsSingleSlotApplication(t *testing.T) {
	f := newDeployFixture(t, nil)
	ctx := context.Background()
	app := f.app(t, "orders-api")
	artifact := f.upload(t, "orders-1.0.0", []byte("v1"))
	if _, err := f.rt.Specs.PutSpec(ctx, app.Name, f.spec(artifact, "1.0.0"), "tester"); err != nil {
		t.Fatalf("登记规格: %v", err)
	}

	if _, err := f.rt.Slots.List(ctx, app.Name); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST，got %s (%v)", domain.CodeOf(err), err)
	}
	if _, err := f.rt.Slots.History(ctx, app.Name, 0); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("history 也要拒绝，got %s (%v)", domain.CodeOf(err), err)
	}
}

// 对账的第一条规则：**serving_slot 以 Nginx 为准**。
func TestReconcilePullsServingSlotBackToNginx(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	f.deployV1(t, app.Name)

	// 线上在 green、库里还记着 blue（promote 之前被杀）。
	f.nginx.setCurrent(domain.SlotGreen)

	result, err := f.rt.Slots.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Checked != 1 {
		t.Fatalf("应当对账 1 个应用，got %d（%s）", result.Checked, result.Summary())
	}
	change := findChange(result.Changes, "serving_slot")
	if change == nil || change.From != "blue" || change.To != "green" {
		t.Fatalf("应当有一处 serving_slot blue → green，got %s", result.Summary())
	}
	if slot := f.servingSlot(t, app.Name); slot != domain.SlotGreen {
		t.Fatalf("对账后库里的 serving_slot 应当是 green，got %q", slot)
	}

	// 第二条规则也跟着生效：blue 的进程还在跑，而它已经不是接流量那一侧了。
	// 库里原本记着 serving——**任何不自洽的取值都会误导运维**，因此按事实改成 standby。
	stateChange := findChange(result.Changes, "slot_state")
	if stateChange == nil || stateChange.Slot != domain.SlotBlue || stateChange.To != "standby" {
		t.Fatalf("blue 应当被改成 standby，got %s", result.Summary())
	}
	if rows := f.slotRows(t, app.Name); rows[domain.SlotBlue].State != domain.SlotStandby {
		t.Fatalf("blue 的状态应当是 standby，got %+v", rows[domain.SlotBlue])
	}

	// 改过什么要留在时间线上：state 会被后来的对账覆盖，历史不会。
	events := f.slotEvents(t, app.Name)
	servingEvent := findEventDetail(events, "serving_slot")
	if servingEvent == nil {
		t.Fatalf("对账应当落一条说明 serving_slot 被改过的事件，got %v", eventKinds(events))
	}
	if !strings.Contains(servingEvent.Detail, "blue") || !strings.Contains(servingEvent.Detail, "green") {
		t.Fatalf("事件要按「从哪一侧改到哪一侧」说清，got %q", servingEvent.Detail)
	}
	// 这一条**没有**版本号，而且这是对的：受管配置文件里只写了槽位（迭代 4 规格 D3
	// 刻意如此，版本在库里），而 green 这一侧还没有槽位行——对账手里没有能填进去的版本。
	// 与其编一个，不如留空。下面那条（按槽位行改状态的）才拿得到版本。
	if servingEvent.Version != "" {
		t.Fatalf("线上事实里没有版本，不该凭空写一个：%+v", servingEvent)
	}
	stateEvent := findEventDetail(events, "改成 standby")
	if stateEvent == nil || stateEvent.Version != "1.0.0" {
		t.Fatalf("按槽位行改状态的那条事件应当带着版本，got %+v", stateEvent)
	}
}

// 对账的第二条规则：**进程不在跑的槽位，状态改成 stopped**。
func TestReconcileStopsSlotsWhoseProcessIsGone(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	f.deployV1(t, app.Name)
	second := f.upload(t, "orders-2.0.0", []byte("v2"))
	if op := f.deploy(t, app.Name, f.blueGreenSpec(second, "2.0.0")); op.Status != domain.StatusSucceeded {
		t.Fatalf("v2 部署应当成功: %s", op.ErrorMessage)
	}
	if rows := f.slotRows(t, app.Name); rows[domain.SlotGreen].State != domain.SlotServing {
		t.Fatalf("前置条件：green 应当在服务，got %+v", rows[domain.SlotGreen])
	}

	// 模拟一次重启之后谁都没有起来。
	f.adapter.statusWhen = func(domain.Slot) domain.RuntimeStatus { return domain.RuntimeInactive }

	result, err := f.rt.Slots.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	change := findChange(result.Changes, "slot_state")
	if change == nil || change.Slot != domain.SlotGreen || change.From != "serving" || change.To != "stopped" {
		t.Fatalf("green 应当被改成 stopped，got %s", result.Summary())
	}
	rows := f.slotRows(t, app.Name)
	if rows[domain.SlotGreen].State != domain.SlotStopped {
		t.Fatalf("green 应当是 stopped，got %+v", rows[domain.SlotGreen])
	}
	// blue 本来就是 stopped，不该被再写一遍（对账只改不一致的地方）。
	if len(result.Changes) != 1 {
		t.Fatalf("只应当有一处改动，got %s", result.Summary())
	}
	// **serving_slot 不动**：线上的 upstream 还指着 green，对账不替运维改流量。
	if slot := f.servingSlot(t, app.Name); slot != domain.SlotGreen {
		t.Fatalf("对账不该改 serving_slot，got %q", slot)
	}
}

// 没有蓝绿痕迹的应用不参与对账：线上不可能有我们写的受管配置，扫它们只是白起进程。
func TestReconcileSkipsApplicationsWithoutSlotTraces(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	artifact := f.upload(t, "orders-1.0.0", []byte("v1"))
	if _, err := f.rt.Specs.PutSpec(context.Background(), app.Name, f.blueGreenSpec(artifact, "1.0.0"), "tester"); err != nil {
		t.Fatalf("登记规格: %v", err)
	}

	result, err := f.rt.Slots.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Checked != 0 || len(result.Changes) != 0 || len(result.Skipped) != 0 {
		t.Fatalf("没部署过的应用不该被对账，got %s", result.Summary())
	}
}

// 库里有槽位痕迹、而读得到的规格都不再声明槽位：工具不猜该按哪一边算，如实跳过。
//
// 构造方式说明了这一条真正在防什么：`serving_slot` 被写过（说明这台机器上曾经有过槽位），
// 而应用现在的规格是单槽的、也没有任何 release 留下蓝绿规格——这时**没有**任何一边是
// 可以据以对账的事实，跳过是唯一诚实的做法。
func TestReconcileSkipsApplicationWhoseSpecIsNoLongerBlueGreen(t *testing.T) {
	f := newDeployFixture(t, nil)
	ctx := context.Background()
	app := f.app(t, "orders-api")
	artifact := f.upload(t, "orders-1.0.0", []byte("v1"))
	if _, err := f.rt.Specs.PutSpec(ctx, app.Name, f.spec(artifact, "1.0.0"), "tester"); err != nil {
		t.Fatalf("登记规格: %v", err)
	}
	// 库里留下的槽位痕迹：serving_slot 有值，而规格不是蓝绿。
	if err := f.store.SetServingSlot(ctx, app.ID, domain.SlotBlue, time.Now().UTC()); err != nil {
		t.Fatalf("SetServingSlot: %v", err)
	}

	result, err := f.rt.Slots.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(result.Skipped) != 1 || !strings.Contains(result.Skipped[0], "不是蓝绿形态") {
		t.Fatalf("应当跳过并说明原因，got %s", result.Summary())
	}
	if len(result.Changes) != 0 {
		t.Fatalf("跳过的应用不该被改动，got %s", result.Summary())
	}
}

// 时间线：观察窗口通过要留下「看了几眼」，退回窗口里的退化要留下失败与已采样次数。
func TestSlotHistoryRecordsSwitchAndObservation(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	artifact := f.upload(t, "orders-1.0.0", []byte("v1"))
	spec := f.blueGreenSpec(artifact, "1.0.0", func(spec *domain.ApplicationSpec) {
		spec.Nginx.ObservationSeconds = 1
	})
	if op := f.deploy(t, app.Name, spec); op.Status != domain.StatusSucceeded {
		t.Fatalf("部署应当成功: %s", op.ErrorMessage)
	}

	events := f.slotEvents(t, app.Name)
	if got := eventKinds(events); strings.Join(got, ",") != "blue/observed,blue/switched" {
		t.Fatalf("时间线应当（新的在前）是 observed → switched，got %v", got)
	}
	if events[1].ReleaseID == "" || events[1].Version != "1.0.0" {
		t.Fatalf("切流事件要点出切到哪一版，got %+v", events[1])
	}
	// 首次部署没有流量可切，因此**不该**有 stopped；观察窗口的采样次数要看得见。
	if !strings.Contains(events[0].Detail, "采样") {
		t.Fatalf("观察事件要说明采样了几次：%q", events[0].Detail)
	}
	// 事件挂在 Operation 上：`slot history` 要能回答「这次切流是哪次发布做的」。
	if events[1].OperationID == "" {
		t.Fatalf("切流事件应当带上操作 id，got %+v", events[1])
	}
}

// 目标槽位上的失败也要进时间线——**槽位状态会被对账按事实重算，历史不会**。
func TestSlotHistoryRecordsFailureOnTargetSlot(t *testing.T) {
	f := newDeployFixture(t, nil)
	ctx := context.Background()
	app := f.app(t, "orders-api")
	f.deployV1(t, app.Name)

	f.adapter.health = domain.RuntimeHealth{Ready: false, Detail: "端口没有监听"}
	f.adapter.healthErr = domain.NewError(v1.CodeRuntimeNotReady, "unit 未就绪")
	second := f.upload(t, "orders-2.0.0", []byte("v2"))
	if op := f.deploy(t, app.Name, f.blueGreenSpec(second, "2.0.0")); op.Status != domain.StatusFailed {
		t.Fatalf("前置条件：v2 应当失败，got %s", op.Status)
	}

	events := f.slotEvents(t, app.Name)
	if len(events) == 0 || events[0].Kind != domain.SlotEventFailed {
		t.Fatalf("失败应当落一条事件，got %v", eventKinds(events))
	}
	if events[0].Slot != domain.SlotGreen {
		t.Fatalf("失败的是目标槽位 green，got %s", events[0].Slot)
	}
	if !strings.Contains(events[0].Detail, string(v1.CodeRuntimeNotReady)) {
		t.Fatalf("事件说明里要有错误码（扫时间线时先看它）：%q", events[0].Detail)
	}
	// 流量没动过，因此时间线上**不该**出现 green 的 switched。
	for _, event := range events {
		if event.Slot == domain.SlotGreen && event.Kind == domain.SlotEventSwitched {
			t.Fatalf("切流没发生过，不该有 switched 事件：%v", eventKinds(events))
		}
	}
	// 失败之后库里会把它记成 failed，而时间线里那一条**不会**被对账抹掉。
	if rows := f.slotRows(t, app.Name); rows[domain.SlotGreen].State != domain.SlotFailed {
		t.Fatalf("green 应当被记成 failed，got %+v", rows[domain.SlotGreen])
	}
	f.adapter.health = domain.RuntimeHealth{Ready: true}
	f.adapter.healthErr = nil
	f.adapter.statusWhen = func(domain.Slot) domain.RuntimeStatus { return domain.RuntimeInactive }
	if _, err := f.rt.Slots.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rows := f.slotRows(t, app.Name); rows[domain.SlotGreen].State != domain.SlotStopped {
		t.Fatalf("对账应当按事实把 green 改成 stopped，got %+v", rows[domain.SlotGreen])
	}
	remaining := f.slotEvents(t, app.Name)
	if findEvent(remaining, domain.SlotEventFailed) == nil {
		t.Fatalf("对账不该抹掉失败的历史：%v", eventKinds(remaining))
	}
}

// 回滚也是一次真实的切流，时间线必须记上——否则会读成「流量一直在 blue」。
func TestSlotHistoryRecordsRollbackSwitch(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	f.deployV1(t, app.Name)
	second := f.upload(t, "orders-2.0.0", []byte("v2"))
	if op := f.deploy(t, app.Name, f.blueGreenSpec(second, "2.0.0")); op.Status != domain.StatusSucceeded {
		t.Fatalf("v2 部署应当成功: %s", op.ErrorMessage)
	}
	if op := f.rollback(t, app.Name); op.Status != domain.StatusSucceeded {
		t.Fatalf("回滚应当成功: %s", op.ErrorMessage)
	}

	events := f.slotEvents(t, app.Name)
	// 新的在前：回滚切回 blue → blue 排空停掉（green）→ ...
	// 这里只钉住最要紧的两条：回滚确实留下了一次切流，且 blue 又被记成 serving。
	switches := 0
	for _, event := range events {
		if event.Kind == domain.SlotEventSwitched {
			switches++
		}
	}
	if switches != 3 {
		t.Fatalf("应当有三次切流（v1→blue、v2→green、回滚→blue），got %d：%v", switches, eventKinds(events))
	}
	if events[0].Slot != domain.SlotGreen || events[0].Kind != domain.SlotEventStopped {
		t.Fatalf("回滚之后最新的一条应当是 green 被停掉，got %v", eventKinds(events))
	}
}

// 从未部署过的应用：时间线是空的，不是错误。
func TestSlotHistoryEmptyForFreshApplication(t *testing.T) {
	f := newDeployFixture(t, nil)
	ctx := context.Background()
	app := f.app(t, "orders-api")
	if _, err := f.rt.Specs.PutSpec(ctx, app.Name, blueGreenSpecFor(app.Name, "art_0123456789"), "tester"); err != nil {
		t.Fatalf("登记规格: %v", err)
	}

	history, err := f.rt.Slots.History(ctx, app.Name, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history.Events) != 0 {
		t.Fatalf("还没有部署过，时间线应当是空的，got %v", eventKinds(history.Events))
	}
}

func findChange(changes []SlotReconcileChange, field string) *SlotReconcileChange {
	for i := range changes {
		if changes[i].Field == field {
			return &changes[i]
		}
	}
	return nil
}

func findEvent(events []domain.SlotEvent, kind domain.SlotEventKind) *domain.SlotEvent {
	for i := range events {
		if events[i].Kind == kind {
			return &events[i]
		}
	}
	return nil
}

// findEventDetail 按说明里的关键词找事件。同一时刻可能落好几条（对账会一次改多处，
// 它们的 at 完全相同），因此**不能靠「最新那条」去断言是哪一处改动**。
func findEventDetail(events []domain.SlotEvent, needle string) *domain.SlotEvent {
	for i := range events {
		if strings.Contains(events[i].Detail, needle) {
			return &events[i]
		}
	}
	return nil
}

func itoaSlice(values []int) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, itoa(value))
	}
	return out
}

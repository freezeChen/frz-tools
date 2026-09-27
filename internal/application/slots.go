package application

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// 迭代 4c：槽位的运营视图与对账。
//
// 这里有三件事，它们共用一套「两份事实」的口径：
//
//	List     —— 把库里的运营视图与线上实际（Nginx 指向谁 + systemd 跑没跑）**一起**报出来，
//	            不一致就标出来。**它不写库**：只读命令有副作用是坏味道。
//	History  —— 读时间线。库里的 state 会被对账与后续动作覆盖，历史不会。
//	Reconcile —— 启动时把库里那份镜像纠正到线上事实（规格 D2：以 Nginx 为准），
//	            并按 systemd 的实际状态重算槽位状态。
//
// 「线上事实」只有一个来源：Nginx 的受管配置文件（`NginxAdapter.Current`）。
// 库里的 `serving_slot` 是它的镜像——**镜像会有过期的时候**，这正是要报出来、
// 要定期纠正的那件事。

// SlotStatus 是 `app slot list` 的一行：两类事实并排放在一起。
//
// 并排而不是合并，是因为**它们不一致本身就是最有用的那条信息**。
// 只报其中一个（无论报哪个）都会让「库说 A、线上是 B」这种状态彻底隐形。
type SlotStatus struct {
	Slot  domain.Slot
	State domain.SlotState
	// ReleaseID / Version 是这一侧当前跑着的版本（来自库里的槽位行）。
	ReleaseID string
	Version   string
	// Ports 是这一侧声明的端口，UnitName 是它的 unit 名（都是派生的，不手写）。
	Ports    []int
	UnitName string
	// Serving 是**线上事实**：Nginx 的 upstream 现在指着这一侧。
	Serving bool
	// ProcessState 是 systemd 报的进程状态；ProcessKnown 为 false 表示本部署没有运行时
	// 适配器（非 Linux），这一半事实读不到。
	ProcessState domain.RuntimeStatus
	ProcessKnown bool
	// Ready 是就绪探测结果。**只在进程真的在跑时才探**——停着的槽位当然不就绪，
	// 探它只会多出一条没有信息量的 false，还会让人以为「探过了、就是不健康」。
	Ready       bool
	ReadyProbed bool
	ReadyDetail string
	// SwitchedAt 是流量最近一次切到这一侧的时刻（库里的记录）。
	SwitchedAt *time.Time
	UpdatedAt  time.Time
	// Detail 是这一行的补充说明（release 在库里找不到、systemd 说不出来、探测失败……）。
	Detail string
}

// SlotView 是 `app slot list` 的整体结果。
type SlotView struct {
	Application *domain.Application
	// ServingSlot 是**线上事实**：Nginx 指向哪一侧（空 = 还没部署过）。
	ServingSlot domain.Slot
	// RecordedSlot 是库里的那份镜像。
	RecordedSlot domain.Slot
	// OnlineKnown 为 false 表示本部署读不到线上事实（没有 Nginx 适配器）：
	// 这是非 Linux 的正常形态，不是错误，但必须在输出里说清楚——否则「没有不一致」
	// 会被读成「一致」。
	OnlineKnown bool
	// Inconsistent 为真表示两份事实不一致：库需要一次对账（下次 opsd 启动会做）。
	Inconsistent bool
	// Detail 说明不一致在哪、或者为什么读不到线上事实。
	Detail string
	// Slots 固定是两侧（domain.Slots 的顺序），**没有行的那一侧也会出现**——
	// 命令的输出形状不该随「部署过几次」变化。
	Slots []SlotStatus
}

// SlotHistory 是 `app slot history` 的结果。
type SlotHistory struct {
	Application *domain.Application
	Events      []domain.SlotEvent
}

// SlotReconcileChange 是对账改掉的一处不一致。
type SlotReconcileChange struct {
	Application string
	// Field 是被改写的那一处：`serving_slot` 或 `slot_state`。
	Field string
	Slot  domain.Slot
	From  string
	To    string
}

// SlotReconcileResult 是一次对账的结果。它是**给人看与给测试断言**的：
// 「对账跑过了但什么都没改」与「对账改了三处」是完全不同的两件事。
type SlotReconcileResult struct {
	// Checked 是真正问过线上事实的应用数。
	Checked int
	// Changes 是被改写的不一致。
	Changes []SlotReconcileChange
	// Skipped 是跳过的应用（不再声明槽位、没有规格、或读线上事实失败）。
	Skipped []string
	// Errors 是读线上事实时遇到的错误（对账不因它们中断：一个应用的配置坏了，
	// 不该让别的应用也跟着不对账）。
	Errors []string
}

// SlotService 承载槽位的读用例与对账。
type SlotService struct {
	repo    Repository
	runtime RuntimeAdapter
	nginx   NginxAdapter
	now     func() time.Time
	logger  *slog.Logger
}

func newSlotService(repo Repository, runtime RuntimeAdapter, nginx NginxAdapter, now func() time.Time, logger *slog.Logger) *SlotService {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SlotService{repo: repo, runtime: runtime, nginx: nginx, now: now, logger: logger}
}

// resolve 取回应用与规格，并要求它是蓝绿形态。
//
// 「不是蓝绿」在这里是错误而不是「返回一个空视图」：单槽应用没有槽位可言，
// 渲染出一个两侧都是空的行，只会让人以为「槽位有，只是没部署过」。
func (s *SlotService) resolve(ctx context.Context, appRef string) (*domain.Application, *domain.ApplicationSpec, error) {
	app, err := s.repo.GetApplication(ctx, appRef)
	if err != nil {
		return nil, nil, err
	}
	spec, err := s.specFor(ctx, app)
	if err != nil {
		return nil, nil, err
	}
	if !spec.BlueGreen() {
		return nil, nil, domain.NewError(v1.CodeInvalidRequest,
			"应用 %s 不是蓝绿形态（manifest 里没有 exec.slots），没有槽位可看", spec.Application)
	}
	return app, spec, nil
}

// specFor 取用来回答槽位问题的规格。
//
// **优先取当前在服务的那个 release 的规格**，而不是「应用当前规格」：受管 Nginx 配置
// 就是用它渲染出来的，拿别的规格去读那份文件会读不出东西（upstream 名对不上，
// 报出来的是「受管文件读不出它指向哪个槽位」——一句把人引向错误方向的话）。
//
// 还没部署过（没有 active release、或那一版没留下规格）时退回「当前规格」：
// 那时要回答的是「这个应用**打算**怎么部署」。
func (s *SlotService) specFor(ctx context.Context, app *domain.Application) (*domain.ApplicationSpec, error) {
	// 部署只写「按 release 的规格」（1c 决定 3），因此**部署过的应用不一定有当前规格**
	// ——`GetApplicationSpec` 在这条路上会报 SPEC_NOT_FOUND，而那绝不是「这个应用没规格」。
	current, err := s.repo.ActiveRelease(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	if current != nil {
		spec, specErr := s.repo.GetApplicationSpecForRelease(ctx, app.ID, current.ID)
		if specErr == nil {
			return spec, nil
		}
		if domain.CodeOf(specErr) != v1.CodeSpecNotFound {
			return nil, specErr
		}
	}
	return s.repo.GetApplicationSpec(ctx, app.ID)
}

// List 组装 `app slot list` 的视图：库里的运营视图 + 线上事实 + 进程与就绪。
//
// 它**不写库**（见文件头）。线上事实读不出来时：
//   - 没有 Nginx 适配器 → 这是一种部署形态（非 Linux），标 OnlineKnown=false 继续；
//   - 有适配器但读不出来（受管文件被手工改成了我们不认识的形态）→ **整体报错**。
//     这时最不该做的事就是渲染一份半边空的视图，让人以为「只有一半信息」。
func (s *SlotService) List(ctx context.Context, appRef string) (*SlotView, error) {
	app, spec, err := s.resolve(ctx, appRef)
	if err != nil {
		return nil, err
	}

	recorded, err := s.repo.ServingSlot(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.ApplicationSlots(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	bySlot := make(map[domain.Slot]domain.ApplicationSlot, len(rows))
	for _, row := range rows {
		bySlot[row.Slot] = row
	}

	view := &SlotView{Application: app, RecordedSlot: recorded}
	var online domain.Slot
	if s.nginx == nil {
		view.Detail = "本部署未装配 Nginx 适配器：线上事实（upstream 指向哪一侧）读不到，下面只有库里的记录与进程状态"
	} else {
		if online, err = s.nginx.Current(ctx, spec); err != nil {
			return nil, err
		}
		view.OnlineKnown = true
		view.ServingSlot = online
		if online != recorded {
			view.Inconsistent = true
			view.Detail = fmt.Sprintf(
				"库里的 serving_slot 是 %s，而 Nginx 实际指向 %s：下次 opsd 启动对账时会按线上改库（也可以再部署一次来纠正）",
				describeSlot(recorded), describeSlot(online))
		}
	}

	for _, slot := range domain.Slots {
		status := SlotStatus{
			Slot:         slot,
			Ports:        spec.SlotPorts(slot),
			UnitName:     spec.UnitNameFor(slot),
			ProcessKnown: s.runtime != nil,
			ProcessState: domain.RuntimeUnknown,
		}
		// 没有行的那一侧：它从没被部署过。状态**留空**而不是填一个 stopped——
		// 我们从没问过它的进程（那种槽位的 unit 大概也不存在），填什么都是猜的。
		// 「没有记录」是准确的说法，空状态配上「还没部署过」的版本说明已经够清楚。
		if row, ok := bySlot[slot]; ok {
			status.State = row.State
			status.ReleaseID = row.ReleaseID
			status.SwitchedAt = row.SwitchedAt
			status.UpdatedAt = row.UpdatedAt
			if status.ReleaseID != "" {
				if release, err := s.repo.GetRelease(ctx, status.ReleaseID); err == nil {
					status.Version = release.Version
				} else if domain.CodeOf(err) != v1.CodeReleaseNotFound {
					return nil, err
				} else {
					status.Detail = "这一侧记着的 release " + status.ReleaseID + " 在库里不存在"
				}
			}
		}
		if view.OnlineKnown {
			status.Serving = online == slot
		}
		s.fillRuntime(ctx, spec, &status)
		view.Slots = append(view.Slots, status)
	}
	return view, nil
}

// fillRuntime 把 systemd 的那一半事实填进一行：进程状态，以及在进程真的在跑时探一次就绪。
//
// 两者失败都**不**让整个 list 失败：这是一条诊断命令，它在最需要的时候（主机上有东西
// 不对劲）必须还能用。失败写进这一行的 Detail，让人看得见。
//
// 「没部署过的那一侧」也照样问：unit 不存在时两个适配器都返回 inactive（而不是报错），
// 而 **inactive 比 unknown 有用得多**——它是一条事实，unknown 只是「我们没问」。
func (s *SlotService) fillRuntime(ctx context.Context, spec *domain.ApplicationSpec, status *SlotStatus) {
	if s.runtime == nil {
		return
	}
	process, err := s.runtime.Status(ctx, spec, status.Slot)
	if err != nil {
		status.Detail = joinDetail(status.Detail, "读进程状态失败："+domain.MessageOf(err))
		return
	}
	status.ProcessState = process
	if !process.Running() {
		return
	}
	health, err := s.runtime.Health(ctx, spec, status.Slot)
	if err != nil {
		status.Detail = joinDetail(status.Detail, "就绪探测失败："+domain.MessageOf(err))
		return
	}
	status.Ready = health.Ready
	status.ReadyProbed = true
	status.ReadyDetail = health.Detail
	if !health.Ready {
		status.Detail = joinDetail(status.Detail, "进程在跑但没有就绪："+health.Detail)
	}
}

// History 读时间线。
func (s *SlotService) History(ctx context.Context, appRef string, limit int) (*SlotHistory, error) {
	app, _, err := s.resolve(ctx, appRef)
	if err != nil {
		return nil, err
	}
	events, err := s.repo.SlotEvents(ctx, app.ID, limit)
	if err != nil {
		return nil, err
	}
	return &SlotHistory{Application: app, Events: events}, nil
}

// Reconcile 是启动时的对账：把库里那份镜像纠正到线上事实。
//
// 两条方向不同的规则（迭代 4 规格补充第 18.2 节）：
//
//  1. `serving_slot` ← Nginx 受管文件。线上事实在前，库跟着改。
//  2. 槽位状态 ← systemd 实际状态 + 线上 serving_slot。**按事实重算**：
//     进程不在跑 → stopped；在跑且是 serving → serving；在跑但不是 → standby。
//
// 第 2 条**刻意是重算而不是「只修一种错」**：state 是运营视图，它对「这一侧现在在做什么」
// 的回答必须与两份线上事实自洽，任何不自洽的取值都会误导运维。重算不会丢信息——
// 「上一次在这边推失败了」这件事已经在**时间线**里（`failed` 事件），那才是它该待的地方。
//
// 对账**不因单个应用的失败而中断**：一台主机上一个应用的受管配置坏了，
// 不该让别的应用也跟着不对账。
func (s *SlotService) Reconcile(ctx context.Context) (SlotReconcileResult, error) {
	result := SlotReconcileResult{}
	if s.nginx == nil && s.runtime == nil {
		// 两条事实都读不到（非 Linux 的部署形态）：没有可对账的东西。
		return result, nil
	}
	ids, err := s.repo.ApplicationsWithSlots(ctx)
	if err != nil {
		return result, err
	}

	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		s.reconcileApplication(ctx, id, &result)
	}
	return result, nil
}

// reconcileApplication 对账一个应用。所有失败都记进 result，不向外抛——
// 一个应用坏掉不该让启动流程失败，也不该让后面的应用不被对账。
func (s *SlotService) reconcileApplication(ctx context.Context, applicationID string, result *SlotReconcileResult) {
	app, err := s.repo.GetApplication(ctx, applicationID)
	if err != nil {
		result.skipped(applicationID, "取应用失败: "+domain.MessageOf(err))
		return
	}
	rows, err := s.repo.ApplicationSlots(ctx, app.ID)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("%s: 读槽位行失败（%s）", app.Name, domain.MessageOf(err)))
		return
	}
	spec, err := s.specFor(ctx, app)
	if err != nil {
		result.skipped(app.Name, "取规格失败: "+domain.MessageOf(err))
		return
	}
	if !spec.BlueGreen() {
		// 库里有槽位痕迹，而我们读得到的规格都不再声明槽位：可能是有人把 manifest 换回了
		// 单槽、或者相关的 release 已经不在了。工具不猜该按哪一边算，如实跳过
		// （受管配置还在盘上，这一点在 `app slot list` 里能看到）。
		result.skipped(app.Name, "规格不是蓝绿形态（库里有槽位痕迹）")
		return
	}
	releaseOf := func(slot domain.Slot) *domain.Release {
		for _, row := range rows {
			if row.Slot == slot && row.ReleaseID != "" {
				return s.releaseOrNil(ctx, row.ReleaseID)
			}
		}
		return nil
	}

	now := s.now()
	online := domain.Slot("")
	if s.nginx != nil {
		if online, err = s.nginx.Current(ctx, spec); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: 读线上事实失败（%s）",
				app.Name, domain.MessageOf(err)))
			return
		}
		result.Checked++

		recorded, err := s.repo.ServingSlot(ctx, app.ID)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: 读 serving_slot 失败（%s）",
				app.Name, domain.MessageOf(err)))
			return
		}
		if online != recorded {
			if err := s.repo.SetServingSlot(ctx, app.ID, online, now); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: 改写 serving_slot 失败（%s）",
					app.Name, domain.MessageOf(err)))
				return
			}
			result.Changes = append(result.Changes, SlotReconcileChange{
				Application: app.Name, Field: "serving_slot", Slot: online,
				From: string(recorded), To: string(online),
			})
			s.recordSlotEvent(ctx, app.ID, online, domain.SlotEventReconciled,
				fmt.Sprintf("对账：Nginx 实际指向 %s，库里的 serving_slot 从 %s 改成它",
					describeSlot(online), describeSlot(recorded)), releaseOf(online), nil)
		}
	}

	if s.runtime == nil {
		return
	}
	for _, row := range rows {
		want, err := s.expectedState(ctx, spec, row, online)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s/%s: 读进程状态失败（%s）",
				app.Name, row.Slot, domain.MessageOf(err)))
			continue
		}
		if want == "" || want == row.State {
			continue
		}
		if err := s.repo.PutApplicationSlot(ctx, app.ID, row.Slot, row.ReleaseID, want, nil, now); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s/%s: 改写槽位状态失败（%s）",
				app.Name, row.Slot, domain.MessageOf(err)))
			continue
		}
		result.Changes = append(result.Changes, SlotReconcileChange{
			Application: app.Name, Field: "slot_state", Slot: row.Slot,
			From: string(row.State), To: string(want),
		})
		s.recordSlotEvent(ctx, app.ID, row.Slot, domain.SlotEventReconciled,
			fmt.Sprintf("对账：按线上事实把状态从 %s 改成 %s", row.State, want),
			releaseOf(row.Slot), nil)
	}
}

// releaseOrNil 按 id 取 release，取不到时返回 nil。
//
// 时间线是**记录**：release 行不在了不该让「记一条事件」这件事失败（正相反，
// 「这一侧记着一个已经不存在的 release」本身就是要留下的线索）。
func (s *SlotService) releaseOrNil(ctx context.Context, releaseID string) *domain.Release {
	release, err := s.repo.GetRelease(ctx, releaseID)
	if err != nil {
		return nil
	}
	return release
}

// expectedState 按两份事实算出某一侧**此刻应当**是的状态。返回空串表示「推不出来，
// 别动它」。
func (s *SlotService) expectedState(
	ctx context.Context, spec *domain.ApplicationSpec, row domain.ApplicationSlot, online domain.Slot,
) (domain.SlotState, error) {
	process, err := s.runtime.Status(ctx, spec, row.Slot)
	if err != nil {
		return "", err
	}
	switch {
	case process == domain.RuntimeUnknown:
		// 问不出进程状态（systemctl 报错那类）：**不动它**。把「不知道」写成 stopped
		// 就是编一个结论。
		return "", nil
	case process == domain.RuntimeFailed || process == domain.RuntimeActivating || process == domain.RuntimeDeactivating:
		// 过渡态与失败态本身就是要报出来的事实，不覆盖。
		return "", nil
	case !process.Running():
		// 进程不在跑：无论它在库里被记成什么，此刻它都不在服务。这一条**不依赖线上事实**
		// ——它是「这一侧跑没跑」这个更基本的问题。
		return domain.SlotStopped, nil
	case s.nginx == nil:
		// 在跑，但读不到线上事实：它是在接流量还是闲着，我们不知道。猜成 standby
		// 会让运维以为流量已经切走了，而实际上我们根本不知道。
		return "", nil
	case online == row.Slot:
		// 在跑且在接流量：是不是 serving **只由线上事实回答**，不由库里那一位回答
		// ——库里那一位正是我们要纠正的东西。
		return domain.SlotServing, nil
	default:
		return domain.SlotStandby, nil
	}
}

// recordSlotEvent 落一条时间线事件。失败只记日志：**时间线是记录，不是前置条件**——
// 让它把一次成功的部署或一次对账搞失败，是本末倒置。
func (s *SlotService) recordSlotEvent(
	ctx context.Context, applicationID string, slot domain.Slot, kind domain.SlotEventKind,
	detail string, release *domain.Release /* 可以是 nil */, operationID *string,
) {
	appendSlotEvent(ctx, s.repo, s.logger, applicationID, slot, kind, detail, release, operationID, s.now())
}

// appendSlotEvent 是落事件的共同实现。部署（DeployService）与对账（SlotService）都要写
// 时间线，因此它是包级函数——两个服务各写一份的话，「失败只记日志」这条约定迟早会在
// 其中一处变成「失败就返回」。
//
// release **传对象而不是 id**：事件里要同时记 release id 与版本号，而分开传两个字符串时
// 漏掉版本号是必然会发生的事（第一版就漏了，「切到哪一版」于是成了空列）。
func appendSlotEvent(
	ctx context.Context, repo Repository, logger *slog.Logger, applicationID string, slot domain.Slot,
	kind domain.SlotEventKind, detail string, release *domain.Release, operationID *string, now time.Time,
) {
	event := &domain.SlotEvent{
		ApplicationID: applicationID, Slot: slot, Kind: kind, Detail: detail, At: now,
	}
	if release != nil {
		event.ReleaseID = release.ID
		event.Version = release.Version
	}
	if operationID != nil {
		event.OperationID = *operationID
	}
	if err := repo.AppendSlotEvent(ctx, event); err != nil {
		logger.Warn("记录槽位时间线失败", "application", applicationID, "slot", slot,
			"kind", kind, "error", err)
	}
}

// skipped 记下一个被跳过的应用。
func (r *SlotReconcileResult) skipped(application, reason string) {
	r.Skipped = append(r.Skipped, application+"："+reason)
}

// describeSlot 把空槽位说成人话：「（还没部署过）」比一个空字符串清楚。
func describeSlot(slot domain.Slot) string {
	if slot == "" {
		return "（还没部署过）"
	}
	return string(slot)
}

// joinDetail 把两条说明接起来，避免后面那条把前面那条盖掉。
func joinDetail(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "；" + add
}

// Summary 把对账结果压成一行文本，供启动日志使用——「对账跑过了但什么都没改」与
// 「对账改了三处」是完全不同的两件事，日志里必须能分辨。
func (r SlotReconcileResult) Summary() string {
	parts := []string{fmt.Sprintf("对账了 %d 个蓝绿应用", r.Checked)}
	if len(r.Changes) > 0 {
		descriptions := make([]string, 0, len(r.Changes))
		for _, change := range r.Changes {
			descriptions = append(descriptions, fmt.Sprintf("%s/%s: %s → %s（%s）",
				change.Application, change.Slot, change.From, change.To, change.Field))
		}
		parts = append(parts, "修正 "+strings.Join(descriptions, "；"))
	}
	if len(r.Skipped) > 0 {
		parts = append(parts, "跳过 "+strings.Join(r.Skipped, "；"))
	}
	if len(r.Errors) > 0 {
		parts = append(parts, "出错 "+strings.Join(r.Errors, "；"))
	}
	return strings.Join(parts, "，")
}

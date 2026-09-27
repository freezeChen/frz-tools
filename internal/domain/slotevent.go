package domain

import "time"

// 迭代 4c：槽位的时间线。
//
// 与 ApplicationSlot 的分工是**时态**：那个回答「这一侧现在什么状态」（每次动作覆盖写），
// 这个回答「这一侧发生过什么」（只追加）。`app slot history` 读的是这一份。
//
// 它**不是审计**（audit_events 才是）：审计记的是「谁在什么时候做了什么」，
// 时间线记的是「这一侧发生了什么」。两者有重叠（一次切流两处都有），但问的问题不同，
// 因此不合并——审计要能按住户/操作人过滤，时间线要能按应用/槽位最快地读出来。
type SlotEventKind string

const (
	// SlotEventSwitched 是流量切到了这一侧：upstream 已指向它、reload 成功。
	//
	// 这是时间线上最重要的一条——「什么时候切到过哪一版」问的就是它。
	SlotEventSwitched SlotEventKind = "switched"
	// SlotEventObserved 是观察窗口正常结束。detail 里带着采样次数与窗口秒数。
	SlotEventObserved SlotEventKind = "observed"
	// SlotEventFailed 是这一侧的这次尝试失败了（含观察窗口内被判退化）。
	// detail 里是错误码与原因；发生在窗口内的还带已采样次数。
	SlotEventFailed SlotEventKind = "failed"
	// SlotEventStopped 是这一侧的进程被停掉（排空之后，或失败收尾）。
	SlotEventStopped SlotEventKind = "stopped"
	// SlotEventReconciled 是对账改写了库里那份镜像（线上事实与库不一致）。
	SlotEventReconciled SlotEventKind = "reconciled"
)

// Valid 报告 kind 是不是认识的事件种类。
func (k SlotEventKind) Valid() bool {
	switch k {
	case SlotEventSwitched, SlotEventObserved, SlotEventFailed, SlotEventStopped, SlotEventReconciled:
		return true
	}
	return false
}

// SlotEvent 是时间线上的一条事件（对应 `slot_events` 表）。
type SlotEvent struct {
	ID            int64
	ApplicationID string
	Slot          Slot
	// ReleaseID / Version 是这件事发生时这一侧跑着的版本。两者都可以为空：
	// 对账认得槽位，但不一定认得版本（库里没有对应的行）。
	ReleaseID string
	Version   string
	Kind      SlotEventKind
	// Detail 是人类可读的补充（错误码与原因、采样次数……）。它刻意是自由文本而不是
	// 结构化字段：每种 kind 要记的东西都不一样，硬凑成一张表的列会让绝大多数列常年为空。
	Detail string
	// OperationID 是触发这件事的操作；对账产生的事件没有（它不由任何操作触发）。
	OperationID string
	At          time.Time
}

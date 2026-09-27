package v1

import "time"

// 迭代 4c：槽位的对外视图。

// SlotStatus 是槽位列表里的一行：**库里的运营视图与线上事实并排**。
//
// 并排而不是合并，是因为**它们不一致本身就是最有用的那条信息**：只报其中一个
// （无论报哪个）都会让「库说 A、线上是 B」这种状态彻底隐形。
type SlotStatus struct {
	Slot  string `json:"slot"`
	State string `json:"state,omitempty"`
	// ReleaseID / Version 是这一侧当前跑着的版本（来自库里的槽位行）。
	ReleaseID string `json:"releaseId,omitempty"`
	Version   string `json:"version,omitempty"`
	// Ports 与 UnitName 都是**派生**出来的，不手写。
	Ports    []int  `json:"ports,omitempty"`
	UnitName string `json:"unitName,omitempty"`
	// Serving 是线上事实：Nginx 的 upstream 现在指着这一侧。
	Serving bool `json:"serving"`
	// ProcessState 是 systemd 报的进程状态。ProcessKnown 为 false 表示本部署没有运行时
	// 适配器（非 Linux），这一半事实读不到——**不能把读不到当成 inactive**。
	ProcessState string `json:"processState,omitempty"`
	ProcessKnown bool   `json:"processKnown"`
	// Ready 为 null 表示**没探**（进程不在跑，或没有适配器）。探过但不健康才是 false。
	// 停着的槽位当然不就绪，探它只会多出一条没有信息量的 false。
	Ready       *bool  `json:"ready,omitempty"`
	ReadyDetail string `json:"readyDetail,omitempty"`
	// SwitchedAt 是流量最近一次切到这一侧的时刻（不是部署时刻）。
	SwitchedAt *time.Time `json:"switchedAt,omitempty"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
	// Detail 是这一行的补充说明（release 在库里找不到、读进程状态失败、就绪探测失败……）。
	Detail string `json:"detail,omitempty"`
}

// SlotListResponse 是 `GET /applications/{id}/slots` 的响应。
type SlotListResponse struct {
	APIVersion  string `json:"apiVersion"`
	Application string `json:"application"`
	// ServingSlot 是**线上事实**（Nginx 指向哪一侧），RecordedSlot 是库里的镜像。
	ServingSlot  string `json:"servingSlot,omitempty"`
	RecordedSlot string `json:"recordedSlot,omitempty"`
	// OnlineKnown 为 false 表示本部署读不到线上事实（没有 Nginx 适配器）：这是非 Linux
	// 的正常形态，但必须显式说出来——否则「没有不一致」会被读成「一致」。
	OnlineKnown bool `json:"onlineKnown"`
	// Inconsistent 为真表示两份事实不一致（下次 opsd 启动对账会按线上改库）。
	Inconsistent bool         `json:"inconsistent"`
	Detail       string       `json:"detail,omitempty"`
	Items        []SlotStatus `json:"items"`
}

// SlotEvent 是时间线上的一条事件。
type SlotEvent struct {
	ID        int64  `json:"id"`
	Slot      string `json:"slot"`
	ReleaseID string `json:"releaseId,omitempty"`
	Version   string `json:"version,omitempty"`
	// Kind 取值见 domain.SlotEventKind：switched / observed / failed / stopped / reconciled。
	Kind string `json:"kind"`
	// Detail 是自由文本补充（错误码与原因、采样次数……）。
	Detail string `json:"detail,omitempty"`
	// OperationID 是触发这件事的操作；对账产生的事件没有（它不由任何操作触发）。
	OperationID string    `json:"operationId,omitempty"`
	At          time.Time `json:"at"`
}

// SlotHistoryResponse 是 `GET /applications/{id}/slots/history` 的响应。
type SlotHistoryResponse struct {
	APIVersion  string      `json:"apiVersion"`
	Application string      `json:"application"`
	Items       []SlotEvent `json:"items"`
}

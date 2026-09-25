package v1

import "time"

// RuntimeDecision 是适配器在 Prepare 期间做出的、与「这份规格在这台主机上会变成什么」
// 有关的决策记录：所选 unit 档位、探测到的 systemd 版本与降级说明。
//
// 规格要求它与 unit 的注释一样可追溯：同一份 manifest 在 systemd 255 与 219 上会生成
// 不同的 unit，没有这份记录就无从解释「为什么这台机器上的 unit 长得不一样」。
// tier/systemdVersion/degradations 是 systemd 适配器的诊断维度；将来若出现第二个
// 真实适配器，这里应退化成自由键值而不是继续堆字段。
type RuntimeDecision struct {
	UnitName       string    `json:"unitName"`
	UnitPath       string    `json:"unitPath"`
	Tier           string    `json:"tier,omitempty"`
	SystemdVersion int       `json:"systemdVersion,omitempty"`
	Degradations   []string  `json:"degradations,omitempty"`
	DecidedAt      time.Time `json:"decidedAt"`
}

// RuntimeActionRequest 是 start/stop 的可选请求体。dryRun 刻意显式列出而不是靠
// 未知字段报错：适配器端口没有 dry-run 语义，接受它就会变成「以为只是预演、其实
// 真的启停了进程」。要做无副作用的检查请用 runtime/validate。
type RuntimeActionRequest struct {
	// Slot 指定操作打在哪个槽位（迭代 4 的蓝绿）。**单槽应用必须留空，蓝绿应用必须给**——
	// 工具不去猜「他大概想操作哪一侧」：猜错的表现是操作了另一侧而看起来成功了。
	Slot           string `json:"slot,omitempty"`
	DryRun         bool   `json:"dryRun"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	CreatedBy      string `json:"createdBy,omitempty"`
	// Retry 省略即不自动重试。1d 的决策 3 允许 runtime.* 声明重试，但语义有额外收口：
	// 只有「就绪从未通过」才算失败、才可重试；已就绪过再崩溃归 unit 的 Restart=
	// 与健康检查，不触发操作级重试（迭代 1d 规格 D6）。
	Retry *RetrySpec `json:"retry,omitempty"`
}

// RuntimeValidateResponse 只表示「这份规格能被本机适配器执行」，不含任何副作用，
// 也不代表进程已经在跑。
type RuntimeValidateResponse struct {
	APIVersion  string `json:"apiVersion"`
	Application string `json:"application"`
	Valid       bool   `json:"valid"`
}

// RuntimePrepareResponse 在成功时带上适配器做出的决策；决策未知时 decision 缺省，
// 而不是给一个看起来像真的的空档位。
type RuntimePrepareResponse struct {
	APIVersion  string           `json:"apiVersion"`
	Application string           `json:"application"`
	Prepared    bool             `json:"prepared"`
	Decision    *RuntimeDecision `json:"decision,omitempty"`
}

// RuntimeHealthResponse 是「能否接流量」的快照。未就绪不是错误码而是 ready=false：
// 调用方靠它轮询。确定性失败（unit failed、启动超时）才返回 RUNTIME_NOT_READY 错误。
type RuntimeHealthResponse struct {
	APIVersion  string    `json:"apiVersion"`
	Application string    `json:"application"`
	Ready       bool      `json:"ready"`
	CheckedAt   time.Time `json:"checkedAt"`
	Detail      string    `json:"detail,omitempty"`
}

// RuntimeOpSpec 是 runtime.* 操作随 Operation 存下来的参数。
//
// 它**只**承载槽位：应用的规格仍然在执行时现读（与 spec put 的语义一致，因此重试
// runtime.start 用的是最新 manifest）。槽位必须存下来，是因为它**不是规格的一部分**
// ——规格对两个槽位是同一份（端口与就绪目标由槽位自己声明），执行时从规格里推不出来。
type RuntimeOpSpec struct {
	Slot string `json:"slot,omitempty"`
}

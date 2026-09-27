package v1

// DeployRequest 部署一个版本（迭代 3）。
//
// manifest 以**原文**传送而不是结构化字段，与 `PUT /applications/{id}/spec` 同样的理由：
// 严格解码（KnownFields）与迁移链是唯一能挡住「未知字段被静默丢弃」的入口。区别在于部署的
// manifest 是**这一版要用的那份**，它会随 release 一起存下来——回滚要连配置一起回滚。
type DeployRequest struct {
	Manifest       string     `json:"manifest"`
	IdempotencyKey string     `json:"idempotencyKey,omitempty"`
	CreatedBy      string     `json:"createdBy,omitempty"`
	Retry          *RetrySpec `json:"retry,omitempty"`
}

// RollbackRequest 回滚到某个既有版本。To 为空表示「上一个曾经激活过的版本」。
type RollbackRequest struct {
	To             string     `json:"to,omitempty"`
	IdempotencyKey string     `json:"idempotencyKey,omitempty"`
	CreatedBy      string     `json:"createdBy,omitempty"`
	Retry          *RetrySpec `json:"retry,omitempty"`
}

// DeployResponse 是部署与回滚的统一响应。
//
// 两种结果：
//
//   - 产生了 Operation（HTTP 202）：部署在异步进行，用 operation id 查进度；
//   - **没有产生 Operation**（HTTP 200，Noop=true）：要上的那个版本已经是当前版本，
//     或者回滚的目标就是当前版本。重复提交一次部署不该有任何副作用，而"重新物化一遍"
//     既做不到（目录已存在会被拒）也没有意义。
type DeployResponse struct {
	APIVersion string     `json:"apiVersion"`
	Noop       bool       `json:"noop,omitempty"`
	Release    Release    `json:"release"`
	Operation  *Operation `json:"operation,omitempty"`

	// Slot 是**这一次发布会落到哪一侧**（蓝绿应用才有值）。它与 Release.Slot 不是一回事：
	// 那个是「这一版当初上到了哪一侧」（历史），这个是「这一次会上到哪一侧」（按提交时刻
	// 的线上状态推算，迭代 4c 规格 §18.3）。单槽应用两个都为空。
	Slot string `json:"slot,omitempty"`
	// Switching 表示这次发布**包含一次切流**。第一次部署没有流量可切，因此是 false
	// ——这是调用方区分「首次上线」与「换版本」的最小信息。
	//
	// 它说的是「这次发布有没有切流这个动作」，**不是**「流量已经切过去了」：切流是异步的，
	// 结果要看 Operation。
	Switching bool `json:"switching,omitempty"`
}

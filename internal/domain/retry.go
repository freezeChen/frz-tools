package domain

import (
	"encoding/json"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// 退避的默认值与上限。这些取值已在迭代 1d 规格第 12 节「决定 1」中冻结，
// 改动必须先改规格。
const (
	// DefaultRetryMaxAttempts 是省略 maxAttempts 时的取值。1 表示不重试——
	// 与「自动重试默认关闭」一致：给出 retry 段却没有说明尝试几次，不构成重试的意图。
	DefaultRetryMaxAttempts = 1
	// DefaultRetryBaseDelay 是退避的基数：第一次失败后大约等这么久。
	DefaultRetryBaseDelay = 5 * time.Second
	// DefaultRetryMaxDelay 是退避的绝对上限，避免一条链占着一整天。
	DefaultRetryMaxDelay = 5 * time.Minute
	// MaxRetryAttempts 是尝试次数的上限。再大没有意义，且会让退避窗口失控。
	MaxRetryAttempts = 10
	// retryJitterPercent 是抖动比例：实际延迟落在 base 的 ±20% 内。
	// 抖动是为了避免多个同一时刻失败的操作在下一个时刻再次同时涌上来。
	retryJitterPercent = 20
)

// RetryPolicyFromSpec 把提交上来的策略原文转成值对象，省略的字段取默认值。
//
// 它同时是**读回持久化策略**的入口：库里存的是提交原文，读回时走同一个函数，
// 因此「写入时按什么解释」与「读回时按什么解释」不会漂移。
func RetryPolicyFromSpec(spec v1.RetrySpec) RetryPolicy {
	policy := RetryPolicy{
		MaxAttempts: spec.MaxAttempts,
		BaseDelay:   time.Duration(spec.BaseDelaySeconds) * time.Second,
		MaxDelay:    time.Duration(spec.MaxDelaySeconds) * time.Second,
	}
	if policy.MaxAttempts == 0 {
		policy.MaxAttempts = DefaultRetryMaxAttempts
	}
	if policy.BaseDelay == 0 {
		policy.BaseDelay = DefaultRetryBaseDelay
	}
	if policy.MaxDelay == 0 {
		policy.MaxDelay = DefaultRetryMaxDelay
	}
	for _, raw := range spec.RetryableErrorCodes {
		policy.RetryableCodes = append(policy.RetryableCodes, v1.ErrorCode(raw))
	}
	return policy
}

// ParseRetryPolicy 从持久化的策略原文解析出值对象。
//
// 解析失败时**按不重试处理**（返回零值策略与错误），调用方据此保守地什么都不做：
// 宁可少重试一次，也不能因为策略读不出来就乱重试。
func ParseRetryPolicy(raw json.RawMessage) (RetryPolicy, error) {
	if len(raw) == 0 {
		return RetryPolicy{}, nil
	}
	var spec v1.RetrySpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return RetryPolicy{}, err
	}
	return RetryPolicyFromSpec(spec), nil
}

// defaultRetryableCodes 是可重试错误码的默认白名单（规格 D3）。
//
// 这是一份**白名单**而不是黑名单：新增错误码默认不可重试，要重试必须在这里登记。
// 理由是不可重试的代价（重复执行副作用、对着权限问题反复重试）远高于少重试一次。
func defaultRetryableCodes() []v1.ErrorCode {
	return []v1.ErrorCode{
		v1.CodeExecTimeout,     // 瞬时慢，重试有意义
		v1.CodeExecExitNonZero, // 命令自身偶发失败（资源竞争、临时文件冲突）
		v1.CodeRuntimeNotReady, // 预热没完成，正是「等一等再试」的典型
		v1.CodeDaemonRestarted, // 被上一个守护进程中断，与命令本身无关
	}
}

// neverRetryableCodes 是**任何情况下都不得重试**的错误码。用户在请求里可以缩小
// 白名单，但不得把这些码加进来——「取消也要重试」这类意图不该由调用方表达。
func neverRetryableCodes() map[v1.ErrorCode]bool {
	return map[v1.ErrorCode]bool{
		v1.CodeExecCancelled:    true, // 用户主动取消，重试等于违抗指令
		v1.CodePermissionDenied: true, // 权限不会因为重试而改变
		v1.CodeLockBusy:         true, // 锁冲突在提交期就返回了，不是执行期失败
		v1.CodeSecretUnresolved: true, // 凭据缺失不会自愈
		v1.CodeInternal:         true, // 未知原因，宁可让运维看见
		v1.CodeConfigInvalid:    true,
		v1.CodeInvalidRequest:   true,
		v1.CodeManifestInvalid:  true,
	}
}

// RetryPolicy 是一次操作的重试策略。零值（MaxAttempts = 0）表示「不重试」。
type RetryPolicy struct {
	// MaxAttempts 含首次尝试：1 表示不重试，3 表示最多执行三次。
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	// RetryableCodes 为空表示用默认白名单；非空表示在默认白名单之上**取交集**
	// （即只能缩小，不能扩大——见 Validate）。
	RetryableCodes []v1.ErrorCode
}

// Enabled 报告该策略是否会产生自动重试。
func (p RetryPolicy) Enabled() bool { return p.MaxAttempts > 1 }

// Validate 校验策略。非法策略在**提交时**就要失败，不等到执行失败之后才发现配置写错了。
func (p RetryPolicy) Validate() error {
	if p.MaxAttempts < 1 {
		return NewError(v1.CodeRetryPolicyInvalid, "retry.maxAttempts 至少为 1（1 表示不重试），got %d", p.MaxAttempts)
	}
	if p.MaxAttempts > MaxRetryAttempts {
		return NewError(v1.CodeRetryPolicyInvalid, "retry.maxAttempts 最多为 %d，got %d", MaxRetryAttempts, p.MaxAttempts)
	}
	if p.BaseDelay < 0 {
		return NewError(v1.CodeRetryPolicyInvalid, "retry.baseDelay 不能为负数")
	}
	if p.MaxDelay < 0 {
		return NewError(v1.CodeRetryPolicyInvalid, "retry.maxDelay 不能为负数")
	}
	if p.MaxDelay > 0 && p.BaseDelay > p.MaxDelay {
		return NewError(v1.CodeRetryPolicyInvalid,
			"retry.baseDelay（%s）不能大于 retry.maxDelay（%s）", p.BaseDelay, p.MaxDelay)
	}
	for _, code := range p.RetryableCodes {
		if neverRetryableCodes()[code] {
			return NewError(v1.CodeRetryPolicyInvalid,
				"错误码 %s 在任何情况下都不可重试，不得加入 retryableErrorCodes", code)
		}
		if !p.defaultRetryable(code) {
			return NewError(v1.CodeRetryPolicyInvalid,
				"错误码 %s 不在默认可重试白名单内，retryableErrorCodes 只能从中取子集", code)
		}
	}
	return nil
}

// defaultRetryable 判断错误码是否在**默认**白名单内。
func (p RetryPolicy) defaultRetryable(code v1.ErrorCode) bool {
	for _, c := range defaultRetryableCodes() {
		if c == code {
			return true
		}
	}
	return false
}

// Retryable 判断某个错误码在当前策略下是否应当重试。
//
// 判据是「默认白名单 ∩ 请求给出的子集」。请求**只能缩小**范围：把默认白名单里的码
// 从 RetryableCodes 里去掉即表示不重试它，但列出一个不在默认白名单里的码是非法的
// （Validate 会拒绝）。
func (p RetryPolicy) Retryable(code v1.ErrorCode) bool {
	if !p.Enabled() {
		return false
	}
	if neverRetryableCodes()[code] {
		return false
	}
	if !p.defaultRetryable(code) {
		return false
	}
	if len(p.RetryableCodes) == 0 {
		return true
	}
	for _, c := range p.RetryableCodes {
		if c == code {
			return true
		}
	}
	return false
}

// NextDelay 返回第 attempt 次尝试失败后应当等待多久。
//
// attempt 从 1 开始（第 1 次尝试失败后进入第 2 次）。延迟按 base * 2^(attempt-1)
// 指数增长，被 MaxDelay 截断，再叠加 ±retryJitterPercent 的抖动。
//
// 抖动由 jitter 注入而不在这里取随机数：调用方传 [0,1) 的均匀随机值，
// 这样退避序列在测试里是可断言的确定值。**抖动必须在写入时算一次并存进
// not_before**，不能在读取时重算——否则守护进程重启后同一个操作会算出不同的时间。
func (p RetryPolicy) NextDelay(attempt int, jitter float64) time.Duration {
	base := p.BaseDelay
	if base <= 0 {
		base = DefaultRetryBaseDelay
	}
	maxDelay := p.MaxDelay
	if maxDelay <= 0 {
		maxDelay = DefaultRetryMaxDelay
	}
	if attempt < 1 {
		attempt = 1
	}

	delay := base
	for i := 1; i < attempt; i++ {
		delay *= 2
		// 指数增长可能在到达上限前就溢出；一旦超过上限就没必要继续乘。
		if delay >= maxDelay {
			delay = maxDelay
			break
		}
	}
	if delay > maxDelay {
		delay = maxDelay
	}

	if jitter < 0 {
		jitter = 0
	}
	if jitter > 1 {
		jitter = 1
	}
	factor := 1 + (jitter*2-1)*retryJitterPercent/100
	delay = time.Duration(float64(delay) * factor)

	// MaxDelay 是**绝对上限**：抖动之后仍然不得超过它。
	if delay > maxDelay {
		delay = maxDelay
	}
	if delay < 0 {
		delay = 0
	}
	return delay
}

// RetryPlan 是一次「排下一次尝试」的完整决定。
//
// 它作为 FinishInput 的一部分提交，使「把当前操作标记为失败」与「创建下一次尝试」
// 落在**同一个事务**里。分开两步做的话，两步之间崩溃会静默丢掉这次重试——
// 失败的操作还留在库里，看起来只是「没重试」，没有任何迹象表明少做了一步。
type RetryPlan struct {
	OperationID     string
	NotBefore       time.Time
	RetryPolicyJSON []byte
}

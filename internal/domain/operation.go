package domain

import (
	"encoding/json"
	"time"
)

type Status string

const (
	StatusPending    Status = "pending"
	StatusRunning    Status = "running"
	StatusSucceeded  Status = "succeeded"
	StatusFailed     Status = "failed"
	StatusCancelled  Status = "cancelled"
	StatusRolledBack Status = "rolled_back"
)

const (
	PhaseValidate = "validate"
	// PhasePrepare 是运行时准备阶段：它只出现在 runtime.* 操作里，
	// 用来把「建用户/目录/环境文件/unit」与「启停进程」在日志里分开。
	PhasePrepare  = "prepare"
	PhaseExecute  = "execute"
	PhaseFinalize = "finalize"
)

func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusRolledBack:
		return true
	}
	return false
}

func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusRunning, StatusSucceeded, StatusFailed, StatusCancelled, StatusRolledBack:
		return true
	}
	return false
}

var allowedTransitions = map[Status][]Status{
	StatusPending: {StatusRunning, StatusCancelled},
	StatusRunning: {StatusSucceeded, StatusFailed, StatusCancelled},
}

func CanTransition(from, to Status) bool {
	for _, candidate := range allowedTransitions[from] {
		if candidate == to {
			return true
		}
	}
	return false
}

func CanRetry(s Status) bool {
	return s == StatusFailed || s == StatusCancelled
}

type Operation struct {
	ID             string
	Kind           string
	Resource       string
	Status         Status
	Phase          string
	DryRun         bool
	IdempotencyKey string
	RequestHash    string
	RetryOf        string
	ExitCode       *int
	ErrorCode      string
	ErrorMessage   string
	Spec           json.RawMessage
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	CreatedBy      string

	// Attempt 是这条重试链上的第几次尝试，从 1 开始。未声明重试的操作恒为 1。
	Attempt int
	// NotBefore 是退避窗口的终点：早于它的 pending 操作不被领取。nil 表示立即可领取。
	NotBefore *time.Time
	// RetryPolicy 是这条链的重试策略；零值表示不重试。
	RetryPolicy RetryPolicy
	// RetryPolicyJSON 是提交时的策略原文，用于持久化与事后审计。
	// 它只在「当时到底按什么策略重试的」这件事上有意义，因此不与字段重复存储。
	RetryPolicyJSON json.RawMessage
}

// RetryExhausted 报告该操作是否是「声明了重试、已失败、且到达尝试上限」的最后一跳。
// 调用方据此区分「失败了，但还会再试」与「失败了，不会再有下一次」。
func (o *Operation) RetryExhausted() bool {
	return o.Status == StatusFailed && o.RetryPolicy.Enabled() && o.Attempt >= o.RetryPolicy.MaxAttempts
}

type CommandSpec struct {
	Argv                []string
	WorkingDirectory    string
	Environment         map[string]string
	SecretEnv           map[string]SecretRef
	Timeout             time.Duration
	MaxOutputBytes      int64
	SensitiveEnvKeys    []string
	SensitiveArgIndexes []int
	DryRun              bool
}

type Result struct {
	ExitCode   int
	Stdout     string
	Stderr     string
	Truncated  bool
	DurationMS int64
	Executed   bool
}

type CreateResult struct {
	Operation *Operation
	Created   bool
}

type FinishInput struct {
	OperationID  string
	Status       Status
	ExitCode     *int
	ErrorCode    string
	ErrorMessage string
	// Retry 非 nil 时，仓储必须在**同一个事务**里创建下一次尝试（D4）。
	// 分成两步做的话，两步之间崩溃会静默丢掉这次重试。
	Retry *RetryPlan
}

package v1

import (
	"encoding/json"
	"time"
)

const APIVersion = "ops.frz.io/v1alpha1"

const (
	KindExecutorCommand = "executor.command"

	// runtime.start / runtime.stop 走 Operation：它们复用同一套锁、状态机、审计、
	// 日志、取消与重试，因此「启动一个应用」在查询与排查上和「跑一条命令」完全一致。
	KindRuntimeStart = "runtime.start"
	KindRuntimeStop  = "runtime.stop"

	// 备份类操作。它们与 runtime.* 走同一条创建路径，因此锁、幂等键、请求摘要、
	// 审计、日志、取消、重试与查询全部复用既有机制，没有第二条旁路。
	KindBackupRun     = "backup.run"
	KindBackupVerify  = "backup.verify"
	KindBackupRestore = "backup.restore"

	// 部署类操作（迭代 3）。它们与 runtime.* 共用同一把应用级锁，因此同一应用上的
	// 「部署」与「启停」天然互斥——那是同一个应用上的两件事，同时做会互相踩。
	KindAppDeploy   = "app.deploy"
	KindAppRollback = "app.rollback"
)

type CreateOperationRequest struct {
	Kind           string          `json:"kind"`
	Resource       string          `json:"resource"`
	DryRun         bool            `json:"dryRun"`
	Spec           json.RawMessage `json:"spec,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	CreatedBy      string          `json:"createdBy,omitempty"`
	// Retry 省略即不重试（1d 规格 D2）。执行器跑的是任意 argv，默认重试等于
	// 默认重复执行副作用，所以自动重试必须由调用方显式声明。
	Retry *RetrySpec `json:"retry,omitempty"`
}

// RetrySpec 是提交时声明的重试策略。字段省略时取服务端默认值（base 5s、maxDelay 5m）。
type RetrySpec struct {
	MaxAttempts         int      `json:"maxAttempts,omitempty"`
	BaseDelaySeconds    int      `json:"baseDelaySeconds,omitempty"`
	MaxDelaySeconds     int      `json:"maxDelaySeconds,omitempty"`
	RetryableErrorCodes []string `json:"retryableErrorCodes,omitempty"`
}

type ExecutorCommandSpec struct {
	Argv                []string             `json:"argv"`
	WorkingDirectory    string               `json:"workingDirectory,omitempty"`
	Environment         map[string]string    `json:"environment,omitempty"`
	SecretEnvironment   map[string]SecretRef `json:"secretEnvironment,omitempty"`
	TimeoutSeconds      int                  `json:"timeoutSeconds,omitempty"`
	MaxOutputBytes      int64                `json:"maxOutputBytes,omitempty"`
	SensitiveEnvKeys    []string             `json:"sensitiveEnvKeys,omitempty"`
	SensitiveArgIndexes []int                `json:"sensitiveArgIndexes,omitempty"`
}

// SecretRef 只描述凭据的来源。明文在使用时刻解析，不进入请求体、数据库或日志。
type SecretRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type Operation struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	Resource       string          `json:"resource"`
	Status         string          `json:"status"`
	Phase          string          `json:"phase"`
	DryRun         bool            `json:"dryRun"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	RequestHash    string          `json:"requestHash,omitempty"`
	RetryOf        string          `json:"retryOf,omitempty"`
	ExitCode       *int            `json:"exitCode,omitempty"`
	ErrorCode      string          `json:"errorCode,omitempty"`
	ErrorMessage   string          `json:"errorMessage,omitempty"`
	Spec           json.RawMessage `json:"spec,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
	StartedAt      *time.Time      `json:"startedAt,omitempty"`
	FinishedAt     *time.Time      `json:"finishedAt,omitempty"`
	CreatedBy      string          `json:"createdBy,omitempty"`

	// 重试链上的位置。Attempt 从 1 开始；未声明重试的操作 attempt=1、maxAttempts=1。
	Attempt       int        `json:"attempt"`
	MaxAttempts   int        `json:"maxAttempts"`
	NextAttemptAt *time.Time `json:"nextAttemptAt,omitempty"`
	// RetryExhausted 只在「声明了重试、已失败、且到达上限」时为 true，
	// 让调用方不必自己数链就知道不会再有下一次。
	RetryExhausted bool `json:"retryExhausted,omitempty"`
}

type OperationResponse struct {
	APIVersion string    `json:"apiVersion"`
	Operation  Operation `json:"operation"`
}

type LogEntry struct {
	ID      int64             `json:"id"`
	Level   string            `json:"level"`
	Time    time.Time         `json:"time"`
	Phase   string            `json:"phase,omitempty"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

type LogsResponse struct {
	APIVersion string     `json:"apiVersion"`
	Operation  string     `json:"operation"`
	Items      []LogEntry `json:"items"`
	NextCursor int64      `json:"nextCursor,omitempty"`
}

type HealthResponse struct {
	APIVersion string `json:"apiVersion"`
	Daemon     string `json:"daemon"`
	Database   string `json:"database"`
	Workers    int    `json:"workers"`
	UptimeMS   int64  `json:"uptimeMs"`
}

type ErrorResponse struct {
	APIVersion string         `json:"apiVersion"`
	Code       ErrorCode      `json:"code"`
	Message    string         `json:"message"`
	Details    map[string]any `json:"details,omitempty"`
}

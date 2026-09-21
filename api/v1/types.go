package v1

import (
	"encoding/json"
	"time"
)

const APIVersion = "ops.frz.io/v1alpha1"

const (
	KindExecutorCommand = "executor.command"
)

type CreateOperationRequest struct {
	Kind           string          `json:"kind"`
	Resource       string          `json:"resource"`
	DryRun         bool            `json:"dryRun"`
	Spec           json.RawMessage `json:"spec,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	CreatedBy      string          `json:"createdBy,omitempty"`
}

type ExecutorCommandSpec struct {
	Argv                []string          `json:"argv"`
	WorkingDirectory    string            `json:"workingDirectory,omitempty"`
	Environment         map[string]string `json:"environment,omitempty"`
	TimeoutSeconds      int               `json:"timeoutSeconds,omitempty"`
	MaxOutputBytes      int64             `json:"maxOutputBytes,omitempty"`
	SensitiveEnvKeys    []string          `json:"sensitiveEnvKeys,omitempty"`
	SensitiveArgIndexes []int             `json:"sensitiveArgIndexes,omitempty"`
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

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
}

type CommandSpec struct {
	Argv                []string
	WorkingDirectory    string
	Environment         map[string]string
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
}

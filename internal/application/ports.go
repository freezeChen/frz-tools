package application

import (
	"context"
	"time"

	"frz-tools/internal/domain"
)

// Repository 是持久化端口，由 SQLite 适配器实现。
type Repository interface {
	CreateOperation(ctx context.Context, op *domain.Operation) (*domain.CreateResult, error)
	GetOperation(ctx context.Context, id string) (*domain.Operation, error)
	ClaimNextPending(ctx context.Context, now time.Time) (*domain.Operation, error)
	Finish(ctx context.Context, in domain.FinishInput, now time.Time) (*domain.Operation, error)
	CancelPending(ctx context.Context, id string, now time.Time) (*domain.Operation, bool, error)
	AppendLog(ctx context.Context, entry domain.LogEntry) error
	AppendAudit(ctx context.Context, event domain.AuditEvent) error
	ListLogs(ctx context.Context, operationID string, afterID int64, limit int) ([]domain.LogEntry, error)
	RecoverRunning(ctx context.Context, now time.Time) ([]domain.Operation, error)
	ActiveLock(ctx context.Context, resource string) (*domain.ResourceLock, error)
	CountByStatus(ctx context.Context, status domain.Status) (int, error)
}

// Executor 是进程执行端口，由本机执行器适配器实现。
type Executor interface {
	Run(ctx context.Context, spec domain.CommandSpec) (domain.Result, error)
}

type Defaults struct {
	Timeout          time.Duration
	MaxOutputBytes   int64
	SensitiveEnvKeys []string
}

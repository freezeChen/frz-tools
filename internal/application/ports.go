package application

import (
	"context"
	"io"
	"time"

	"github.com/freezeChen/frz-tools/internal/domain"
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

	CreateArtifact(ctx context.Context, artifact *domain.Artifact) (*domain.Artifact, bool, error)
	GetArtifact(ctx context.Context, id string) (*domain.Artifact, error)
	ListArtifacts(ctx context.Context, cursor string, limit int) ([]domain.Artifact, error)
	SoftDeleteArtifact(ctx context.Context, id string, now time.Time) error
	CountArtifactReferences(ctx context.Context, artifactID string) (int, error)
	SumArtifactSizes(ctx context.Context) (int64, error)

	CreateApplication(ctx context.Context, app *domain.Application) error
	GetApplication(ctx context.Context, idOrName string) (*domain.Application, error)
	ListApplications(ctx context.Context) ([]domain.Application, error)

	CreateRelease(ctx context.Context, release *domain.Release) (*domain.Release, error)
	GetRelease(ctx context.Context, id string) (*domain.Release, error)
	ListReleases(ctx context.Context, applicationID string, limit int) ([]domain.Release, error)

	CreateSchedule(ctx context.Context, schedule *domain.Schedule) error
	GetSchedule(ctx context.Context, ref string) (*domain.Schedule, error)
	ListSchedules(ctx context.Context, limit int) ([]domain.Schedule, error)
	ListEnabledSchedules(ctx context.Context) ([]domain.Schedule, error)
	SetScheduleEnabled(ctx context.Context, id string, enabled bool, now time.Time) (*domain.Schedule, error)
	UpdateScheduleProgress(ctx context.Context, id string, progress domain.ScheduleProgress, now time.Time) error
	DeleteSchedule(ctx context.Context, id string) error
	DispatchScheduledRun(ctx context.Context, in domain.ScheduledDispatch) (domain.ScheduleRun, bool, error)
	RecordMissedRun(ctx context.Context, scheduleID, runID string, scheduledFor, now time.Time) (bool, error)
	ListScheduleRuns(ctx context.Context, scheduleID string, limit int) ([]domain.ScheduleRun, error)
}

// Executor 是进程执行端口，由本机执行器适配器实现。
type Executor interface {
	Run(ctx context.Context, spec domain.CommandSpec) (domain.Result, error)
}

// StorageBackend 是制品内容存储端口。它只接受 digest，不接受调用方提供的文件名，
// 因此「用户输入不得参与路径构造」是结构性保证，而不是靠字符串校验。
type StorageBackend interface {
	Put(ctx context.Context, r io.Reader, expected domain.Digest) (Stored, error)
	Open(ctx context.Context, digest domain.Digest) (io.ReadCloser, error)
	Stat(ctx context.Context, digest domain.Digest) (Stored, error)
	Delete(ctx context.Context, digest domain.Digest) error
	List(ctx context.Context) ([]Stored, error)
}

type Stored struct {
	Digest     domain.Digest
	Size       int64
	ModifiedAt time.Time
}

// SecretResolver 在使用时刻把 SecretRef 解析成明文；解析结果不得落库或落日志。
type SecretResolver interface {
	Resolve(ctx context.Context, ref domain.SecretRef) (string, error)
}

type Defaults struct {
	Timeout          time.Duration
	MaxOutputBytes   int64
	SensitiveEnvKeys []string
}

// ArtifactPolicy 是制品服务的容量与留用策略，来自配置。
type ArtifactPolicy struct {
	MaxUploadBytes int64
	QuotaBytes     int64
}

// ResolvedSecrets 收集一次执行中解析出的明文，用于按值脱敏。
type ResolvedSecrets struct {
	values []string
}

func (r *ResolvedSecrets) Add(value string) {
	if value != "" {
		r.values = append(r.values, value)
	}
}

func (r *ResolvedSecrets) Redactor() *domain.Redactor {
	return domain.NewRedactor(r.values...)
}

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

	// 应用规格：一个应用一份「当前」规格，提交即覆盖；specJSON 由领域层校验并
	// 序列化，仓储只存不解释。
	PutApplicationSpec(ctx context.Context, applicationID string, specJSON []byte, now time.Time, updatedBy string) error
	GetApplicationSpec(ctx context.Context, applicationID string) (*domain.ApplicationSpec, error)

	// 主机与环境在 1c 只是身份与标签，不承载连接语义。FindLocalHost 按
	// address 为空查找本机记录，供启动时的自举使用（没有时返回 nil, nil）。
	CreateHost(ctx context.Context, host *domain.Host) error
	GetHost(ctx context.Context, ref string) (*domain.Host, error)
	ListHosts(ctx context.Context) ([]domain.Host, error)
	FindLocalHost(ctx context.Context) (*domain.Host, error)

	CreateEnvironment(ctx context.Context, environment *domain.Environment) error
	GetEnvironment(ctx context.Context, ref string) (*domain.Environment, error)
	ListEnvironments(ctx context.Context) ([]domain.Environment, error)
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

// RuntimeAdapter 把「应用规格」映射到具体运行时。实现方必须通过
// internal/application/runtimecontract 的共享合约测试，否则 proc 与 systemd 两个
// 实现的语义会各自漂移，而「本地能跑、真机不能跑」正是这类工具最难排查的故障。
//
// 所有方法都不得假设调用方已经校验过规格：实现方必须在 Prepare/Start/Health/Status
// 内部先校验，因为「未经验证的规格被直接启动」是最危险的一类误用。
type RuntimeAdapter interface {
	// Validate 只检查规格能否被本适配器执行，不产生任何副作用。
	Validate(ctx context.Context, spec *domain.ApplicationSpec) error
	// Prepare 创建用户、目录、环境文件与 unit 文件；幂等，可重复调用。
	Prepare(ctx context.Context, spec *domain.ApplicationSpec) error
	Start(ctx context.Context, spec *domain.ApplicationSpec) error
	Stop(ctx context.Context, spec *domain.ApplicationSpec) error
	// Health 返回「能否接流量」，Status 返回「进程本身的状态」。
	// 两者刻意不合并：进程活着不等于已就绪。
	Health(ctx context.Context, spec *domain.ApplicationSpec) (domain.RuntimeHealth, error)
	Status(ctx context.Context, spec *domain.ApplicationSpec) (domain.RuntimeStatus, error)
}

// RuntimePrepareReporter 让装配层把适配器独有的 Prepare 决策（systemd 的 unit 档位与
// 探测到的版本）交给 Operation 的日志与审计。
//
// 它刻意不是 RuntimeAdapter 的一部分：这些信息与运行语义无关、进不了共享合约测试，
// 塞进端口只会逼所有实现各编一份自己的「档位」。装配层本来就持有具体适配器，
// 由它做这一次翻译最自然。
type RuntimePrepareReporter interface {
	// ReportRuntimePrepare 在 Prepare 成功之后被调用；决策未知时返回 ok=false，
	// 而不是返回一个看起来像真的空决策。
	ReportRuntimePrepare(ctx context.Context, spec *domain.ApplicationSpec) (RuntimeDecision, bool)
}

// RuntimeDecision 是一次 Prepare 的最小可追溯记录。字段与 systemd 适配器的
// UnitDecision 对应：1c 只有这一个真实适配器，档位就是它的诊断维度。
type RuntimeDecision struct {
	UnitName       string
	UnitPath       string
	Tier           string
	SystemdVersion int
	Degradations   []string
	DecidedAt      time.Time
}

// runtimeOperationResolver 是 Service 与 RuntimeService 之间的内部接缝：把 kind 与
// 应用引用解析成规范化的 Operation 资源，并在建 Operation 之前完成运行时侧的前置校验。
// 它不导出——除了 RuntimeService 没有别的实现者，调用方只通过 HTTP/CLI 使用。
type runtimeOperationResolver interface {
	ResolveRuntimeOperation(ctx context.Context, kind, appRef string) (string, error)
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

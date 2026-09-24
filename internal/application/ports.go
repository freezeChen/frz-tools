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
	RecoverRunning(ctx context.Context, now time.Time, plan func(domain.Operation) *domain.RetryPlan) ([]domain.Operation, error)
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

	// 备份：策略以 name 为键 upsert（policyID 只在首次插入时使用，冲突时保留原有 id，
	// 否则指向它的历史备份会改归属）；备份记录描述「这份备份能不能用」，
	// 与 Operation 的状态分开。
	SaveBackupPolicy(ctx context.Context, policy *domain.BackupPolicy, policyID string, now time.Time, updatedBy string) (*domain.BackupPolicy, error)
	GetBackupPolicy(ctx context.Context, ref string) (*domain.BackupPolicy, error)
	ListBackupPolicies(ctx context.Context) ([]domain.BackupPolicy, error)
	CreateBackup(ctx context.Context, backup *domain.Backup) error
	GetBackup(ctx context.Context, id string) (*domain.Backup, error)
	ListBackups(ctx context.Context, policyRef string, limit int) ([]domain.Backup, error)
	FinishBackup(ctx context.Context, in domain.FinishBackupInput, now time.Time) (*domain.Backup, error)
	MarkBackupVerified(ctx context.Context, id string, ok bool, now time.Time) error
	FailStaleBackups(ctx context.Context, now time.Time) (int, error)

	// 保留策略（prune）要的三件事。
	//
	// ListBackupsForRetention 按**完成时刻倒序**取该策略的备份，**含非 succeeded 的行**：
	// 「谁参与保留计算」的判据只有领域层的 Usable() 一处，在 SQL 里再写一遍就是第二份
	// 会漂移的真相。
	ListBackupsForRetention(ctx context.Context, policyRef string, limit int) ([]domain.Backup, error)
	// CountBackupReferencesByDigest 数还有多少份**未删除**的备份指着同一个 digest。
	// 删内容之前必须问这一句：内容寻址是按内容去重的，跨策略共享一个 blob 是可能的。
	CountBackupReferencesByDigest(ctx context.Context, digest domain.Digest) (int, error)
	// MarkBackupPruned 把一份备份标记为已清理（只改元数据、不碰内容）。
	// 先标记、后删内容：中断时最坏留下无主的 blob，而不是「元数据说在、内容没了」。
	MarkBackupPruned(ctx context.Context, id string, now time.Time) error
	// HasIncompleteOperation 报告某个资源上还有没有未完成（pending 或 running）的
	// Operation。它与 CreateOperation 判定 LOCK_BUSY 用的是**同一个判据**：同一个词
	// （「这个资源正被用着」）在仓库里只该有一个定义。
	//
	// prune 用它跳过"正在被恢复/校验"的备份。**不能用活跃锁代替**：资源锁是 worker
	// 领取操作时才获取的，一份排队中的恢复拿不到锁——按锁判断会放它过去。
	HasIncompleteOperation(ctx context.Context, resource string) (bool, error)
}

// Executor 是进程执行端口，由本机执行器适配器实现。
type Executor interface {
	// Run 执行命令并把 stdout 收进**有上限的内存缓冲**。超过上限的部分会被丢弃，
	// 因此它只适合输出不大的命令。
	Run(ctx context.Context, spec domain.CommandSpec) (domain.Result, error)

	// RunStream 与 Run 的语义完全一致（argv-only、allowedPaths 校验、超时、取消、
	// 同一套错误码），差别只在 stdout 与 stdin 是调用方给的流。
	//
	// 数据库备份适配器依赖它：一次 pg_dump 的输出可能远大于任何内存缓冲，用 Run
	// 跑会得到一份**记录为成功、内容却残缺**的备份。in / out 为 nil 表示不接。
	RunStream(ctx context.Context, spec domain.CommandSpec, in io.Reader, out io.Writer) (domain.Result, error)
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

// BackupAdapter 把「一份备份策略」映射到具体的资源类型（目录、数据库……）。
//
// 它只产出**逻辑备份流**：压缩、加密、摘要与原子提交到 StorageBackend 由应用层统一
// 负责（迭代 2 规格 D1）。把这些下放给每个适配器，等于让每种数据库各实现一套加密——
// 任何一处写错都是静默的数据泄露，或静默的不可恢复。
//
// 所有方法都不得假设调用方已经校验过策略：实现方必须在内部先校验，
// 因为「未经验证的策略被直接执行」是最危险的一类误用。
type BackupAdapter interface {
	// Kind 报告它负责的资源种类，装配层据此按策略分派。
	Kind() domain.BackupResourceKind

	// Validate 只检查策略能否被本适配器执行，不产生任何副作用。
	Validate(ctx context.Context, policy *domain.BackupPolicy) error

	// Preflight 检查连接、客户端版本、磁盘空间、凭据可解析等前置条件。
	// 必须在 Backup 之前完成，且**不得留下任何备份产物**。
	Preflight(ctx context.Context, policy *domain.BackupPolicy) (domain.PreflightReport, error)

	// Backup 把逻辑备份流写进 w，并返回本次备份的自述元数据。
	Backup(ctx context.Context, policy *domain.BackupPolicy, w io.Writer) (domain.BackupMetadata, error)

	// Restore 从 r 读回逻辑备份流并恢复。mode 决定恢复到真实目标还是隔离环境。
	Restore(ctx context.Context, policy *domain.BackupPolicy, r io.Reader, mode domain.RestoreMode) error

	// Verify 校验备份流的自洽性（如 tar -t），**不接触目标资源**。
	Verify(ctx context.Context, policy *domain.BackupPolicy, r io.Reader) error

	// Cleanup 释放本适配器在本次操作中产生的临时资源（临时目录、临时恢复实例）。
	//
	// 它**不**决定保留策略：「哪些备份该删」由 GFS 策略决定，那是通用逻辑，属于应用层
	// （规格 D2）。保留策略一旦抽象进适配器，每个适配器都要实现一遍，而它们对「一次备份」
	// 的理解各不相同，最后必然漂移成几套语义。
	Cleanup(ctx context.Context, policy *domain.BackupPolicy, operationID string) error
}

// ReleaseAdapter 把一份制品物化成一个可运行的 release 目录，并管理版本的切换与清理。
//
// 它刻意与 RuntimeAdapter 分开：解包是「归档格式 + 路径安全」的逻辑，与「进程管理」正交，
// 而两个运行时实现（systemd / proc）都要用它——放进 RuntimeAdapter 就得各写一遍，
// 那正是「不允许各自实现一份」要避免的事。
//
// 与 RuntimeAdapter 一样，所有方法都不得假设调用方已经校验过规格：实现方必须在内部先校验。
type ReleaseAdapter interface {
	// Materialize 把制品字节解到该 release 的目录里（模式与属主按 1c 的约定：0750、
	// runUser:runUser）。**已存在的目录必须被拒绝**——制品不可变，复用一个目录等于允许
	// 「同一个版本号下换了内容」。
	Materialize(ctx context.Context, spec *domain.ApplicationSpec, releaseID string, artifact io.Reader) error
	// Activate 把 current 指针切到该 release（原子替换符号链接）。
	Activate(ctx context.Context, spec *domain.ApplicationSpec, releaseID string) error
	// Remove 删掉一个 release 目录；实现必须**拒绝删除 current 指向的那个**。
	Remove(ctx context.Context, spec *domain.ApplicationSpec, releaseID string) error
	// List 列出磁盘上实际存在的 release 目录，供发现「库里有记录、盘上没目录」这类不一致。
	List(ctx context.Context, spec *domain.ApplicationSpec) ([]string, error)
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

// backupOperationResolver 是 Service 与 BackupService 之间的内部接缝，与
// runtimeOperationResolver 同形：把 kind 与引用解析成规范化的 Operation 资源。
// 它不导出——除了 BackupService 没有别的实现者。
type backupOperationResolver interface {
	ResolveBackupOperation(ctx context.Context, kind, ref string) (string, error)
	// PrepareRestore 在**创建期**校验一次恢复请求（含原地恢复的显式确认）。
	// 让它在排队之后才失败，等于让运维以为提交成功了。
	PrepareRestore(ctx context.Context, backupID string, mode domain.RestoreMode, confirmed bool) (*RestoreTarget, error)
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

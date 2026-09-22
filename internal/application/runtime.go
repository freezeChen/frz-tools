package application

import (
	"log/slog"
	"time"

	"github.com/freezeChen/frz-tools/internal/idgen"
)

// Options 是 Runtime 的构造参数。端口都以字段提供，便于测试替换。
type Options struct {
	Repo     Repository
	Executor Executor
	Secrets  SecretResolver
	Store    StorageBackend

	AllowExecutable func(string) bool
	Defaults        Defaults
	ArtifactPolicy  ArtifactPolicy

	Workers int
	Idle    time.Duration
	Logger  *slog.Logger

	// NewID 可覆盖，用于测试注入确定性 ID；默认由 idgen.New 按前缀生成。
	NewID func(prefix string) string
	Now   func() time.Time
}

// Runtime 把操作服务、制品服务、目录服务、调度器与 worker 池组合在一起，
// 使它们共享同一个取消注册表：服务负责发出取消信号，worker 池负责响应。
type Runtime struct {
	Service   *Service
	Artifacts *ArtifactService
	Catalogs  *CatalogService
	Schedules *ScheduleService
	Scheduler *Scheduler
	Pool      *Pool
}

func NewRuntime(opts Options) *Runtime {
	idGen := opts.NewID
	if idGen == nil {
		idGen = idgen.New
	}

	cancels := newCancelRegistry()
	pool := newPool(opts.Repo, opts.Executor, opts.Secrets, opts.Defaults, cancels, opts.Workers, opts.Logger)
	if opts.Idle > 0 {
		pool.idle = opts.Idle
	}
	if opts.Now != nil {
		pool.now = opts.Now
	}

	service := newService(opts.Repo, opts.Defaults, opts.AllowExecutable, cancels, func() string {
		return idGen("op")
	}, opts.Logger)
	if opts.Now != nil {
		service.now = opts.Now
	}
	service.notify = pool.Notify

	artifacts := newArtifactService(opts.Repo, opts.Store, opts.ArtifactPolicy, func() string {
		return idGen("art")
	}, opts.Logger)
	if opts.Now != nil {
		artifacts.now = opts.Now
	}

	catalogs := newCatalogService(opts.Repo, idGen, opts.Now)

	// 调度器是触发时刻的唯一权威；它与 worker 池共享唤醒通道，
	// 计划发生变化时立刻重算等待时间，而不是等兜底周期。
	scheduler := newScheduler(opts.Repo, idGen, opts.Logger, opts.Now, pool.Notify)
	schedules := newScheduleService(opts.Repo, opts.Defaults, idGen, opts.Now, scheduler.Wake)

	return &Runtime{
		Service:   service,
		Artifacts: artifacts,
		Catalogs:  catalogs,
		Schedules: schedules,
		Scheduler: scheduler,
		Pool:      pool,
	}
}

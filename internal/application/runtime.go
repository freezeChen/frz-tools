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

	// RuntimeAdapter 是运行时适配器（systemd / proc）。非 Linux 上装配层刻意不注入：
	// runtime.* 于是给出 RUNTIME_UNSUPPORTED，而不是让一个假适配器在生产里假装能用。
	RuntimeAdapter RuntimeAdapter
	// PrepareReporter 由装配层实现，把适配器私有的 Prepare 决策（unit 档位、systemd
	// 版本）翻译成 Operation 的日志与审计字段。
	PrepareReporter RuntimePrepareReporter

	AllowExecutable func(string) bool
	Defaults        Defaults
	ArtifactPolicy  ArtifactPolicy

	Workers int
	Idle    time.Duration
	Logger  *slog.Logger

	// NewID 可覆盖，用于测试注入确定性 ID；默认由 idgen.New 按前缀生成。
	NewID func(prefix string) string
	Now   func() time.Time
	// Jitter 可覆盖退避的抖动源（返回 [0,1)）；默认取 math/rand。
	// 可注入是为了让退避序列在测试里可断言。
	Jitter func() float64
}

// Runtime 把操作服务、制品服务、目录服务、调度器与 worker 池组合在一起，
// 使它们共享同一个取消注册表：服务负责发出取消信号，worker 池负责响应。
type Runtime struct {
	Service   *Service
	Artifacts *ArtifactService
	Catalogs  *CatalogService
	Specs     *SpecService
	Hosts     *HostService
	Runtimes  *RuntimeService
	Schedules *ScheduleService
	Scheduler *Scheduler
	Pool      *Pool
}

func NewRuntime(opts Options) *Runtime {
	idGen := opts.NewID
	if idGen == nil {
		idGen = idgen.New
	}

	// 适配器实例只建一次并与 worker 池共享：systemd 的版本探测与就绪计数都是实例态，
	// 建两份会让同一次 runtime.start 里的探测结果互相不可见。
	runtimes := newRuntimeService(opts.Repo, opts.RuntimeAdapter, opts.PrepareReporter)

	cancels := newCancelRegistry()
	pool := newPool(opts.Repo, opts.Executor, opts.Secrets, opts.Defaults, cancels, runtimes, opts.Workers, opts.Logger, idGen)
	if opts.Idle > 0 {
		pool.idle = opts.Idle
	}
	if opts.Now != nil {
		pool.now = opts.Now
	}
	if opts.Jitter != nil {
		pool.jitter = opts.Jitter
	}

	service := newService(opts.Repo, opts.Defaults, opts.AllowExecutable, cancels, runtimes, func() string {
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
	specs := newSpecService(opts.Repo, opts.Now)
	hosts := newHostService(opts.Repo, idGen, opts.Now)

	// 调度器是触发时刻的唯一权威；它与 worker 池共享唤醒通道，
	// 计划发生变化时立刻重算等待时间，而不是等兜底周期。
	scheduler := newScheduler(opts.Repo, idGen, opts.Logger, opts.Now, pool.Notify)
	schedules := newScheduleService(opts.Repo, opts.Defaults, idGen, opts.Now, scheduler.Wake)

	return &Runtime{
		Service:   service,
		Artifacts: artifacts,
		Catalogs:  catalogs,
		Specs:     specs,
		Hosts:     hosts,
		Runtimes:  runtimes,
		Schedules: schedules,
		Scheduler: scheduler,
		Pool:      pool,
	}
}

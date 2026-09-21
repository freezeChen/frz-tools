package application

import (
	"log/slog"
	"time"
)

// Runtime 把操作服务与 worker 池组合在一起，使两者共享同一个取消注册表：
// 服务负责发出取消信号，worker 池负责响应。
type Runtime struct {
	Service *Service
	Pool    *Pool
}

func NewRuntime(
	repo Repository,
	exec Executor,
	allow func(string) bool,
	defaults Defaults,
	workers int,
	idle time.Duration,
	logger *slog.Logger,
) *Runtime {
	cancels := newCancelRegistry()
	pool := newPool(repo, exec, defaults, cancels, workers, logger)
	if idle > 0 {
		pool.idle = idle
	}
	service := newService(repo, defaults, allow, cancels, logger)
	service.notify = pool.Notify
	return &Runtime{Service: service, Pool: pool}
}

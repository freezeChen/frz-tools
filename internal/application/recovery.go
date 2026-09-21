package application

import (
	"context"
	"log/slog"
	"time"
)

// Recover 把上一个守护进程实例遗留的 running 操作标记为失败，并释放它们持有的
// 资源锁。被中断的命令不会被自动重放。
func Recover(ctx context.Context, repo Repository, logger *slog.Logger, now time.Time) (int, error) {
	recovered, err := repo.RecoverRunning(ctx, now)
	if err != nil {
		return 0, err
	}
	if len(recovered) > 0 {
		logger.Warn("recovered operations interrupted by a previous daemon run", "count", len(recovered))
	}
	return len(recovered), nil
}

package application

import (
	"context"
	"log/slog"
	"math/rand"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/idgen"
)

// RecoveryOptions 是恢复流程需要的装配层依赖。为空时取生产默认值。
type RecoveryOptions struct {
	NewID  func(prefix string) string
	Jitter func() float64
}

// Recover 把上一个守护进程实例遗留的 running 操作标记为失败，并释放它们持有的
// 资源锁。被中断的命令**默认不会被自动重放**——这个默认语义从迭代 0 起就没变过。
//
// 唯一的例外很窄：**显式声明了重试策略**、且 `DAEMON_RESTARTED` 在策略白名单里的操作，
// 会按策略排一次重试（迭代 1d 规格 D5）。之所以不把它扩大成「所有 running 都自动重放」，
// 是因为那会让一个非幂等的命令在每次重启之后被重新执行一次——这是本工具里
// 最危险的一类行为变化。
func Recover(ctx context.Context, repo Repository, logger *slog.Logger, now time.Time, opts RecoveryOptions) (int, error) {
	newID := opts.NewID
	if newID == nil {
		newID = idgen.New
	}
	jitter := opts.Jitter
	if jitter == nil {
		jitter = rand.Float64
	}

	// 判定复用 worker 的那一份逻辑，只是把失败原因固定成 DAEMON_RESTARTED：
	// 「被中断该不该重试」与「执行失败该不该重试」必须是同一套规则。
	plan := func(op domain.Operation) *domain.RetryPlan {
		return planRetryFor(&op, v1.CodeDaemonRestarted, now, newID, jitter)
	}

	recovered, err := repo.RecoverRunning(ctx, now, plan)
	if err != nil {
		return 0, err
	}
	if len(recovered) > 0 {
		logger.Warn("recovered operations interrupted by a previous daemon run", "count", len(recovered))
	}

	// 备份记录也要收尾：被杀死的那次备份会永远停在 running，让运维分不清
	// 「在跑」还是「早就死了」。它本来就不会被当成有效备份（Usable 要求 succeeded），
	// 但挂着不动同样有害。
	if stale, err := repo.FailStaleBackups(ctx, now); err != nil {
		return len(recovered), err
	} else if stale > 0 {
		logger.Warn("marked backups interrupted by a previous daemon run as failed", "count", stale)
	}
	return len(recovered), nil
}

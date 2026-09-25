package systemd

import (
	"context"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/execcmd"
)

// Result / Runner / CommandRunner 是共享实现的**别名**：执行外部命令的语义
// （argv-only、永不经过 shell；err 只表示「命令根本没跑起来」；退出码单独返回；
// stderr 留着报错用）只能有一份实现——systemd 与 nginx 两个适配器都会用到它。
//
// 留别名而不是让调用方直接写 execcmd.X：这个包里的用法（含测试夹具）都以
// 「systemd 适配器怎么执行命令」为语境，改名只会制造无意义的 diff。
type Result = execcmd.Result

type Runner = execcmd.Runner

// CommandRunner 是生产实现。
var CommandRunner Runner = execcmd.Command

// mustRun 执行一条命令并要求成功。错误码由调用方给（同一个「执行失败」在不同阶段的
// 含义完全不同：能否重试、要不要回滚都不一样）。
func (a *Adapter) mustRun(ctx context.Context, code v1.ErrorCode, argv ...string) (Result, error) {
	return execcmd.Must(ctx, a.runner, code, argv...)
}

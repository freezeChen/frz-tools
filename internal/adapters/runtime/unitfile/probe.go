package unitfile

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// VersionRunner 执行一次 argv 并返回 stdout。
//
// 用函数字段把「执行」注入进来，而不是在探测逻辑里直接调 os/exec：开发机是 macOS，
// 没有 systemctl，用真实命令会让探测的单元测试变成环境测试（本地必失败、CI 上则
// 测到的是 runner 的 systemd 版本而不是解析逻辑）。systemd 适配器也借此在测试里
// 注入固定输出。
type VersionRunner func(ctx context.Context, argv []string) ([]byte, error)

// SystemctlArgv 是探测固定使用的命令。它没有参数、不经过 shell，因此不存在参数注入面；
// 版本探测失败时调用方得到的是错误，而不是一个「猜出来的档位」。
var SystemctlArgv = []string{"systemctl", "--version"}

// Prober 通过 systemctl --version 探测 systemd 主版本。
type Prober struct {
	run  VersionRunner
	argv []string
}

// NewProber 用给定的 runner 构造探测器。run 为 nil 时 Detect 会报错，
// 这是有意的：静默降级成「假定 strict」是最危险的默认值。
func NewProber(run VersionRunner) *Prober {
	return &Prober{run: run, argv: SystemctlArgv}
}

// Detect 返回探测到的 systemd 主版本。
//
// 探测失败、输出不可解析、版本低于最低支持版本，三种情况全部报错：
// 猜测档位会把一个在目标主机上必定加载失败的 unit 当成正常产物发出去。
func (p *Prober) Detect(ctx context.Context) (int, error) {
	if p == nil || p.run == nil {
		return 0, domain.NewError(v1.CodeInternal, "systemd 版本探测缺少 runner")
	}
	stdout, err := p.run(ctx, p.argv)
	if err != nil {
		return 0, domain.NewError(v1.CodeRuntimeUnsupport,
			"执行 %s 探测 systemd 版本失败: %v", strings.Join(p.argv, " "), err)
	}
	version, err := ParseSystemctlVersion(string(stdout))
	if err != nil {
		return 0, err
	}
	// 版本下限只在 TierFor 里定义一次，这里复用它以免两处判断漂移。
	if _, err := TierFor(version); err != nil {
		return 0, err
	}
	return version, nil
}

// ParseSystemctlVersion 解析 `systemctl --version` 的输出，形如：
//
//	systemd 255
//	+PAM +AUDIT +SELINUX ...
//
// 只认首行、且首行第二个 token 必须是十进制主版本；`systemd 252 (252.5-2~deb12u1)`
// 这类带括号补丁号的形态同样接受。其余形态一律报错而不是猜。
func ParseSystemctlVersion(output string) (int, error) {
	firstLine, _, _ := strings.Cut(output, "\n")
	fields := strings.Fields(firstLine)
	trimmed := strings.TrimSpace(firstLine)
	if len(fields) < 2 || fields[0] != "systemd" {
		return 0, domain.NewError(v1.CodeRuntimeUnsupport,
			"无法从 %q 解析 systemd 版本：期望形如 `systemd 255`", trimmed)
	}
	version, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, domain.NewError(v1.CodeRuntimeUnsupport,
			"systemd 版本号不是十进制整数: %q", fields[1])
	}
	return version, nil
}

// CommandRunner 是基于 exec.CommandContext 的 VersionRunner：argv-only、无 shell，
// 与 internal/adapters/executor 的做法一致。
func CommandRunner(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("argv 不能为空")
	}
	return exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
}

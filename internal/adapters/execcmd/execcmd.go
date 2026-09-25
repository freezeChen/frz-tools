// Package execcmd 是「执行一条外部命令并拿到结果」这件事的**唯一**实现。
//
// 抽出来是因为它有两个消费者：systemd 适配器（systemctl）与 nginx 适配器（`nginx -t` /
// `nginx -s reload`）。两边各写一份的话，最容易漂移的是三件**有语义**的细节：
//
//  1. 只接受 argv，**永不经过 shell**（1c 起就是硬约定，见 AGENTS.md）；
//  2. 命令**根本没跑起来**（找不到二进制、context 被取消）才返回 err；跑起来了但退出码非零
//     是**正常结果**，由调用方读 ExitCode 判断——压成一个 error 就再也分不出这两种情况，
//     而 systemctl 与 nginx 都用退出码表达语义；
//  3. stderr 要留着：这两个工具把原因写在 stderr 里，报错时不带它等于让人去猜。
package execcmd

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// Result 是一条外部命令的结果。
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner 以 argv 执行一条命令。做成函数类型是为了可注入：macOS 上没有 systemctl、
// 测试机上未必有 nginx，单元测试必须能整段替换掉「怎么执行命令」这件事。
type Runner func(ctx context.Context, argv []string) (Result, error)

// Command 是生产实现：argv-only、exec.CommandContext。
func Command(ctx context.Context, argv []string) (Result, error) {
	if len(argv) == 0 {
		return Result{}, errors.New("argv 不能为空")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	err := command.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return result, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	// 命令没跑起来：退出码标成 -1，避免调用方把它当成一次「正常退出」。
	result.ExitCode = -1
	return result, err
}

// Must 执行一条命令并要求它成功，失败时用调用方给的错误码包一句能直接读的话。
//
// 错误码必须由调用方给：同一个「执行失败」，在 Prepare 与在切流里的含义完全不同
// （能否重试、要不要回滚都不一样），而这个包不可能知道。
func Must(ctx context.Context, runner Runner, code v1.ErrorCode, argv ...string) (Result, error) {
	result, err := runner(ctx, argv)
	if err != nil {
		return result, domain.NewError(code, "执行 %s 失败: %v", strings.Join(argv, " "), err)
	}
	if result.ExitCode != 0 {
		return result, domain.NewError(code, "%s 退出码 %d: %s",
			strings.Join(argv, " "), result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return result, nil
}

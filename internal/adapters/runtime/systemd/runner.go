package systemd

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// Result 是一条外部命令的结果。退出码单独返回、不压进 error，是因为 systemctl
// 用退出码表达语义（例如「没有这个 unit」），压成一个 error 之后就再也分不出来。
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner 以 argv 执行一条命令，永不经过 shell。
//
// err 只在「命令根本没跑起来」时非 nil（找不到二进制、context 被取消）；
// 命令跑起来但退出码非零时 err 为 nil，调用方读 Result.ExitCode。
// 做成函数字段是为了可注入：macOS 上没有 systemctl，单元测试必须能整段替换掉
// 「怎么执行命令」这件事，否则这个包在开发机上根本测不了。
type Runner func(ctx context.Context, argv []string) (Result, error)

// CommandRunner 是生产实现：argv-only、exec.CommandContext，与
// internal/adapters/executor 的做法一致。
func CommandRunner(ctx context.Context, argv []string) (Result, error) {
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

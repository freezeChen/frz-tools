package systemd

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"os/user"
	"strconv"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
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

// OwnerResolver 把 runUser 解析成 uid/gid。
//
// 做成可注入的，是因为测试进程通常不是 root，无法把文件 chown 给别的用户；
// 测试注入当前进程的 uid/gid，从而仍能断言「chown 真的被调用过、模式真的落实了」。
type OwnerResolver func(name string) (uid, gid int, err error)

// OSUserOwner 是生产实现：查系统用户数据库——useradd 写进去的就是同一份数据。
func OSUserOwner(name string) (int, int, error) {
	account, err := user.Lookup(name)
	if err != nil {
		return 0, 0, domain.NewError(v1.CodeInternal, "解析用户 %q 失败: %v", name, err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return 0, 0, domain.NewError(v1.CodeInternal, "用户 %q 的 uid 不是数字: %q", name, account.Uid)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return 0, 0, domain.NewError(v1.CodeInternal, "用户 %q 的 gid 不是数字: %q", name, account.Gid)
	}
	return uid, gid, nil
}

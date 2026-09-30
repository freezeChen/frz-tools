package e2e

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// runOpsctlCapture 是 runOpsctlBinary 的「连 stderr 一起拿回来」版本。
// 迭代 6b 起 opsctl 的错误出口是 `opsctl: <CODE>: <消息>`（规格 D4），
// 断言这个形状就必须把 stderr 与退出码分开拿。
func runOpsctlCapture(t *testing.T, args ...string) (string, string, int, error) {
	t.Helper()
	cmd := exec.Command(opsctlBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	}
	return stdout.String(), stderr.String(), code, err
}

// 错误文本带码（迭代 6 规格 §9.2 第 1 条）：制造 LOCK_BUSY（同一 resource
// 连续提交两个操作），stderr 必须出现 `LOCK_BUSY:`，退出码不变（仍为 4）。
func TestErrorOutputCarriesErrorCode(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	// 先占住资源锁：一个还在跑的操作会让同资源的下一次提交吃 LOCK_BUSY。
	first := submit(t, d.socket, "stderr-code", "/bin/sleep", "3")
	t.Cleanup(func() { waitForStatus(t, d.socket, first.ID, "succeeded", "failed") })

	// 不带 --json：走人类可读的那条出口。
	_, stderr, code, err := runOpsctlCapture(t, "--socket", d.socket,
		"operation", "submit", "--kind", v1.KindExecutorCommand,
		"--resource", "stderr-code", "--", "/usr/bin/true")
	if err == nil {
		t.Fatal("同资源第二个操作应当被拒绝（LOCK_BUSY）")
	}
	if code != 4 {
		t.Fatalf("LOCK_BUSY 的退出码应当仍是 4，得到 %d（stderr：%s）", code, stderr)
	}
	// 新形状：opsctl: <CODE>: <消息>。码在文本里，不用拿退出码去反查表。
	if !strings.HasPrefix(stderr, "opsctl: LOCK_BUSY: ") {
		t.Fatalf("stderr 应当是 `opsctl: LOCK_BUSY: <消息>` 的形状：\n%s", stderr)
	}
}

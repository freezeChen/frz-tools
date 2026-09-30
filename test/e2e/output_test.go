package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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

// follow 落地（迭代 6 规格 §9.2 第 3 条）：提交一个有日志输出的真实异步操作，
// follow 持续输出直到终态后退出，成功退出码 0；游标不重复打印已输出的条目。
func TestLogsFollowExitsAfterTerminalSuccess(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	op := submit(t, d.socket, "follow-success", "/bin/sh", "-c", "echo follow-marker; sleep 2")
	stdout, stderr, code, err := runOpsctlCapture(t, "--socket", d.socket,
		"operation", "logs", op.ID, "--follow")
	if err != nil {
		t.Fatalf("follow 应当随操作成功而成功退出：%v\nstderr：%s", err, stderr)
	}
	if code != 0 {
		t.Fatalf("成功终态的退出码应当是 0，得到 %d（stderr：%s）", code, stderr)
	}
	if !strings.Contains(stdout, "follow-marker") {
		t.Fatalf("follow 应当输出命令的 stdout 日志：\n%s", stdout)
	}
	if !strings.Contains(stdout, "operation finished") {
		t.Fatalf("follow 应当输出终态日志条目：\n%s", stdout)
	}
	// 游标必须前进：终态条目只在第一批或补拉的那一轮里出现一次，
	// 打出两遍说明循环没有按游标去重。
	if n := strings.Count(stdout, "operation finished"); n != 1 {
		t.Fatalf("operation finished 应当恰好出现一次（游标去重），实际 %d 次：\n%s", n, stdout)
	}
}

// 失败的操作：follow 把剩余日志拉完后，按该操作的错误码经 api/v1 退出码表退出
// （EXEC_EXIT_NONZERO → 12），错误行带码。
func TestLogsFollowExitsWithMappedCodeOnFailure(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	op := submit(t, d.socket, "follow-failure", "/usr/bin/false")
	_, stderr, code, err := runOpsctlCapture(t, "--socket", d.socket,
		"operation", "logs", op.ID, "--follow")
	if err == nil {
		t.Fatalf("follow 一个失败的操作不该报成功：%s", stderr)
	}
	if code != 12 {
		t.Fatalf("EXEC_EXIT_NONZERO 应当映射到退出码 12，得到 %d（stderr：%s）", code, stderr)
	}
	if !strings.Contains(stderr, "EXEC_EXIT_NONZERO:") {
		t.Fatalf("失败时的错误行应当带码：\n%s", stderr)
	}
}

// Ctrl-C 即停（迭代 6 规格 D5）：跟随中的 opsctl 收到 SIGINT 后干净退出（0），
// 操作本身不受影响，仍可继续到终态。
func TestLogsFollowStopsCleanlyOnInterrupt(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	op := submit(t, d.socket, "follow-interrupt", "/bin/sleep", "30")
	t.Cleanup(func() {
		// 无论断言结果如何都别把 30 秒的锁留给同包后面的用例。
		_, _, _ = runOpsctl(t, d.socket, "operation", "cancel", op.ID)
		waitForStatus(t, d.socket, op.ID, "cancelled", "succeeded", "failed")
	})

	cmd := exec.Command(opsctlBinary, "--socket", d.socket, "operation", "logs", op.ID, "--follow")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start opsctl: %v", err)
	}

	// 留出进入轮询循环的时间，再发 SIGINT。
	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("发送 SIGINT: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Ctrl-C 之后应当干净退出（退出码 0）：%v\nstderr：%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("Ctrl-C 之后没有退出\nstderr：%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "已停止跟随") {
		t.Fatalf("停止跟随应当有一句交代：\n%s", stderr.String())
	}

	// 被跟随的操作不受影响：还能取消成功。
	if _, _, err := runOpsctl(t, d.socket, "operation", "cancel", op.ID); err != nil {
		t.Fatalf("操作应当仍可取消：%v", err)
	}
	cancelled := waitForStatus(t, d.socket, op.ID, "cancelled")
	if cancelled.Status != "cancelled" {
		t.Fatalf("操作应当以 cancelled 结束，得到 %+v", cancelled)
	}
}

// --json 与 --follow 不能同时给：follow 是持续的增量输出，拼不成一份 JSON 文档。
func TestLogsFollowRejectsJSON(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	op := submit(t, d.socket, "follow-json", "/usr/bin/true")
	_, _, code, err := runOpsctlCapture(t, "--socket", d.socket,
		"operation", "logs", op.ID, "--follow", "--json")
	if err == nil || code != 2 {
		t.Fatalf("--follow 与 --json 同时给出应当以 INVALID_REQUEST（退出码 2）拒绝，得到 %d（%v）", code, err)
	}
}

// 非 follow 的 logs --json 路径保持原状（原始响应），--follow 没有把它带走。
func TestLogsJSONPathUnchanged(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	op := submit(t, d.socket, "logs-json", "/usr/bin/true")
	waitForStatus(t, d.socket, op.ID, "succeeded")

	stdout, _, err := runOpsctl(t, d.socket, "operation", "logs", op.ID, "--json")
	if err != nil {
		t.Fatalf("logs --json: %v", err)
	}
	var response struct {
		APIVersion string `json:"apiVersion"`
		Operation  string `json:"operation"`
		Items      []struct {
			Message string `json:"message"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(stdout), &response); err != nil {
		t.Fatalf("logs --json 应当仍然是原始响应：%v\n%s", err, stdout)
	}
	if response.APIVersion != v1.APIVersion || len(response.Items) == 0 {
		t.Fatalf("logs --json 的形状不对：\n%s", stdout)
	}
}

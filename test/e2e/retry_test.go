package e2e

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"

	_ "modernc.org/sqlite"
)

// retryRow 是直接查库读出的一条重试链上的操作。
// 只看 CLI 拿到的第一个操作证明不了「真的又跑了一次」，所以这里查库看链。
type retryRow struct {
	ID            string
	Status        string
	Attempt       int
	RetryOf       string
	ErrorCode     string
	NextAttemptAt sql.NullString
}

func operationsOn(t *testing.T, database, resource string) []retryRow {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+database)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	rows, err := db.Query(
		`SELECT id, status, attempt, COALESCE(retry_of, ''), COALESCE(error_code, ''), not_before
		 FROM operations WHERE resource = ? ORDER BY created_at, id`, resource)
	if err != nil {
		t.Fatalf("query operations: %v", err)
	}
	defer rows.Close()

	var out []retryRow
	for rows.Next() {
		var row retryRow
		if err := rows.Scan(&row.ID, &row.Status, &row.Attempt, &row.RetryOf, &row.ErrorCode, &row.NextAttemptAt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// flakyArgv 返回一个「第一次失败、第二次起成功」的命令：读计数文件，为 0 时写 1 并退出 1，
// 否则退出 0。用它来证明自动重试真的把命令又跑了一遍。
func flakyArgv(t *testing.T, dir string) []string {
	t.Helper()
	counter := filepath.Join(dir, "attempts")
	script := fmt.Sprintf(
		`n=$(cat %q 2>/dev/null || echo 0); n=$((n+1)); echo $n > %q; [ "$n" -ge 2 ]`,
		counter, counter)
	return []string{"/bin/sh", "-c", script}
}

func counterValue(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "attempts"))
	if err != nil {
		return ""
	}
	return string(raw)
}

// submitWithRetry 提交一个声明了重试策略的操作并返回它。
func submitWithRetry(t *testing.T, socket, resource string, maxAttempts int, base string, argv ...string) v1.Operation {
	t.Helper()
	args := append([]string{
		"operation", "submit",
		"--kind", v1.KindExecutorCommand,
		"--resource", resource,
		"--retry-max", fmt.Sprint(maxAttempts),
		"--retry-base", base,
		"--json", "--",
	}, argv...)
	stdout, _, err := runOpsctl(t, socket, args...)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return decodeOperation(t, stdout)
}

// 端到端：声明了重试的操作在失败后自动又跑了一次，并且链在 CLI 上可查。
func TestRetryReRunsCommandThroughCLI(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	dir := t.TempDir()
	first := submitWithRetry(t, d.socket, "retry-e2e", 3, "1s", flakyArgv(t, dir)...)

	// 第一跳必定失败（计数文件从 0 开始）。
	failed := waitForStatus(t, d.socket, first.ID, string(domain.StatusFailed))
	if failed.ErrorCode != string(v1.CodeExecExitNonZero) {
		t.Fatalf("第一跳 want EXEC_EXIT_NONZERO, got %q", failed.ErrorCode)
	}
	if failed.Attempt != 1 || failed.MaxAttempts != 3 {
		t.Fatalf("第一跳的 attempt/maxAttempts want 1/3, got %d/%d", failed.Attempt, failed.MaxAttempts)
	}

	// 第二跳由退避排出来，等它跑完。
	waitFor(t, 15*time.Second, func() bool {
		rows := operationsOn(t, d.database, "retry-e2e")
		return len(rows) == 2 && rows[1].Status == string(domain.StatusSucceeded)
	})

	rows := operationsOn(t, d.database, "retry-e2e")
	if len(rows) != 2 {
		t.Fatalf("want 2 跳, got %d", len(rows))
	}
	if rows[1].RetryOf != first.ID {
		t.Fatalf("第二跳的 retryOf want %s, got %s", first.ID, rows[1].RetryOf)
	}
	if rows[1].Attempt != 2 {
		t.Fatalf("第二跳 attempt want 2, got %d", rows[1].Attempt)
	}

	// 命令真的被执行了两次，而不是「库里多了一行但没跑」。
	if got := counterValue(t, dir); got != "2\n" {
		t.Fatalf("计数文件 want 2, got %q", got)
	}

	// 链上的第二跳在 CLI 上可查，且 attempt / maxAttempts 逐字段可见。
	stdout, _, err := runOpsctl(t, d.socket, "operation", "get", rows[1].ID, "--json")
	if err != nil {
		t.Fatalf("get retry: %v", err)
	}
	var decoded v1.Operation
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Attempt != 2 || decoded.MaxAttempts != 3 {
		t.Fatalf("CLI 输出 want attempt/maxAttempts 2/3, got %d/%d", decoded.Attempt, decoded.MaxAttempts)
	}
	if decoded.RetryOf != first.ID {
		t.Fatalf("CLI 输出应当带 retryOf=%s, got %q", first.ID, decoded.RetryOf)
	}
}

// 退避必须在**领取之前**过滤掉：一个排了长退避的重试，应当在客户端的口径里
// 明确表现为「还在等」，而不是被 worker 领走占着。
func TestRetryBackoffIsVisibleAndNotClaimed(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	dir := t.TempDir()
	// base 取 60 秒：第二次尝试在测试窗口内不可能到期。
	first := submitWithRetry(t, d.socket, "retry-backoff", 2, "60s", flakyArgv(t, dir)...)

	waitForStatus(t, d.socket, first.ID, string(domain.StatusFailed))

	waitFor(t, 10*time.Second, func() bool {
		return len(operationsOn(t, d.database, "retry-backoff")) == 2
	})
	rows := operationsOn(t, d.database, "retry-backoff")
	retryID := rows[1].ID

	// 第二跳还停在 pending，且带着一个未来的 nextAttemptAt。
	stdout, _, err := runOpsctl(t, d.socket, "operation", "get", retryID, "--json")
	if err != nil {
		t.Fatalf("get retry: %v", err)
	}
	var decoded v1.Operation
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Status != string(domain.StatusPending) {
		t.Fatalf("退避期间 want pending, got %s", decoded.Status)
	}
	if decoded.NextAttemptAt == nil {
		t.Fatal("退避期间必须给出 nextAttemptAt，否则调用方无法判断还要等多久")
	}
	if !decoded.NextAttemptAt.After(time.Now()) {
		t.Fatalf("nextAttemptAt 应当在未来，got %s", decoded.NextAttemptAt)
	}

	// 命令只跑过一次——退避中的重试没有被领取。
	if got := counterValue(t, dir); got != "1\n" {
		t.Fatalf("退避期间命令不该再被执行，计数文件 got %q", got)
	}

	// 人类可读输出也应当能看出「在等下一次」。
	human, _, err := runOpsctl(t, d.socket, "operation", "get", retryID)
	if err != nil {
		t.Fatalf("get retry: %v", err)
	}
	if !strings.Contains(human, "attempt:") || !strings.Contains(human, "nextAttemptAt:") {
		t.Fatalf("人类可读输出应当带 attempt 与 nextAttemptAt，got:\n%s", human)
	}
}

// waitFor 轮询一个条件，超时即失败。
func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待条件超时（%s）", timeout)
}

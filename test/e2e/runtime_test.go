package e2e

import (
	"database/sql"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"

	_ "modernc.org/sqlite"
)

// countRuntimeOperations 直接查库统计 runtime.* 的操作行：这条断言要证明的是
// 「快速失败不建 Operation」，只看 CLI 输出证明不了。
func countRuntimeOperations(t *testing.T, database string) int {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+database)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM operations WHERE kind LIKE 'runtime.%'`).Scan(&count); err != nil {
		t.Fatalf("count runtime operations: %v", err)
	}
	return count
}

// 本机（macOS）没有注入 systemd 适配器：runtime.* 必须给出 RUNTIME_UNSUPPORTED（退出码 21），
// 而不是 500、panic，或一条注定失败的 Operation。
//
// 真正执行 systemd 的端到端链路需要 Linux 容器，属于 A7（见汇报的未验证项）。
func TestRuntimeWithoutAdapterThroughCLI(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	// 未登记 manifest 的应用：SPEC_NOT_FOUND（退出码 2）优先于「适配器不可用」——
	// §11 要求未提交 manifest 的应用在 runtime/* 上返回这个码。
	if _, _, err := runOpsctl(t, d.socket, "app", "create", "empty-app"); err != nil {
		t.Fatalf("create application: %v", err)
	}
	for _, action := range []string{"validate", "prepare", "start", "stop", "health"} {
		if _, code, err := runOpsctl(t, d.socket, "runtime", action, "--app", "empty-app"); code != 2 {
			t.Fatalf("runtime %s 未提交 manifest want exit 2 (SPEC_NOT_FOUND), got %d: %v", action, code, err)
		}
	}

	// 提交一份完整 manifest 之后，失败原因变成「本机没有可用适配器」。
	dir := t.TempDir()
	if _, _, err := runOpsctl(t, d.socket, "app", "create", "billing-api"); err != nil {
		t.Fatalf("create application: %v", err)
	}
	artifact := uploadBillingArtifact(t, d, dir)
	manifestPath := writeArtifactFile(t, dir, "billing-api.yaml", billingManifest(artifact.ID))
	if _, _, err := runOpsctl(t, d.socket, "spec", "put", "--app", "billing-api", "--file", manifestPath); err != nil {
		t.Fatalf("put spec: %v", err)
	}
	for _, action := range []string{"validate", "prepare", "start", "stop", "health"} {
		stdout, code, err := runOpsctl(t, d.socket, "runtime", action, "--app", "billing-api")
		if code != 21 {
			t.Fatalf("runtime %s want exit 21 (RUNTIME_UNSUPPORTED), got %d: %v (%s)", action, code, err, stdout)
		}
		if err == nil {
			t.Fatalf("runtime %s 必须失败，不能静默成功", action)
		}
	}

	// start 是快速失败：适配器不可用是部署属性，排队执行没有意义，
	// 因此不得留下任何 runtime.* 的操作行污染审计与 operation 列表。
	if count := countRuntimeOperations(t, d.database); count != 0 {
		t.Fatalf("want no runtime.* operation rows, got %d", count)
	}

	// 回归：同一个守护进程里 executor.command 的行为不受影响。
	op := submit(t, d.socket, "runtime-regression", "/usr/bin/true")
	finished := waitForStatus(t, d.socket, op.ID, string(domain.StatusSucceeded), string(domain.StatusFailed))
	if finished.Status != string(domain.StatusSucceeded) {
		t.Fatalf("executor.command 链路被破坏：status=%s errorCode=%s", finished.Status, finished.ErrorCode)
	}
	if finished.Kind != v1.KindExecutorCommand {
		t.Fatalf("kind want %s, got %s", v1.KindExecutorCommand, finished.Kind)
	}
}

package e2e

import (
	"database/sql"
	stdruntime "runtime"
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

// 未装配适配器时，runtime.* 必须给出 RUNTIME_UNSUPPORTED（退出码 21），而不是 500、
// panic，或一条注定失败的 Operation。
//
// 这个前提只在非 Linux 上成立：装配层的平台选择是 GOOS == linux 才注入 systemd 适配器
// （见 cmd/opsd/main.go）。所以两个平台断言的是相反的命题，不能把 macOS 的行为写死——
// 那样这个用例在 Linux CI 上必然失败。Linux 分支只钉「适配器确实被装配」这一件事；
// 真正的启停与就绪链路需要 root 与 systemd，由 Linux 容器断言覆盖（test/linux/verify.sh
// 的 check_runtime）。
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

	// 提交一份完整 manifest 之后，前置校验都通过，剩下就由「本机有没有适配器」决定，
	// 而这是平台属性（见函数头的说明）。
	dir := t.TempDir()
	if _, _, err := runOpsctl(t, d.socket, "app", "create", "billing-api"); err != nil {
		t.Fatalf("create application: %v", err)
	}
	artifact := uploadBillingArtifact(t, d, dir)
	manifestPath := writeArtifactFile(t, dir, "billing-api.yaml", billingManifest(artifact.ID))
	if _, _, err := runOpsctl(t, d.socket, "spec", "put", "--app", "billing-api", "--file", manifestPath); err != nil {
		t.Fatalf("put spec: %v", err)
	}
	if stdruntime.GOOS == "linux" {
		// Linux：适配器已装配，因此这里断言的是反面——同一个调用不再报
		// RUNTIME_UNSUPPORTED，而是真的校验通过（validate 是同步用例，只做校验，
		// 不建用户也不建目录，因此在非 root 下也能通过）。
		stdout, code, err := runOpsctl(t, d.socket, "runtime", "validate", "--app", "billing-api")
		if code != 0 {
			t.Fatalf("Linux 上已装配 systemd 适配器，runtime validate want exit 0, got %d: %v (%s)", code, err, stdout)
		}
		if count := countRuntimeOperations(t, d.database); count != 0 {
			t.Fatalf("runtime validate 是同步用例，不得留下 Operation 行，got %d", count)
		}
	} else {
		// 未装配适配器的平台：五个动作都必须快速失败。适配器不可用是部署属性，
		// 排队执行没有意义，因此也不得留下任何 runtime.* 操作行污染审计与 operation 列表。
		for _, action := range []string{"validate", "prepare", "start", "stop", "health"} {
			stdout, code, err := runOpsctl(t, d.socket, "runtime", action, "--app", "billing-api")
			if code != 21 {
				t.Fatalf("runtime %s want exit 21 (RUNTIME_UNSUPPORTED), got %d: %v (%s)", action, code, err, stdout)
			}
			if err == nil {
				t.Fatalf("runtime %s 必须失败，不能静默成功", action)
			}
		}
		if count := countRuntimeOperations(t, d.database); count != 0 {
			t.Fatalf("want no runtime.* operation rows, got %d", count)
		}
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

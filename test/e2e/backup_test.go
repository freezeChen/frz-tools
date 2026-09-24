package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// backupPolicyManifest 造一份指向 sourceDir 的策略 manifest。
//
// 刻意关掉加密：这一组用例要验的是**路径与流程**能不能走通，而密钥得配一份凭据才
// 解析得了。加密的往返已由单元与集成测试覆盖（含「落盘字节里不得出现明文」）。
func backupPolicyManifest(name, sourceDir string) string {
	return fmt.Sprintf(`apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: %s
resource:
  kind: files
  paths:
    - %s
encoding:
  compression: gzip
  encryption:
    enabled: false
retention:
  keepLast: 3
`, name, sourceDir)
}

func writeManifest(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

// runBackupCommand 提交一条返回 Operation 的备份类命令并解析出操作。
func runBackupCommand(t *testing.T, socket string, args ...string) v1.Operation {
	t.Helper()
	full := append(args, "--json")
	stdout, _, err := runOpsctl(t, socket, full...)
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	var op v1.Operation
	if err := json.Unmarshal([]byte(stdout), &op); err != nil {
		t.Fatalf("decode operation from %q: %v", stdout, err)
	}
	return op
}

// 走 CLI 的完整闭环：提交策略 → 备份 → 列出 → 校验 → 清空 → 原地恢复 → 内容一致。
func TestBackupRoundTripThroughCLI(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	// 被备份的目录：真实路径、真实内容。生产上 files 适配器的根前缀是 "/"，
	// 因此策略里直接写这个绝对路径。
	sourceDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(filepath.Join(sourceDir, "nested"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "alpha.txt"), []byte("alpha-content\n"), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "nested", "beta.bin"), []byte{0, 1, 2, 255}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	dir := t.TempDir()
	manifest := writeManifest(t, dir, "policy.yaml", backupPolicyManifest("e2e-backup", sourceDir))

	if _, _, err := runOpsctl(t, d.socket, "backup", "policy", "put", "--file", manifest); err != nil {
		t.Fatalf("policy put: %v", err)
	}

	// 读回：策略应当能被列出，且字段完整。
	stdout, _, err := runOpsctl(t, d.socket, "backup", "policy", "list", "--json")
	if err != nil {
		t.Fatalf("policy list: %v", err)
	}
	var policies v1.BackupPolicyListResponse
	if err := json.Unmarshal([]byte(stdout), &policies); err != nil {
		t.Fatalf("decode policies: %v", err)
	}
	if len(policies.Items) != 1 || policies.Items[0].Name != "e2e-backup" {
		t.Fatalf("策略列表不对: %+v", policies.Items)
	}

	// 备份。
	runOp := runBackupCommand(t, d.socket, "backup", "run", "--policy", "e2e-backup")
	if runOp.Kind != v1.KindBackupRun {
		t.Fatalf("kind want %s, got %s", v1.KindBackupRun, runOp.Kind)
	}
	finished := waitForStatus(t, d.socket, runOp.ID, string(domain.StatusSucceeded), string(domain.StatusFailed))
	if finished.Status != string(domain.StatusSucceeded) {
		t.Fatalf("备份应当成功，got %s (%s: %s)", finished.Status, finished.ErrorCode, finished.ErrorMessage)
	}

	// 找出这条备份记录。
	stdout, _, err = runOpsctl(t, d.socket, "backup", "list", "--json")
	if err != nil {
		t.Fatalf("backup list: %v", err)
	}
	var backups v1.BackupListResponse
	if err := json.Unmarshal([]byte(stdout), &backups); err != nil {
		t.Fatalf("decode backups: %v", err)
	}
	if len(backups.Items) != 1 {
		t.Fatalf("应当恰好一条备份记录，got %d", len(backups.Items))
	}
	record := backups.Items[0]
	if record.Status != string(domain.BackupSucceeded) {
		t.Fatalf("备份记录应当 succeeded，got %s（%s）", record.Status, record.ErrorMessage)
	}
	if record.StorageDigest == "" {
		t.Fatal("成功的备份必须记录 digest")
	}
	if record.OperationID != runOp.ID {
		t.Fatalf("备份记录应当指回发起它的 Operation: want %s, got %s", runOp.ID, record.OperationID)
	}

	// 校验：走 Operation。
	verifyOp := runBackupCommand(t, d.socket, "backup", "verify", record.ID)
	if verifyFinished := waitForStatus(t, d.socket, verifyOp.ID,
		string(domain.StatusSucceeded), string(domain.StatusFailed)); verifyFinished.Status != string(domain.StatusSucceeded) {
		t.Fatalf("校验应当成功，got %s (%s)", verifyFinished.Status, verifyFinished.ErrorMessage)
	}

	// 原地恢复**必须**显式确认；不带时应当在提交期就被拒（退出码 25）。
	if _, code, err := runOpsctl(t, d.socket, "backup", "restore", record.ID, "--mode", "inPlace"); code != 25 {
		t.Fatalf("未确认的原地恢复 want exit 25 (BACKUP_RESTORE_UNCONFIRMED), got %d: %v", code, err)
	}

	// 清空源目录，再原地恢复：从空到有证明不了「真的把内容写回来了」。
	if err := os.RemoveAll(sourceDir); err != nil {
		t.Fatalf("wipe: %v", err)
	}

	restoreOp := runBackupCommand(t, d.socket, "backup", "restore", record.ID,
		"--mode", "inPlace", "--confirm")
	if restoreFinished := waitForStatus(t, d.socket, restoreOp.ID,
		string(domain.StatusSucceeded), string(domain.StatusFailed)); restoreFinished.Status != string(domain.StatusSucceeded) {
		t.Fatalf("原地恢复应当成功，got %s (%s)", restoreFinished.Status, restoreFinished.ErrorMessage)
	}

	// 内容一致：文件内容与权限位都要对上。
	alpha, err := os.ReadFile(filepath.Join(sourceDir, "alpha.txt"))
	if err != nil {
		t.Fatalf("read restored alpha: %v", err)
	}
	if string(alpha) != "alpha-content\n" {
		t.Fatalf("恢复的内容不对: %q", alpha)
	}
	info, err := os.Stat(filepath.Join(sourceDir, "alpha.txt"))
	if err != nil {
		t.Fatalf("stat restored alpha: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("恢复的权限位不对：want 0640, got %o", info.Mode().Perm())
	}
	beta, err := os.ReadFile(filepath.Join(sourceDir, "nested", "beta.bin"))
	if err != nil {
		t.Fatalf("read restored beta: %v", err)
	}
	if len(beta) != 4 || beta[3] != 0xff {
		t.Fatalf("二进制内容恢复不对: %v", beta)
	}
}

// 隔离恢复不碰真实数据，而且对不存在的备份给出明确的 NOT_FOUND。
func TestBackupRestoreIsolatedAndNotFoundThroughCLI(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	sourceDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(sourceDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	dir := t.TempDir()
	manifest := writeManifest(t, dir, "policy.yaml", backupPolicyManifest("e2e-isolated", sourceDir))
	if _, _, err := runOpsctl(t, d.socket, "backup", "policy", "put", "--file", manifest); err != nil {
		t.Fatalf("policy put: %v", err)
	}
	runOp := runBackupCommand(t, d.socket, "backup", "run", "--policy", "e2e-isolated")
	if finished := waitForStatus(t, d.socket, runOp.ID,
		string(domain.StatusSucceeded), string(domain.StatusFailed)); finished.Status != string(domain.StatusSucceeded) {
		t.Fatalf("备份应当成功，got %s (%s)", finished.Status, finished.ErrorMessage)
	}

	stdout, _, err := runOpsctl(t, d.socket, "backup", "list", "--json")
	if err != nil {
		t.Fatalf("backup list: %v", err)
	}
	var backups v1.BackupListResponse
	if err := json.Unmarshal([]byte(stdout), &backups); err != nil {
		t.Fatalf("decode: %v", err)
	}
	record := backups.Items[0]

	// 隔离恢复：不需要确认，也不该碰真实数据。
	op := runBackupCommand(t, d.socket, "backup", "restore", record.ID, "--mode", "isolated")
	if finished := waitForStatus(t, d.socket, op.ID,
		string(domain.StatusSucceeded), string(domain.StatusFailed)); finished.Status != string(domain.StatusSucceeded) {
		t.Fatalf("隔离恢复应当成功，got %s (%s)", finished.Status, finished.ErrorMessage)
	}
	content, err := os.ReadFile(filepath.Join(sourceDir, "keep.txt"))
	if err != nil {
		t.Fatalf("隔离恢复之后源文件应当还在: %v", err)
	}
	if string(content) != "keep" {
		t.Fatalf("隔离恢复改动了真实数据: %q", content)
	}

	// 不存在的备份：退出码 2（BACKUP_NOT_FOUND）。
	if _, code, err := runOpsctl(t, d.socket, "backup", "show", "bkp_nope"); code != 2 {
		t.Fatalf("不存在的备份 want exit 2, got %d: %v", code, err)
	}
	if _, code, err := runOpsctl(t, d.socket, "backup", "run", "--policy", "no-such-policy"); code != 2 {
		t.Fatalf("不存在的策略 want exit 2, got %d: %v", code, err)
	}
}

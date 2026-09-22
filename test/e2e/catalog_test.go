package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "frz-tools/api/v1"
)

func decodeArtifact(t *testing.T, raw string) v1.Artifact {
	t.Helper()
	var artifact v1.Artifact
	if err := json.Unmarshal([]byte(raw), &artifact); err != nil {
		t.Fatalf("cannot decode artifact from %q: %v", raw, err)
	}
	return artifact
}

func writeArtifactFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return path
}

func TestArtifactLifecycleThroughCLI(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	payloadDir := t.TempDir()
	path := writeArtifactFile(t, payloadDir, "app.tar.gz", "artifact-e2e-payload")

	stdout, _, err := runOpsctl(t, d.socket, "artifact", "put", path, "--json")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	artifact := decodeArtifact(t, stdout)
	if artifact.Digest == "" || artifact.Size != int64(len("artifact-e2e-payload")) {
		t.Fatalf("unexpected artifact: %+v", artifact)
	}

	// 相同内容重复上传必须复用同一条记录。
	duplicate := writeArtifactFile(t, payloadDir, "renamed.tar.gz", "artifact-e2e-payload")
	stdout, _, err = runOpsctl(t, d.socket, "artifact", "put", duplicate, "--json")
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if reused := decodeArtifact(t, stdout); reused.ID != artifact.ID {
		t.Fatalf("相同内容应复用记录：%s != %s", reused.ID, artifact.ID)
	}

	for _, ref := range []string{artifact.ID, artifact.Digest} {
		if _, _, err := runOpsctl(t, d.socket, "artifact", "inspect", ref, "--json"); err != nil {
			t.Fatalf("inspect %s: %v", ref, err)
		}
	}

	verify, _, err := runOpsctl(t, d.socket, "artifact", "verify", artifact.ID, "--json")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(verify, `"verified": true`) {
		t.Fatalf("unexpected verify response: %s", verify)
	}

	list, _, err := runOpsctl(t, d.socket, "artifact", "list", "--json")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list, artifact.ID) {
		t.Fatalf("列表中缺少制品：%s", list)
	}

	// 预演不得删除任何内容。
	gc, _, err := runOpsctl(t, d.socket, "artifact", "gc", "--keep", "10", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if !strings.Contains(gc, `"removed": []`) && !strings.Contains(gc, `"removed":[]`) {
		t.Fatalf("预演不应报告删除任何内容：%s", gc)
	}
	if _, _, err := runOpsctl(t, d.socket, "artifact", "inspect", artifact.ID); err != nil {
		t.Fatalf("预演之后制品必须还在：%v", err)
	}

	// 清理后再查询应返回未找到（退出码 2）。
	if _, _, err := runOpsctl(t, d.socket, "artifact", "delete", artifact.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, code, err := runOpsctl(t, d.socket, "artifact", "inspect", artifact.ID); err == nil {
		t.Fatal("删除后必须查不到")
	} else if code != 2 {
		t.Fatalf("ARTIFACT_NOT_FOUND 必须退出 2，got %d (%v)", code, err)
	}
}

func TestArtifactPutRejectsWrongDeclaredDigest(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	path := writeArtifactFile(t, t.TempDir(), "payload.bin", "hello")
	wrong := "sha256:" + strings.Repeat("ab", 32)

	_, code, err := runOpsctl(t, d.socket, "artifact", "put", path, "--sha256", wrong, "--json")
	if err == nil {
		t.Fatal("声明摘要不匹配必须失败")
	}
	if code != 5 {
		t.Fatalf("ARTIFACT_CHECKSUM_MISMATCH 必须退出 5，got %d (%v)", code, err)
	}
}

func TestApplicationAndReleaseThroughCLI(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	path := writeArtifactFile(t, t.TempDir(), "release.tar.gz", "release-payload")
	stdout, _, err := runOpsctl(t, d.socket, "artifact", "put", path, "--json")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	artifact := decodeArtifact(t, stdout)

	appOut, _, err := runOpsctl(t, d.socket, "app", "create", "billing-api", "--label", "env=prod", "--json")
	if err != nil {
		t.Fatalf("app create: %v", err)
	}
	var app v1.Application
	if err := json.Unmarshal([]byte(appOut), &app); err != nil {
		t.Fatalf("decode app: %v", err)
	}
	if app.ID == "" {
		t.Fatalf("unexpected application: %+v", app)
	}

	if _, _, err := runOpsctl(t, d.socket, "app", "inspect", "billing-api"); err != nil {
		t.Fatalf("按名称查询应用失败：%v", err)
	}

	releaseOut, _, err := runOpsctl(t, d.socket, "release", "add",
		"--app", "billing-api", "--artifact", artifact.ID, "--version", "1.0.0", "--json")
	if err != nil {
		t.Fatalf("release add: %v", err)
	}
	var release v1.Release
	if err := json.Unmarshal([]byte(releaseOut), &release); err != nil {
		t.Fatalf("decode release: %v", err)
	}
	if release.ApplicationID != app.ID || release.ArtifactID != artifact.ID {
		t.Fatalf("unexpected release: %+v", release)
	}

	// 同一应用重复版本 → RELEASE_CONFLICT，退出码 14。
	_, code, err := runOpsctl(t, d.socket, "release", "add",
		"--app", "billing-api", "--artifact", artifact.ID, "--version", "1.0.0")
	if err == nil {
		t.Fatal("重复版本必须失败")
	}
	if code != 14 {
		t.Fatalf("RELEASE_CONFLICT 必须退出 14，got %d (%v)", code, err)
	}

	// 引用不存在的制品 → ARTIFACT_NOT_FOUND，退出码 2。
	_, code, err = runOpsctl(t, d.socket, "release", "add",
		"--app", "billing-api", "--artifact", "art_missing", "--version", "2.0.0")
	if err == nil {
		t.Fatal("引用不存在的制品必须失败")
	}
	if code != 2 {
		t.Fatalf("ARTIFACT_NOT_FOUND 必须退出 2，got %d (%v)", code, err)
	}

	// 制品被发布引用后必须拒绝删除 → ARTIFACT_IN_USE，退出码 6。
	_, code, err = runOpsctl(t, d.socket, "artifact", "delete", artifact.ID)
	if err == nil {
		t.Fatal("被引用的制品必须拒绝删除")
	}
	if code != 6 {
		t.Fatalf("ARTIFACT_IN_USE 必须退出 6，got %d (%v)", code, err)
	}

	releases, _, err := runOpsctl(t, d.socket, "release", "list", "--app", "billing-api", "--json")
	if err != nil {
		t.Fatalf("release list: %v", err)
	}
	if !strings.Contains(releases, release.ID) {
		t.Fatalf("列表中缺少发布记录：%s", releases)
	}
}

// 凭据以 file 引用注入命令环境，日志里必须只留下脱敏占位符。
func TestSecretFileIsInjectedAndRedacted(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	const secret = "e2e-secret-value-do-not-log"
	tokenPath := filepath.Join(d.secretsDir, "deploy-token")
	if err := os.WriteFile(tokenPath, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	stdout, _, err := runOpsctl(t, d.socket, "operation", "submit",
		"--kind", v1.KindExecutorCommand,
		"--resource", "secret-demo",
		"--secret-env", "DEPLOY_TOKEN=file:"+tokenPath,
		"--json",
		"--", "/bin/sh", "-c", "printf 'token=%s' \"$DEPLOY_TOKEN\"")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	operation := decodeOperation(t, stdout)
	waitForStatus(t, d.socket, operation.ID, "succeeded")

	logs, _, err := runOpsctl(t, d.socket, "operation", "logs", operation.ID)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if strings.Contains(logs, secret) {
		t.Fatalf("日志泄漏了凭据值：%s", logs)
	}
	if !strings.Contains(logs, "[redacted]") {
		t.Fatalf("日志应当包含脱敏占位符：%s", logs)
	}
}

// 权限不对的凭据文件必须被拒绝，且操作以 SECRET_UNRESOLVED 失败。
func TestSecretFileWithLoosePermissionsIsRejected(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	tokenPath := filepath.Join(d.secretsDir, "loose-token")
	if err := os.WriteFile(tokenPath, []byte("leaky"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.Chmod(tokenPath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	stdout, _, err := runOpsctl(t, d.socket, "operation", "submit",
		"--kind", v1.KindExecutorCommand,
		"--resource", "secret-loose",
		"--secret-env", "DEPLOY_TOKEN=file:"+tokenPath,
		"--json",
		"--", "/usr/bin/true")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	operation := decodeOperation(t, stdout)

	final := waitForStatus(t, d.socket, operation.ID, "failed")
	if final.ErrorCode != string(v1.CodeSecretUnresolved) {
		t.Fatalf("want SECRET_UNRESOLVED, got %q", final.ErrorCode)
	}
}

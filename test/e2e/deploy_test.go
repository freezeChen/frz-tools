package e2e

import (
	"runtime"
	"testing"
)

// 部署的 CLI 路径。
//
// **真实的部署流程不在 e2e 里验**：它需要 RuntimeAdapter，而非 Linux 上装配层刻意不注入
// （1c 定的边界：宁可给 RUNTIME_UNSUPPORTED，也不让假适配器在生产里假装能用）。真实的
// 部署与回滚由 `make verify-linux` 的 check_deploy 覆盖——那里有真实的 systemd 与进程。
//
// 这里验的是**平台无关的那一段**：CLI → HTTP → manifest 严格解码。
func TestDeployRejectsBadManifestThroughCLI(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	if _, _, err := runOpsctl(t, d.socket, "app", "create", "orders-api"); err != nil {
		t.Fatalf("创建应用: %v", err)
	}

	// exec.argv 是相对路径且含 ..：逃出 release 目录的直接手段，提交期就该被拒。
	dir := t.TempDir()
	manifest := writeManifest(t, dir, "bad.yaml", `apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: orders-api
runtime: go
artifact:
  id: art_whatever
  fileName: bin/../../../etc/shadow
  unpack:
    strategy: none
exec:
  argv: [bin/app]
  workingDirectory: /var/lib/orders-api
  runUser: orders-api
logs:
  directory: /var/log/orders-api
health:
  readiness:
    type: tcp
    target: "127.0.0.1:8080"
`)
	_, code, _ := runOpsctl(t, d.socket, "app", "deploy", "--app", "orders-api", "--file", manifest)
	if code != 17 {
		t.Fatalf("非法的 artifact.fileName want exit 17 (MANIFEST_INVALID), got %d", code)
	}

	// 缺 --file：参数错误（退出码 2）。
	if _, code, _ := runOpsctl(t, d.socket, "app", "deploy", "--app", "orders-api"); code != 2 {
		t.Fatalf("缺 --file want exit 2, got %d", code)
	}
}

// 非 Linux 上部署明确返回 RUNTIME_UNSUPPORTED，而不是"走到一半才发现"。
//
// 这条断言同时钉住一个刻意的边界：opsd 在非 Linux 上**不注入**假适配器。
func TestDeployReportsUnsupportedWithoutRuntimeAdapter(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux 上会注入真实运行时适配器，这条断言只对非 Linux 有意义")
	}

	d := newDaemon(t)
	d.start(t)

	if _, _, err := runOpsctl(t, d.socket, "app", "create", "orders-api"); err != nil {
		t.Fatalf("创建应用: %v", err)
	}

	dir := t.TempDir()
	manifest := writeManifest(t, dir, "app.yaml", `apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: orders-api
runtime: go
artifact:
  id: art_whatever
  fileName: bin/app
exec:
  argv: [bin/app]
  workingDirectory: /var/lib/orders-api
  runUser: orders-api
logs:
  directory: /var/log/orders-api
health:
  readiness:
    type: tcp
    target: "127.0.0.1:8080"
`)
	if _, code, err := runOpsctl(t, d.socket, "app", "deploy", "--app", "orders-api", "--file", manifest); code != 21 {
		t.Fatalf("非 Linux 上 want exit 21 (RUNTIME_UNSUPPORTED), got %d: %v", code, err)
	}
}

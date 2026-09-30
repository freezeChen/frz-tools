package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// status 的一屏（迭代 6 规格 D3）：健康四项 + 最近的操作列表，
// 未完成的操作在人类可读输出里显式标出；--json 的 {health, operations}
// 字段与 API 逐字段对应。
func TestStatusSummarizesHealthAndRecentOperations(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	done := submit(t, d.socket, "status-done", "/usr/bin/true")
	sleeping := submit(t, d.socket, "status-running", "/bin/sleep", "2")

	human, _, err := runOpsctl(t, d.socket, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{"daemon:   ok", "database: ok", done.ID, sleeping.ID, "（进行中）"} {
		if !strings.Contains(human, want) {
			t.Fatalf("status 的人类可读输出里应当有 %q：\n%s", want, human)
		}
	}

	stdout, _, err := runOpsctl(t, d.socket, "status", "--json")
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}
	var snapshot struct {
		Health     v1.HealthResponse `json:"health"`
		Operations []v1.Operation    `json:"operations"`
	}
	if err := json.Unmarshal([]byte(stdout), &snapshot); err != nil {
		t.Fatalf("decode status --json: %v", err)
	}
	if snapshot.Health.Daemon != "ok" || snapshot.Health.Database != "ok" {
		t.Fatalf("health 没有按 API 的形状透传：%+v", snapshot.Health)
	}
	if len(snapshot.Operations) < 2 {
		t.Fatalf("operations 应当包含刚提交的两条，得到 %d 条", len(snapshot.Operations))
	}
	// 字段逐字段对应：找到进行中的那条，核对其身份字段。
	var seenRunning bool
	for _, op := range snapshot.Operations {
		if op.ID != sleeping.ID {
			continue
		}
		seenRunning = true
		if op.Kind != v1.KindExecutorCommand || op.Resource != "status-running" {
			t.Fatalf("列表元素应当与单条查询同构：%+v", op)
		}
		if op.Status != "running" && op.Status != "pending" {
			t.Fatalf("sleep 操作应当还在未完成状态，得到 %s", op.Status)
		}
	}
	if !seenRunning {
		t.Fatalf("operations 里没有 %s：%+v", sleeping.ID, snapshot.Operations)
	}

	// 收尾：别把仍在跑的操作留给同包后面的用例。
	waitForStatus(t, d.socket, sleeping.ID, "succeeded")
}

// version 的契约（迭代 6 规格 §6）：默认不连 daemon、退出码恒 0；
// --daemon 才连并追加对端版本，连不上显式标注、仍退出 0。
func TestVersionNeverRequiresDaemon(t *testing.T) {
	// daemon 不存在：仍然 0 退出、有输出。
	stdout, code, err := runOpsctlBinary(t, "--socket", "/nonexistent/opsd.sock", "version")
	if err != nil || code != 0 {
		t.Fatalf("version 不该因 daemon 缺席而失败：code=%d err=%v", code, err)
	}
	if !strings.HasPrefix(stdout, "opsctl:") {
		t.Fatalf("version 应当打印客户端版本行：%s", stdout)
	}

	d := newDaemon(t)
	d.start(t)

	// --daemon 对着一个活着的 opsd：两行都有，且对端不是「不可达」。
	stdout, code, err = runOpsctl(t, d.socket, "version", "--daemon")
	if err != nil || code != 0 {
		t.Fatalf("version --daemon: code=%d err=%v", code, err)
	}
	if !strings.HasPrefix(stdout, "opsctl:") || !strings.Contains(stdout, "opsd:") {
		t.Fatalf("version --daemon 应当同时给出两端版本：\n%s", stdout)
	}
	if strings.Contains(stdout, "对端不可达") {
		t.Fatalf("daemon 就在跑，不该报对端不可达：\n%s", stdout)
	}

	// --daemon 对着一个不存在的 socket：仍 0 退出，显式标注。
	stdout, code, err = runOpsctlBinary(t, "--socket", "/nonexistent/opsd.sock", "version", "--daemon")
	if err != nil || code != 0 {
		t.Fatalf("version --daemon 连不上时仍应以 0 退出：code=%d err=%v", code, err)
	}
	if !strings.Contains(stdout, "对端不可达") {
		t.Fatalf("连不上应当打一行对端不可达：\n%s", stdout)
	}
}

// init 生成即合法（迭代 6 规格 §9.1 第 1 条），覆盖保护照契约工作。
func TestInitGeneratesValidSkeleton(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "opsd.yaml")

	stdout, code, err := runOpsctlBinary(t, "init", target)
	if err != nil || code != 0 {
		t.Fatalf("init: code=%d err=%v", code, err)
	}
	if !strings.Contains(stdout, "config validate") {
		t.Fatalf("init 应当提示下一步跑 config validate：%s", stdout)
	}

	if _, code, err := runOpsctlBinary(t, "config", "validate", "--file", target); err != nil || code != 0 {
		t.Fatalf("init 生成的模板应当直接通过 config validate：%v", err)
	}

	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read generated: %v", err)
	}

	// 目标已存在且无 --force：拒绝写入，原文件一字不动。
	_, code, err = runOpsctlBinary(t, "init", target)
	if err == nil || code == 0 {
		t.Fatal("目标已存在且无 --force 时应当拒绝")
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if string(after) != string(original) {
		t.Fatal("拒绝覆盖时不该改动原文件")
	}

	// --force 才覆盖。
	if _, code, err := runOpsctlBinary(t, "init", "--force", target); err != nil || code != 0 {
		t.Fatalf("--force 应当允许覆盖：%v", err)
	}
	after, err = os.ReadFile(target)
	if err != nil {
		t.Fatalf("reread after force: %v", err)
	}
	if string(after) != string(original) {
		t.Fatal("覆盖之后应当是重新生成的模板")
	}

	// 父目录不存在：报错，而不是代为创建出一棵笔误的目录树。
	_, code, err = runOpsctlBinary(t, "init", filepath.Join(dir, "no-such-dir", "opsd.yaml"))
	if err == nil || code == 0 {
		t.Fatal("父目录不存在时应当报错")
	}
	if _, err := os.Stat(filepath.Join(dir, "no-such-dir")); !os.IsNotExist(err) {
		t.Fatal("报错的同时不该把不存在的父目录创建出来")
	}
}

// --remote 生成的文件在填入真实证书之前起不来，且失败消息逐条指向缺失的
// 证书文件（迭代 6 规格 §9.1 第 3 条的集成测试半边；容器半边在 verify.sh）。
func TestInitRemoteTemplateRefusesToStartNamingMissingCerts(t *testing.T) {
	generated := filepath.Join(t.TempDir(), "opsd-remote.yaml")
	if _, code, err := runOpsctlBinary(t, "init", "--remote", generated); err != nil || code != 0 {
		t.Fatalf("init --remote: %v", err)
	}
	body, err := os.ReadFile(generated)
	if err != nil {
		t.Fatalf("read generated: %v", err)
	}

	d := newDaemon(t)
	if err := os.WriteFile(d.configPath, body, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	output := runOpsdExpectingFailure(t, d)
	for _, placeholder := range []string{
		"/etc/opsd/pki/server.crt",
		"/etc/opsd/pki/server.key",
		"/etc/opsd/pki/ca.crt",
	} {
		if !strings.Contains(output, placeholder) {
			t.Fatalf("启动失败的消息应当逐条指向缺失的证书文件 %s：\n%s", placeholder, output)
		}
	}
}

// 远程只读身份可用（迭代 6 规格 §9.1 第 6 条）：read 档位调用 status
// （= GET /health + GET /api/v1/operations 两个只读端点）成功，写端点仍
// REMOTE_FORBIDDEN。
func TestRemoteReadScopeCanReadStatusAndOperations(t *testing.T) {
	pki := newTestPKI(t)
	_, addr := startRemoteDaemon(t, pki, "    - cn: opsctl-readonly\n      scope: read\n")

	readonlyCert, readonlyKey := pki.issueClient(t, "opsctl-readonly")
	remoteArgs := func(args ...string) []string {
		return append([]string{"--remote", addr,
			"--client-cert", readonlyCert, "--client-key", readonlyKey,
			"--ca-cert", pki.ca.certPath}, args...)
	}

	stdout, _, err := runOpsctlBinary(t, remoteArgs("status", "--json")...)
	if err != nil {
		t.Fatalf("只读身份应当能读 status：%v", err)
	}
	var snapshot struct {
		Health     v1.HealthResponse `json:"health"`
		Operations []v1.Operation    `json:"operations"`
	}
	if err := json.Unmarshal([]byte(stdout), &snapshot); err != nil {
		t.Fatalf("decode status --json: %v", err)
	}
	if snapshot.Health.Daemon != "ok" {
		t.Fatalf("health 应当按 API 的形状透传：%+v", snapshot.Health)
	}
	if snapshot.Operations == nil {
		t.Fatal("operations 应当是数组（哪怕为空），不是 null")
	}

	if _, _, err := runOpsctlBinary(t, remoteArgs("status")...); err != nil {
		t.Fatalf("只读身份的人类可读 status 也应当可用：%v", err)
	}

	_, code, err := runOpsctlBinary(t, remoteArgs("app", "create", "blocked-app")...)
	if code != 35 {
		t.Fatalf("只读身份写应当得到 exit 35 (REMOTE_FORBIDDEN)，got %d (%v)", code, err)
	}

	// version --daemon 走的也是只读端点（identity）：对只读身份应当可用。
	stdout, code, err = runOpsctlBinary(t, remoteArgs("version", "--daemon")...)
	if err != nil || code != 0 {
		t.Fatalf("version --daemon: code=%d err=%v", code, err)
	}
	if !strings.Contains(stdout, "opsd:") || strings.Contains(stdout, "对端不可达") {
		t.Fatalf("只读身份下 --daemon 应当拿到对端版本：\n%s", stdout)
	}
}

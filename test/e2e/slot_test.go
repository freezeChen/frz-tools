package e2e

import (
	"strings"
	"testing"
)

// 槽位命令的 CLI 路径（迭代 4c）。
//
// 真实的切流、对账与「线上事实」都在 `make verify-linux` 的 check_bluegreen 里验
// ——那里有真 Nginx、真 systemd、真 curl。这里验的是**平台无关的那一段**：
// 蓝绿 manifest 能被接受、两条命令挂得上、单槽应用被明确拒绝。
func TestSlotCommandsThroughCLI(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	if _, _, err := runOpsctl(t, d.socket, "app", "create", "orders-api"); err != nil {
		t.Fatalf("创建应用: %v", err)
	}

	dir := t.TempDir()
	manifest := writeManifest(t, dir, "bg.yaml", `apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: orders-api
runtime: go
artifact:
  id: art_whatever
  fileName: bin/app
  unpack:
    strategy: none
exec:
  argv: [bin/app]
  workingDirectory: /var/lib/orders-api
  runUser: orders-api
  slots:
    blue:
      ports: [18081]
      readiness:
        type: tcp
        target: "127.0.0.1:18081"
        consecutiveSuccesses: 1
    green:
      ports: [18082]
      readiness:
        type: tcp
        target: "127.0.0.1:18082"
        consecutiveSuccesses: 1
health:
  startTimeoutSeconds: 30
logs:
  directory: /var/log/orders-api
nginx:
  listen: 8080
`)
	if _, _, err := runOpsctl(t, d.socket, "spec", "put", "--app", "orders-api", "--file", manifest); err != nil {
		t.Fatalf("提交蓝绿 manifest: %v", err)
	}

	stdout, code, err := runOpsctl(t, d.socket, "app", "slot", "list", "--app", "orders-api")
	if err != nil || code != 0 {
		t.Fatalf("slot list 应当成功：exit=%d err=%v\n%s", code, err, stdout)
	}
	// 两侧都要出现（**没部署过的那一侧也要**）：输出形状不该随部署过几次变化；
	// 而「还没部署过」与「部署了但没起来」是两件事，得看得出来是前者。
	//
	// 刻意**不**断言「线上事实读不到」那句说明：非 Linux 上没有 Nginx 适配器，而
	// CI 的 Linux runner 上有（受管文件还不存在，读出来是空槽位）。那是平台差异，
	// 两种形态下都成立的是下面这几条。
	for _, want := range []string{
		"槽位 blue", "槽位 green", "还没部署过",
		"orders-api-blue.service", "orders-api-green.service",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("slot list 的输出里应当有 %q：\n%s", want, stdout)
		}
	}

	// 时间线在还没切过流时是空的——这是**正常结果**，不是错误。
	stdout, code, err = runOpsctl(t, d.socket, "app", "slot", "history", "--app", "orders-api")
	if err != nil || code != 0 {
		t.Fatalf("slot history 应当成功：exit=%d err=%v\n%s", code, err, stdout)
	}
	if !strings.Contains(stdout, "还没有槽位时间线") {
		t.Fatalf("从没切过流时应当说清楚，而不是打印一张空表：\n%s", stdout)
	}

	// 单槽应用没有槽位：明确报错（exit 2 = INVALID_REQUEST），而不是渲染一份两侧都空的行。
	if _, _, err := runOpsctl(t, d.socket, "app", "create", "billing-api"); err != nil {
		t.Fatalf("创建单槽应用: %v", err)
	}
	single := writeManifest(t, dir, "single.yaml", `apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: billing-api
runtime: go
artifact:
  id: art_whatever
  fileName: bin/app
  unpack:
    strategy: none
exec:
  argv: [bin/app]
  workingDirectory: /var/lib/billing-api
  runUser: billing-api
  ports: [8080]
health:
  readiness:
    type: tcp
    target: "127.0.0.1:8080"
logs:
  directory: /var/log/billing-api
`)
	if _, _, err := runOpsctl(t, d.socket, "spec", "put", "--app", "billing-api", "--file", single); err != nil {
		t.Fatalf("提交单槽 manifest: %v", err)
	}
	for _, sub := range []string{"list", "history"} {
		if _, code, _ := runOpsctl(t, d.socket, "app", "slot", sub, "--app", "billing-api"); code != 2 {
			t.Errorf("单槽应用上的 slot %s 应当以 exit 2 (INVALID_REQUEST) 拒绝，got %d", sub, code)
		}
	}
}

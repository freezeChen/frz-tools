package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// writeKey 造一个私钥文件（内容无关紧要：校验只看模式与属主）。
func writeKey(t *testing.T, dir string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, "server.key")
	if err := os.WriteFile(path, []byte("not-a-real-key"), mode); err != nil {
		t.Fatalf("write key: %v", err)
	}
	// os.WriteFile 受 umask 影响，显式再设一次，让用例断言的是我们写的那个模式。
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod key: %v", err)
	}
	return path
}

func remoteYAML(t *testing.T, body string) string {
	t.Helper()
	return validYAML + "\n" + body
}

func TestRemoteSectionAbsentKeepsListeningLocal(t *testing.T) {
	cfg, err := LoadBytes([]byte(validYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RemoteEnabled() {
		t.Fatal("没有 remote 段就不该开远程监听")
	}
}

func TestRemoteRequiresEveryCertificateField(t *testing.T) {
	// listen 给了，三个证书字段一个都没给：一次把三条都报出来，
	// 而不是改一条重启一次。
	_, err := LoadBytes([]byte(remoteYAML(t, `
remote:
  listen: "0.0.0.0:9443"
  clients:
    - cn: opsctl-central
      scope: write
`)))
	if domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("want CONFIG_INVALID, got %v", err)
	}
	for _, field := range []string{"remote.certFile", "remote.keyFile", "remote.clientCAFile"} {
		if !strings.Contains(domain.MessageOf(err), field) {
			t.Fatalf("报错里应当提到 %s：%v", field, err)
		}
	}
}

func TestRemoteFieldsWithoutListenAreRejected(t *testing.T) {
	// 配了证书却忘了 listen：这是最容易误读的状态——运维以为开了远程，其实没有。
	_, err := LoadBytes([]byte(remoteYAML(t, `
remote:
  certFile: /etc/opsd/pki/server.crt
`)))
	if domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("want CONFIG_INVALID, got %v", err)
	}
	if !strings.Contains(domain.MessageOf(err), "remote.listen 为空") {
		t.Fatalf("报错应当说清是「不听远程时别的字段都不许给」：%v", err)
	}
}

func TestRemoteListenMustBeHostPort(t *testing.T) {
	_, err := LoadBytes([]byte(remoteYAML(t, `
remote:
  listen: "9443"
`)))
	if domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("want CONFIG_INVALID, got %v", err)
	}
	if !strings.Contains(domain.MessageOf(err), "host:port") {
		t.Fatalf("报错应当说清地址的形态：%v", err)
	}
}

func TestRemoteRejectsDuplicateClientCn(t *testing.T) {
	// 同一个 CN 两条规则会让「以哪条为准」变成一个必须记住的规则。
	_, err := LoadBytes([]byte(remoteYAML(t, `
remote:
  listen: "0.0.0.0:9443"
  clients:
    - cn: opsctl-central
      scope: write
    - cn: opsctl-central
      scope: read
`)))
	if domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("want CONFIG_INVALID, got %v", err)
	}
	if !strings.Contains(domain.MessageOf(err), "重复") {
		t.Fatalf("报错应当指出 CN 重复：%v", err)
	}
}

func TestRemoteRejectsUnknownScope(t *testing.T) {
	_, err := LoadBytes([]byte(remoteYAML(t, `
remote:
  listen: "0.0.0.0:9443"
  clients:
    - cn: opsctl-central
      scope: admin
`)))
	if domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("want CONFIG_INVALID, got %v", err)
	}
	if !strings.Contains(domain.MessageOf(err), "admin") {
		t.Fatalf("报错应当回显那个取值：%v", err)
	}
}

func TestRemoteRejectsEmptyClientList(t *testing.T) {
	// 空名单等于谁都进不来；要关远程应当去掉 listen，而不是留一段开着的端口。
	_, err := LoadBytes([]byte(remoteYAML(t, `
remote:
  listen: "0.0.0.0:9443"
  clients: []
`)))
	if domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("want CONFIG_INVALID, got %v", err)
	}
	if !strings.Contains(domain.MessageOf(err), "白名单") {
		t.Fatalf("报错应当说清这是白名单：%v", err)
	}
}

// 私钥模式过宽是**拒绝启动**，不是警告：0644 的私钥等于把机器身份公开了，
// 而它不会自己变回去。
func TestRemoteRejectsWorldReadablePrivateKey(t *testing.T) {
	dir := t.TempDir()
	key := writeKey(t, dir, 0o644)

	_, err := LoadBytes([]byte(remoteYAML(t, `
remote:
  listen: "0.0.0.0:9443"
  certFile: /etc/opsd/pki/server.crt
  keyFile: `+key+`
  clientCAFile: /etc/opsd/pki/ca.crt
  clients:
    - cn: opsctl-central
      scope: write
`)))
	if domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("want CONFIG_INVALID, got %v", err)
	}
	message := domain.MessageOf(err)
	if !strings.Contains(message, key) || !strings.Contains(message, "chmod 600") {
		t.Fatalf("报错应当指出是哪个文件、该怎么办：%v", err)
	}
}

func TestRemoteAcceptsValidSection(t *testing.T) {
	dir := t.TempDir()
	key := writeKey(t, dir, 0o600)

	cfg, err := LoadBytes([]byte(remoteYAML(t, `
remote:
  listen: "0.0.0.0:9443"
  certFile: /etc/opsd/pki/server.crt
  keyFile: `+key+`
  clientCAFile: /etc/opsd/pki/ca.crt
  clients:
    - cn: opsctl-central
      scope: write
      applications:
        - orders-api
    - cn: opsctl-readonly
      scope: read
`)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.RemoteEnabled() {
		t.Fatal("配了 listen 就应当开远程监听")
	}

	central, ok := cfg.FindRemoteIdentity("opsctl-central")
	if !ok {
		t.Fatal("opsctl-central 应当在名单里")
	}
	if central.Scope != domain.ScopeWrite {
		t.Fatalf("want write, got %q", central.Scope)
	}
	if len(central.Applications) != 1 || central.Applications[0] != "orders-api" {
		t.Fatalf("应用白名单没读对：%+v", central.Applications)
	}

	// 省略 applications 表示「这台机上的全部应用」，而不是「一个都不能碰」。
	readonly, ok := cfg.FindRemoteIdentity("opsctl-readonly")
	if !ok {
		t.Fatal("opsctl-readonly 应当在名单里")
	}
	if readonly.Scope != domain.ScopeRead {
		t.Fatalf("want read, got %q", readonly.Scope)
	}
	if len(readonly.Applications) != 0 {
		t.Fatalf("省略 applications 应当表示全部，得到 %+v", readonly.Applications)
	}

	if _, ok := cfg.FindRemoteIdentity("someone-else"); ok {
		t.Fatal("不在名单里的 CN 不该被认出来")
	}
}

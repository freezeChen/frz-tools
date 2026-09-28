package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// 测试用 PKI
//
// 证书在这里现签，而不是把几份 PEM 签进仓库：签进去的证书会过期，而一个「过两年
// 自己坏掉」的测试比没有测试更糟。工具本身不签发证书（迭代 5 规格 D10），因此
// 这些函数只存在于测试里。
// ---------------------------------------------------------------------------

type testCA struct {
	cert     *x509.Certificate
	key      *ecdsa.PrivateKey
	certPath string
}

type testPKI struct {
	// ca 是服务端与客户端共同信任的那个 CA。
	ca *testCA
	// serverCert/serverKey 是 opsd 的身份。
	serverCert, serverKey string
	// clientCert/clientKey 是默认客户端（CN=opsctl-central）的身份。
	clientCert, clientKey string
	// otherCA 是**另一个** CA，用来验「不是我们签的一律拒」。
	otherCA *testCA
}

func newTestCA(t *testing.T, dir, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ca key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	certPath := filepath.Join(dir, name+".crt")
	writeCertPEM(t, certPath, der)
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca cert: %v", err)
	}
	return &testCA{cert: cert, key: key, certPath: certPath}
}

// issue 签一张叶子证书。serverNames 非空时是服务端证书（带 SAN）。
func (ca *testCA) issue(t *testing.T, dir, cn string, serverNames []string, usages []x509.ExtKeyUsage) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  usages,
	}
	for _, name := range serverNames {
		// SAN 必须包含运维用来连它的那个名字，否则客户端一侧的校验会（正确地）拒绝。
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	certPath := filepath.Join(dir, cn+".crt")
	keyPath := filepath.Join(dir, cn+".key")
	writeCertPEM(t, certPath, der)

	encoded, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	// 0600 是硬要求：opsd 与 opsctl 都会拒绝更宽的私钥文件。
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

func writeCertPEM(t *testing.T, path string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()
	ca := newTestCA(t, dir, "frz-test-ca")
	serverCert, serverKey := ca.issue(t, dir, "server", []string{"127.0.0.1", "localhost"},
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	clientCert, clientKey := ca.issue(t, dir, "opsctl-central", nil,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	return &testPKI{
		ca:         ca,
		serverCert: serverCert,
		serverKey:  serverKey,
		clientCert: clientCert,
		clientKey:  clientKey,
		otherCA:    newTestCA(t, dir, "another-ca"),
	}
}

// issueClient 另外签一张客户端证书，用于「CN 不在名单里」这类用例。
func (p *testPKI) issueClient(t *testing.T, cn string) (string, string) {
	t.Helper()
	return p.ca.issue(t, t.TempDir(), cn, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
}

// freePort 拿一个空闲端口。它当然有竞态，但在这个规模的测试里够用，
// 而把端口写死会在并行跑测试时互相撞。
func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close()
	return addr
}

// remoteSection 造 opsd 配置里的 remote 段。clients 是名单条目的 YAML 片段。
func remoteSection(addr string, pki *testPKI, clients string) string {
	return fmt.Sprintf(`remote:
  listen: %q
  certFile: %s
  keyFile: %s
  clientCAFile: %s
  clients:
%s`, addr, pki.serverCert, pki.serverKey, pki.ca.certPath, clients)
}

func countOperations(t *testing.T, database, kind string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+database)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM operations WHERE kind = ?`, kind).Scan(&count); err != nil {
		t.Fatalf("count operations: %v", err)
	}
	return count
}

// auditActor 读一条操作的 operation.created 审计，同时取回 details。
func auditActor(t *testing.T, database, operationID string) (string, string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+database)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()
	var actor string
	var details sql.NullString
	if err := db.QueryRow(`SELECT actor, details_json FROM audit_events
		WHERE event_type = 'operation.created' AND operation_id = ?`, operationID).
		Scan(&actor, &details); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	return actor, details.String
}

// startRemoteDaemon 起一个开了 mTLS 监听的守护进程。
func startRemoteDaemon(t *testing.T, pki *testPKI, clients string) (*daemon, string) {
	t.Helper()
	d := newDaemon(t)
	addr := freePort(t)
	d.setRemote(t, remoteSection(addr, pki, clients))
	d.start(t)
	return d, addr
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

// 默认形态：没有 remote 段就**完全不听 TCP**。这条断言的做法是——同一个端口，
// 有 remote 段时连得上；把 remote 段去掉之后，同一个端口连不上了。
//
// 这是「默认行为与迭代 4 逐字节一致」那一条的证据：能力是显式声明的，
// 不会因为升级而悄悄多开一个端口。
func TestRemoteListenerIsOffByDefault(t *testing.T) {
	pki := newTestPKI(t)
	addr := freePort(t)

	withRemote := newDaemon(t)
	withRemote.setRemote(t, remoteSection(addr, pki, "    - cn: opsctl-central\n      scope: write\n"))
	withRemote.start(t)

	stdout, _, err := runOpsctlRemote(t, addr, pki, "identity", "--json")
	if err != nil {
		t.Fatalf("开了 remote 段就该连得上：%v", err)
	}
	var identity v1.IdentityResponse
	if err := json.Unmarshal([]byte(stdout), &identity); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	if identity.Backend != "tls" {
		t.Fatalf("backend 应当是 tls，得到 %q", identity.Backend)
	}
	withRemote.stop(t)

	// 去掉 remote 段再起一个：这个端口不该有人听。
	withoutRemote := newDaemon(t)
	withoutRemote.start(t)

	if _, _, err := runOpsctlRemote(t, addr, pki, "identity"); err == nil {
		t.Fatal("没配 remote 段就不该有任何 TCP 监听：居然连上了")
	} else if !strings.Contains(err.Error(), "exit code 33") {
		t.Fatalf("连不上应当是 HOST_UNREACHABLE(33)，得到 %v", err)
	}

	// 本机那条路照旧：形态与之前一模一样。
	stdout, _, err = runOpsctl(t, withoutRemote.socket, "identity", "--json")
	if err != nil {
		t.Fatalf("socket 那条路应当照常可用：%v", err)
	}
	if err := json.Unmarshal([]byte(stdout), &identity); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	if identity.Backend != "unix" || identity.Scope != "write" || identity.ClientCn != "" {
		t.Fatalf("socket 身份不对：%+v", identity)
	}
}

// 名单里的身份能连上，而且 /identity 报出来的三个字段——backend、clientCn、scope
// ——正好对应远程链路上最容易错的三处。
func TestRemoteIdentityReportsWhoAndHow(t *testing.T) {
	pki := newTestPKI(t)
	_, addr := startRemoteDaemon(t, pki, `    - cn: opsctl-central
      scope: write
      applications:
        - orders-api
`)

	stdout, _, err := runOpsctlRemote(t, addr, pki, "identity", "--json")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	var identity v1.IdentityResponse
	if err := json.Unmarshal([]byte(stdout), &identity); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	if identity.Backend != "tls" {
		t.Fatalf("backend 应当是 tls，得到 %q", identity.Backend)
	}
	if identity.ClientCn != "opsctl-central" {
		t.Fatalf("clientCn 应当是证书 CN，得到 %q", identity.ClientCn)
	}
	if identity.Scope != "write" {
		t.Fatalf("scope 应当与配置一致，得到 %q", identity.Scope)
	}
	if len(identity.ApplicationsAllowed) != 1 || identity.ApplicationsAllowed[0] != "orders-api" {
		t.Fatalf("应用白名单没报出来：%+v", identity.ApplicationsAllowed)
	}
	if identity.Hostname == "" {
		t.Fatal("hostname 不该为空：它是「我打到的是哪台机」的答案")
	}
	// 端口必须出现在给人看的输出里，否则「打到了哪台」还是看不出来。
	human, _, err := runOpsctlRemote(t, addr, pki, "identity")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if !strings.Contains(human, addr) {
		t.Fatalf("人类可读输出里应当带上地址：%s", human)
	}
}

// 没带客户端证书：TLS 握不上，报 HOST_TLS_FAILED(34)，而且要说清是哪一种失败。
func TestRemoteWithoutClientCertificateFailsAtTLS(t *testing.T) {
	pki := newTestPKI(t)
	_, addr := startRemoteDaemon(t, pki, "    - cn: opsctl-central\n      scope: write\n")

	// 只带 CA，不带客户端证书与私钥。
	_, code, err := runOpsctlBinary(t, "--remote", addr, "--ca-cert", pki.ca.certPath, "identity")
	if code != 34 {
		t.Fatalf("want exit 34 (HOST_TLS_FAILED), got %d (%v)", code, err)
	}
	if err == nil || !strings.Contains(err.Error(), "客户端证书") {
		t.Fatalf("报错应当说清是缺客户端证书：%v", err)
	}
}

// 另一个 CA 签的客户端证书一律拒——握手就过不去，报 HOST_TLS_FAILED。
func TestRemoteRejectsClientFromAnotherCA(t *testing.T) {
	pki := newTestPKI(t)
	_, addr := startRemoteDaemon(t, pki, "    - cn: opsctl-central\n      scope: write\n")

	foreignCert, foreignKey := pki.otherCA.issue(t, t.TempDir(), "opsctl-central", nil,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})

	// CN 一模一样，但签发者不是服务端信的那个 CA：这正是「证书是唯一能自证的东西」
	// 要防的那件事——名字可以随便写，签名不行。
	_, code, err := runOpsctlBinary(t, "--remote", addr,
		"--client-cert", foreignCert, "--client-key", foreignKey, "--ca-cert", pki.ca.certPath, "identity")
	if code != 34 {
		t.Fatalf("want exit 34 (HOST_TLS_FAILED), got %d (%v)", code, err)
	}
}

// 本 CA 签的、但 CN 不在名单里：握手过得了，授权不给——REMOTE_FORBIDDEN(35)。
//
// 与上一条刻意分开：两者在客户端看来是不同的错误，运维要改的也是不同的地方
// （一个去查证书签发，一个去改 clients 名单）。
func TestRemoteRejectsClientCnOutsideAllowlist(t *testing.T) {
	pki := newTestPKI(t)
	_, addr := startRemoteDaemon(t, pki, "    - cn: opsctl-central\n      scope: write\n")

	strangerCert, strangerKey := pki.issueClient(t, "stranger")
	_, code, err := runOpsctlBinary(t, "--remote", addr,
		"--client-cert", strangerCert, "--client-key", strangerKey, "--ca-cert", pki.ca.certPath, "identity")
	if code != 35 {
		t.Fatalf("want exit 35 (REMOTE_FORBIDDEN), got %d (%v)", code, err)
	}
	if err == nil || !strings.Contains(err.Error(), "stranger") {
		t.Fatalf("报错应当回显那个 CN，便于运维去改名单：%v", err)
	}
}

// 客户端也要校验服务端的证书（防中间人）：拿一份不信任的 CA 去连，拒绝。
func TestRemoteClientVerifiesServerCertificate(t *testing.T) {
	pki := newTestPKI(t)
	_, addr := startRemoteDaemon(t, pki, "    - cn: opsctl-central\n      scope: write\n")

	_, code, err := runOpsctlBinary(t, "--remote", addr,
		"--client-cert", pki.clientCert, "--client-key", pki.clientKey,
		"--ca-cert", pki.otherCA.certPath, "identity")
	if code != 34 {
		t.Fatalf("want exit 34 (HOST_TLS_FAILED), got %d (%v)", code, err)
	}
	if err == nil || !strings.Contains(err.Error(), "不被信任") {
		t.Fatalf("报错应当指向「对端证书不被信任」：%v", err)
	}
}

// 只读档位：读端点通，写端点拒。档位是 5a 唯一的「粗细」之分，它必须真的生效。
func TestRemoteReadScopeCanReadButNotWrite(t *testing.T) {
	pki := newTestPKI(t)
	_, addr := startRemoteDaemon(t, pki, "    - cn: opsctl-readonly\n      scope: read\n")

	readonlyCert, readonlyKey := pki.issueClient(t, "opsctl-readonly")
	remoteArgs := func(args ...string) []string {
		return append([]string{"--remote", addr,
			"--client-cert", readonlyCert, "--client-key", readonlyKey,
			"--ca-cert", pki.ca.certPath}, args...)
	}

	// 读：通。
	if _, _, err := runOpsctlBinary(t, remoteArgs("identity", "--json")...); err != nil {
		t.Fatalf("只读身份应当能读：%v", err)
	}
	if _, _, err := runOpsctlBinary(t, remoteArgs("operation", "get", "op_missing")...); err == nil {
		t.Fatal("查一个不存在的操作应当报 404，而不是成功")
	}

	// 写：拒，而且是 REMOTE_FORBIDDEN。
	_, code, err := runOpsctlBinary(t, remoteArgs("app", "create", "orders-api")...)
	if code != 35 {
		t.Fatalf("只读身份写应当得到 exit 35 (REMOTE_FORBIDDEN)，got %d (%v)", code, err)
	}
	if err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("报错应当说清是档位不够：%v", err)
	}
}

// 远程的写操作落在**目标机**的库上，审计里的 actor 是证书 CN 而不是自报的名字。
//
// 这条是这一片的核心断言：它同时证明了三件事——请求真的打到了另一台机上、
// 那边真的建了操作、而「谁做的」记的是证书而不是请求体。
func TestRemoteWriteLandsOnTargetHostWithCertifiedActor(t *testing.T) {
	pki := newTestPKI(t)

	// 发起方：一台与本机库完全无关的守护进程，用来证明操作没有落在它这里。
	initiator := newDaemon(t)
	initiator.start(t)

	target, addr := startRemoteDaemon(t, pki, "    - cn: opsctl-central\n      scope: write\n")

	// 目标机上先有一份备份策略（本机动作，不经远程）。
	sourceDir := t.TempDir()
	manifest := writeManifest(t, sourceDir, "policy.yaml", backupPolicyManifest("remote-orders", sourceDir))
	if _, _, err := runOpsctl(t, target.socket, "backup", "policy", "put", "--file", manifest); err != nil {
		t.Fatalf("policy put: %v", err)
	}

	// 经远程提交一次备份。createdBy 故意自报一个假名字。
	stdout, _, err := runOpsctlBinary(t, "--remote", addr,
		"--client-cert", pki.clientCert, "--client-key", pki.clientKey, "--ca-cert", pki.ca.certPath,
		"backup", "run", "--policy", "remote-orders", "--created-by", "alice", "--json")
	if err != nil {
		t.Fatalf("远程备份: %v", err)
	}
	var op v1.Operation
	if err := json.Unmarshal([]byte(stdout), &op); err != nil {
		t.Fatalf("decode operation: %v", err)
	}

	// 落在目标机上。
	if count := countOperations(t, target.database, v1.KindBackupRun); count != 1 {
		t.Fatalf("目标机库里应当有 1 条 backup.run，得到 %d", count)
	}
	// 没有落在发起方这里。
	if count := countOperations(t, initiator.database, v1.KindBackupRun); count != 0 {
		t.Fatalf("发起方库里不该有 backup.run，得到 %d", count)
	}

	// 审计记的是证书 CN，自报的名字只作为线索留在 details 里。
	actor, details := auditActor(t, target.database, op.ID)
	if actor != "remote:opsctl-central" {
		t.Fatalf("审计的 actor 应当是证书 CN，得到 %q", actor)
	}
	if !strings.Contains(details, `"claimedBy":"alice"`) {
		t.Fatalf("details 里应当留下自报的身份，得到 %q", details)
	}
}

// D5：远程禁止 executor.command。它是**刻意的能力缺口**，因此报错要说清楚去哪儿。
func TestRemoteForbidsExecutorCommand(t *testing.T) {
	pki := newTestPKI(t)
	target, addr := startRemoteDaemon(t, pki, "    - cn: opsctl-central\n      scope: write\n")

	_, code, err := runOpsctlBinary(t, "--remote", addr,
		"--client-cert", pki.clientCert, "--client-key", pki.clientKey, "--ca-cert", pki.ca.certPath,
		"operation", "submit", "--kind", v1.KindExecutorCommand, "--resource", "remote-exec", "--", "/bin/echo", "hi")
	if code != 35 {
		t.Fatalf("want exit 35 (REMOTE_FORBIDDEN), got %d (%v)", code, err)
	}
	if err == nil || !strings.Contains(err.Error(), "executor.command") {
		t.Fatalf("报错应当点明被禁的是哪个 kind：%v", err)
	}
	if count := countOperations(t, target.database, v1.KindExecutorCommand); count != 0 {
		t.Fatalf("被拒的请求不该留下操作，得到 %d 条", count)
	}
}

// host check 与 host list --check：本机库里有地址的那些记录会被真的拨号。
//
// 探活结果**不落库**是这条用例的另一半：跑完之后记录必须一字未改。
func TestHostCheckAndListCheckDoNotWriteAnything(t *testing.T) {
	pki := newTestPKI(t)

	// 目标机：开了远程监听。
	_, addr := startRemoteDaemon(t, pki, "    - cn: opsctl-central\n      scope: write\n")

	// 发起方：本机进程，库里登记两条记录——目标机，以及一台连不上的。
	initiator := newDaemon(t)
	initiator.start(t)
	if _, _, err := runOpsctl(t, initiator.socket, "host", "create", "web-1", "--address", addr); err != nil {
		t.Fatalf("create host: %v", err)
	}
	// 127.0.0.1 上一个几乎不可能有人听的端口：探活必须如实报「不可达」，
	// 而不是把这种失败吞掉。
	deadAddr := freePort(t)
	if _, _, err := runOpsctl(t, initiator.socket, "host", "create", "web-dead", "--address", deadAddr); err != nil {
		t.Fatalf("create host: %v", err)
	}

	before, _, err := runOpsctl(t, initiator.socket, "host", "inspect", "web-1", "--json")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}

	// host check：一次只问一台。
	//
	// 拨号用的身份三件套要显式给：--host 只解决「地址从哪儿来」，证书仍然在运维
	// 手上（迭代 5 规格 D7：工具只消费证书，不签发也不代管）。
	checked, _, err := runOpsctlBinary(t, append([]string{
		"--socket", initiator.socket,
		"--client-cert", pki.clientCert, "--client-key", pki.clientKey, "--ca-cert", pki.ca.certPath,
	}, "host", "check", "web-1")...)
	if err != nil {
		t.Fatalf("host check: %v", err)
	}
	if !strings.Contains(checked, "tls") || !strings.Contains(checked, "opsctl-central") {
		t.Fatalf("host check 应当报出 backend 与身份：%s", checked)
	}

	// host list --check：逐台探活 + 汇总。
	summary, code, err := runOpsctlBinary(t, append([]string{
		"--socket", initiator.socket,
		"--client-cert", pki.clientCert, "--client-key", pki.clientKey, "--ca-cert", pki.ca.certPath,
	}, "host", "list", "--check")...)
	if err == nil || code != 33 {
		t.Fatalf("有一台不可达时应当以 33 退出，得到 %d (%v)", code, err)
	}
	// 汇总里必须把**本机那条记录**也算上：它是 address 为空的那一条，探活走的是
	// 当前目标的 socket，因此报出来的是 backend=unix。
	if !strings.Contains(summary, "2 台可达，1 台不可达") {
		t.Fatalf("汇总行不对：%s", summary)
	}
	if !strings.Contains(summary, "（本机）") || !strings.Contains(summary, "（本机 socket，无证书身份）") {
		t.Fatalf("本机那条记录应当按本机来报：%s", summary)
	}
	if !strings.Contains(summary, "web-dead") || !strings.Contains(summary, "连不上") {
		t.Fatalf("不可达的那台应当给出原因：%s", summary)
	}

	// 不落库：记录一字未改。
	after, _, err := runOpsctl(t, initiator.socket, "host", "inspect", "web-1", "--json")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if before != after {
		t.Fatalf("探活不该改动主机记录：\n之前 %s\n之后 %s", before, after)
	}
}

// --host 从本机库读地址，--remote 直接给地址；两者与 --socket 互斥。
func TestRemoteTargetFlagsAreMutuallyExclusive(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	for _, args := range [][]string{
		{"--socket", d.socket, "--remote", "127.0.0.1:1", "identity"},
		{"--socket", d.socket, "--host", "local", "identity"},
		{"--host", "local", "--remote", "127.0.0.1:1", "identity"},
	} {
		_, code, err := runOpsctlBinary(t, args...)
		if code != 2 {
			t.Fatalf("%v want exit 2 (INVALID_REQUEST), got %d (%v)", args, code, err)
		}
		if err == nil || !strings.Contains(err.Error(), "互斥") {
			t.Fatalf("%v 的报错应当说清互斥：%v", args, err)
		}
	}

	// --host 指向 address 为空的本机记录时也要说清楚该怎么办。
	_, code, err := runOpsctl(t, d.socket, "--host", "local", "identity")
	if code != 2 {
		t.Fatalf("--host 指向本机记录 want exit 2, got %d (%v)", code, err)
	}
	if err == nil || !strings.Contains(err.Error(), "本机") {
		t.Fatalf("报错应当说清那条记录就是本机：%v", err)
	}
}

// 私钥模式过宽时 opsd **拒绝启动**，并在报错里指出是哪个文件。
func TestOpsdRefusesToStartWithWorldReadableKey(t *testing.T) {
	pki := newTestPKI(t)
	addr := freePort(t)

	// 把服务端私钥放宽——这正是「一份 0644 的私钥等于把机器身份公开」那条。
	if err := os.Chmod(pki.serverKey, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	d := newDaemon(t)
	d.setRemote(t, remoteSection(addr, pki, "    - cn: opsctl-central\n      scope: write\n"))

	output := runOpsdExpectingFailure(t, d)

	if !strings.Contains(output, pki.serverKey) {
		t.Fatalf("报错应当指出是哪个文件：%s", output)
	}
	if !strings.Contains(output, "chmod 600") {
		t.Fatalf("报错应当说清该怎么办：%s", output)
	}
	if !strings.Contains(output, "CONFIG_INVALID") {
		t.Fatalf("应当是配置错误：%s", output)
	}
}

// lastIdempotencyKey 读回最近一条 app.deploy 操作的幂等键。
//
// 直接查库而不是走 API：`operation get` 需要先知道操作 id，而这里要验的正是
// 「批次号有没有被拼成那把键」。
func (f *targetFixture) lastIdempotencyKey(t *testing.T) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+f.targets["web-1"].daemon.database)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()
	var key sql.NullString
	if err := db.QueryRow(`SELECT idempotency_key FROM operations
		WHERE kind = 'app.deploy' ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&key); err != nil {
		t.Fatalf("read idempotency key: %v", err)
	}
	return key.String
}

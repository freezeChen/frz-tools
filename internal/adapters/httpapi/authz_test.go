package httpapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/backup/files"
	"github.com/freezeChen/frz-tools/internal/adapters/blob"
	"github.com/freezeChen/frz-tools/internal/adapters/executor"
	"github.com/freezeChen/frz-tools/internal/adapters/sqlite"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// authzFixture 是一个装配了远程身份白名单的 Server，外加它的库文件路径——
// 「拒绝发生在动手之前」这条断言要靠直接查库来证明，光看状态码证明不了。
type authzFixture struct {
	server *Server
	store  *sqlite.Store
	dbPath string
}

// staticIdentities 是测试用的白名单。真实的白名单来自配置，这里用一张固定表——
// 中间件只看 FindRemoteIdentity 这一个方法，因此这里换成什么都一样。
type staticIdentities map[string]domain.RemoteIdentity

func (s staticIdentities) FindRemoteIdentity(cn string) (domain.RemoteIdentity, bool) {
	identity, ok := s[cn]
	return identity, ok
}

func newAuthzFixture(t *testing.T, identities RemoteIdentityLookup) *authzFixture {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opsd.db")
	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	backupStore, err := blob.NewLocal(filepath.Join(dir, "backups"), 0o640, 0o750)
	if err != nil {
		t.Fatalf("backup store: %v", err)
	}

	allow := func(string) bool { return true }
	runtime := application.NewRuntime(application.Options{
		Repo:            store,
		Executor:        executor.New(allow, 5*time.Second, 4096, nil),
		AllowExecutable: allow,
		Defaults:        application.Defaults{Timeout: 5 * time.Second, MaxOutputBytes: 4096},
		// 备份是唯一一条**所有平台**都能创建出 Operation 的写路径（files 适配器
		// 不需要 systemd）。远程写操作要落到库里、审计要记对人，都得有一条真能
		// 建出 Operation 的路。
		BackupStore:    backupStore,
		BackupAdapters: []application.BackupAdapter{files.New(files.WithRoot(dir))},
		Workers:        1,
		Idle:           10 * time.Millisecond,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	return &authzFixture{
		store:  store,
		dbPath: dbPath,
		server: NewServer(Dependencies{
			Service:          runtime.Service,
			Catalogs:         runtime.Catalogs,
			Specs:            runtime.Specs,
			Hosts:            runtime.Hosts,
			Targets:          runtime.Targets,
			Backups:          runtime.Backups,
			Store:            store,
			Workers:          1,
			Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
			RemoteIdentities: identities,
		}),
	}
}

// remoteTLS 造一份「已验证的客户端证书」的连接状态。中间件只读 CN，因此这里
// 不需要真证书——握手本身由 e2e 用真 TLS 覆盖。
func remoteTLS(cn string) *tls.ConnectionState {
	return &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{Subject: pkix.Name{CommonName: cn}}},
	}
}

func (f *authzFixture) call(t *testing.T, method, path, body string, state *tls.ConnectionState) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.TLS = state
	recorder := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func (f *authzFixture) callRemote(t *testing.T, cn, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, method, path, body, remoteTLS(cn))
}

func decodeErrorResponse(t *testing.T, recorder *httptest.ResponseRecorder) v1.ErrorResponse {
	t.Helper()
	var decoded v1.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode error response from %q: %v", recorder.Body.String(), err)
	}
	return decoded
}

// countRows 直接查库。用它而不是通过 API 断言「有没有真的动手」，是因为
// 「先建了操作再失败」这种情况在 HTTP 层看起来和「什么都没做」一样。
func (f *authzFixture) countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return count
}

func (f *authzFixture) createApplication(t *testing.T, name string) {
	t.Helper()
	recorder := f.call(t, http.MethodPost, "/api/v1/applications",
		`{"name":"`+name+`"}`, nil)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create application %s: %d %s", name, recorder.Code, recorder.Body.String())
	}
}

// putBackupPolicy 造一份合法的备份策略。远程写操作的用例需要一条**真能建出
// Operation** 的路径：executor.command 被 D5 禁掉了，runtime.*/deploy 在非 Linux
// 上没有适配器，只有备份在所有平台都走得通。
func (f *authzFixture) putBackupPolicy(t *testing.T, name, sourceDir string) {
	t.Helper()
	manifest := `apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: ` + name + `
resource:
  kind: files
  paths:
    - ` + sourceDir + `
encoding:
  encryption:
    enabled: false
retention:
  keepLast: 3
`
	body, err := json.Marshal(v1.PutBackupPolicyRequest{Manifest: manifest, UpdatedBy: "local"})
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	recorder := f.call(t, http.MethodPut, "/api/v1/backup-policies/"+name, string(body), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("put policy: %d %s", recorder.Code, recorder.Body.String())
	}
}

func decodeOperation(t *testing.T, recorder *httptest.ResponseRecorder) v1.Operation {
	t.Helper()
	var decoded v1.OperationResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode operation response from %q: %v", recorder.Body.String(), err)
	}
	return decoded.Operation
}

func submitBody(resource string) string {
	return `{"kind":"` + v1.KindExecutorCommand + `","resource":"` + resource +
		`","spec":{"argv":["/bin/echo","hi"]}}`
}

// 本机（Unix socket）那条路必须**逐字节**保持迭代 4 的行为：能连上就等于本地运维，
// 因此没有 TLS 的请求一律是全权的本机身份。
func TestLocalRequestIsFullWrite(t *testing.T) {
	f := newAuthzFixture(t, nil)

	recorder := f.call(t, http.MethodPost, "/api/v1/operations", submitBody("local-probe"), nil)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("本机应当能提交操作，得到 %d %s", recorder.Code, recorder.Body.String())
	}

	identity := f.call(t, http.MethodGet, "/api/v1/identity", "", nil)
	var decoded v1.IdentityResponse
	if err := json.Unmarshal(identity.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	if decoded.Backend != string(domain.PrincipalKindLocal) {
		t.Fatalf("backend 应当是 unix，得到 %q", decoded.Backend)
	}
	if decoded.Scope != string(domain.ScopeWrite) {
		t.Fatalf("本机的档位应当是 write，得到 %q", decoded.Scope)
	}
	if decoded.ClientCn != "" {
		t.Fatalf("socket 那条路没有证书身份，clientCn 应当为空，得到 %q", decoded.ClientCn)
	}
	if decoded.Applications == nil {
		t.Fatal("本机查得到应用数，不该是 null")
	}
}

// 不在名单里的 CN 一律拒绝——默认拒绝，不是默认放行。
func TestUnknownClientCnIsRejected(t *testing.T) {
	f := newAuthzFixture(t, staticIdentities{
		"opsctl-central": {CN: "opsctl-central", Scope: domain.ScopeWrite},
	})

	recorder := f.callRemote(t, "stranger", http.MethodGet, "/api/v1/identity", "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", recorder.Code)
	}
	decoded := decodeErrorResponse(t, recorder)
	if decoded.Code != v1.CodeRemoteForbidden {
		t.Fatalf("want REMOTE_FORBIDDEN, got %s", decoded.Code)
	}
	if !strings.Contains(decoded.Message, "stranger") {
		t.Fatalf("报错应当回显那个 CN，便于运维去改名单：%s", decoded.Message)
	}
}

// 只读身份打只读端点正常，打写端点被拒；而且**拒绝发生在动手之前**。
func TestReadScopeCannotWriteAndTouchesNothing(t *testing.T) {
	f := newAuthzFixture(t, staticIdentities{
		"opsctl-readonly": {CN: "opsctl-readonly", Scope: domain.ScopeRead},
	})

	// 只读端点：通。
	identity := f.callRemote(t, "opsctl-readonly", http.MethodGet, "/api/v1/identity", "")
	if identity.Code != http.StatusOK {
		t.Fatalf("只读身份应当能问身份，得到 %d", identity.Code)
	}
	var decoded v1.IdentityResponse
	if err := json.Unmarshal(identity.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	if decoded.Backend != string(domain.PrincipalKindRemote) || decoded.ClientCn != "opsctl-readonly" {
		t.Fatalf("identity 没报出远程身份：%+v", decoded)
	}
	if decoded.Scope != string(domain.ScopeRead) {
		t.Fatalf("档位应当是 read，得到 %q", decoded.Scope)
	}

	// 写端点：403，且库里**一条操作都没有**。
	before := f.countRows(t, `SELECT COUNT(*) FROM operations`)
	recorder := f.callRemote(t, "opsctl-readonly", http.MethodPost, "/api/v1/operations", submitBody("readonly-probe"))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d %s", recorder.Code, recorder.Body.String())
	}
	decoded2 := decodeErrorResponse(t, recorder)
	if decoded2.Code != v1.CodeRemoteForbidden {
		t.Fatalf("want REMOTE_FORBIDDEN, got %s", decoded2.Code)
	}
	if after := f.countRows(t, `SELECT COUNT(*) FROM operations`); after != before {
		t.Fatalf("被拒的请求不该留下任何操作：之前 %d 条，之后 %d 条", before, after)
	}
}

// 可写身份提交的操作，审计里的 actor 是**证书 CN**，不是请求体里自报的值。
//
// 走备份路径是因为它是所有平台上唯一一条远程**真能建出 Operation** 的写路径：
// executor.command 被 D5 禁掉，runtime.* 与 deploy 在没有 systemd 的机器上没有
// 适配器。「谁做的」这件事必须在一个真的落到库里的操作上验，否则验的是别的东西。
func TestRemoteActorComesFromCertificateNotRequest(t *testing.T) {
	f := newAuthzFixture(t, staticIdentities{
		"opsctl-central": {CN: "opsctl-central", Scope: domain.ScopeWrite},
	})
	f.putBackupPolicy(t, "orders", t.TempDir())

	recorder := f.callRemote(t, "opsctl-central", http.MethodPost, "/api/v1/backups",
		`{"policy":"orders","createdBy":"alice"}`)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d %s", recorder.Code, recorder.Body.String())
	}
	op := decodeOperation(t, recorder)
	if op.CreatedBy != "remote:opsctl-central" {
		t.Fatalf("操作记录里的 createdBy 应当是认证身份，得到 %q", op.CreatedBy)
	}

	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var actor, details string
	if err := db.QueryRow(
		`SELECT actor, details_json FROM audit_events WHERE event_type = 'operation.created' AND operation_id = ?`,
		op.ID).Scan(&actor, &details); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if actor != "remote:opsctl-central" {
		t.Fatalf("审计的 actor 应当是证书 CN，得到 %q", actor)
	}
	// 自报值不丢：它进的是 details，作为一条线索，而不是事实。
	if !strings.Contains(details, `"claimedBy":"alice"`) {
		t.Fatalf("审计的 details 里应当留下自报的身份，得到 %q", details)
	}
}

// 本机请求的自报值照旧可信——这是迭代 4 的既有行为，不能被这一片改掉。
func TestLocalActorStillComesFromRequest(t *testing.T) {
	f := newAuthzFixture(t, nil)

	body := `{"kind":"` + v1.KindExecutorCommand + `","resource":"local-actor",` +
		`"createdBy":"bob","spec":{"argv":["/bin/echo","hi"]}}`
	recorder := f.call(t, http.MethodPost, "/api/v1/operations", body, nil)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d %s", recorder.Code, recorder.Body.String())
	}
	op := decodeOperation(t, recorder)
	if op.CreatedBy != "bob" {
		t.Fatalf("本机请求的 createdBy 应当原样保留，得到 %q", op.CreatedBy)
	}

	if count := f.countRows(t,
		`SELECT COUNT(*) FROM audit_events WHERE details_json LIKE '%claimedBy%'`); count != 0 {
		t.Fatalf("本机请求不该产生 claimedBy，得到 %d 条", count)
	}
}

// D5：远程禁止 executor.command，而且这是刻意的能力缺口，报错要说清楚。
func TestRemoteCannotSubmitExecutorCommand(t *testing.T) {
	f := newAuthzFixture(t, staticIdentities{
		"opsctl-central": {CN: "opsctl-central", Scope: domain.ScopeWrite},
	})

	before := f.countRows(t, `SELECT COUNT(*) FROM operations`)
	recorder := f.callRemote(t, "opsctl-central", http.MethodPost, "/api/v1/operations", submitBody("remote-exec"))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d %s", recorder.Code, recorder.Body.String())
	}
	decoded := decodeErrorResponse(t, recorder)
	if decoded.Code != v1.CodeRemoteForbidden {
		t.Fatalf("want REMOTE_FORBIDDEN, got %s", decoded.Code)
	}
	if !strings.Contains(decoded.Message, "executor.command") {
		t.Fatalf("报错应当点明是哪个 kind 被禁：%s", decoded.Message)
	}
	if after := f.countRows(t, `SELECT COUNT(*) FROM operations`); after != before {
		t.Fatalf("被拒的请求不该留下任何操作：之前 %d 条，之后 %d 条", before, after)
	}
}

// 应用白名单：名单里的应用能碰，名单外的不能——**直接打 /operations 也不行**。
// 后半条是「HTTP 层判不了、必须在 Service.Create 里再判一次」那一半的证据。
func TestApplicationAllowlistCoversDirectOperationSubmission(t *testing.T) {
	f := newAuthzFixture(t, staticIdentities{
		"opsctl-central": {
			CN:           "opsctl-central",
			Scope:        domain.ScopeWrite,
			Applications: []string{"orders-api"},
		},
	})
	f.createApplication(t, "orders-api")
	f.createApplication(t, "billing-api")

	// 名单内的应用：授权这一关得过（后面的失败只可能来自别的原因）。
	inList := f.callRemote(t, "opsctl-central", http.MethodPut,
		"/api/v1/applications/orders-api/spec", `{"apiVersion":"ops.frz.io/v1alpha1","kind":"ApplicationSpec"}`)
	if inList.Code == http.StatusForbidden {
		t.Fatalf("orders-api 在名单里，不该被授权拒掉：%s", inList.Body.String())
	}

	// 名单外的应用：连只读也不给。
	for _, call := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"读名单外的应用", http.MethodGet, "/api/v1/applications/billing-api", ""},
		{"写名单外的应用", http.MethodPut, "/api/v1/applications/billing-api/spec",
			`{"apiVersion":"ops.frz.io/v1alpha1","kind":"ApplicationSpec"}`},
		{"直接提交指向名单外应用的操作", http.MethodPost, "/api/v1/operations",
			`{"kind":"runtime.start","resource":"billing-api"}`},
	} {
		recorder := f.callRemote(t, "opsctl-central", call.method, call.path, call.body)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%s：want 403，得到 %d %s", call.name, recorder.Code, recorder.Body.String())
		}
		if decoded := decodeErrorResponse(t, recorder); decoded.Code != v1.CodeRemoteForbidden {
			t.Fatalf("%s：want REMOTE_FORBIDDEN，得到 %s", call.name, decoded.Code)
		}
	}

	// 名单内的应用直接提交操作时，拿到的**不是**授权错误——它只可能因为别的理由失败
	// （测试环境没装配 systemd 适配器，因此是 RUNTIME_UNSUPPORTED）。
	allowed := f.callRemote(t, "opsctl-central", http.MethodPost, "/api/v1/operations",
		`{"kind":"runtime.start","resource":"orders-api"}`)
	if allowed.Code == http.StatusForbidden {
		t.Fatalf("orders-api 在名单里，不该被授权拒掉：%s", allowed.Body.String())
	}
	if decoded := decodeErrorResponse(t, allowed); decoded.Code == v1.CodeRemoteForbidden {
		t.Fatalf("拒绝理由不该是授权：%s", decoded.Message)
	}
}

// 打错路径或方法不对时，报的必须是「没有这条路由」，而不是装配错误。
// 中间件调 mux.Handler(r) 拿 pattern，拿不到时的处理最容易在这里出错。
func TestUnknownRouteIsNotReportedAsWiringError(t *testing.T) {
	f := newAuthzFixture(t, nil)

	recorder := f.call(t, http.MethodGet, "/api/v1/nope", "", nil)
	if recorder.Code == http.StatusInternalServerError {
		t.Fatalf("打错的路径不该报成 500：%s", recorder.Body.String())
	}

	// 方法不对：GET /api/v1/operations 没有注册（只有 POST）。
	withIdentities := newAuthzFixture(t, staticIdentities{
		"opsctl-central": {CN: "opsctl-central", Scope: domain.ScopeWrite},
	})
	recorder = withIdentities.callRemote(t, "opsctl-central", http.MethodGet, "/api/v1/operations", "")
	if recorder.Code == http.StatusInternalServerError {
		t.Fatalf("方法不对不该报成 500：%s", recorder.Body.String())
	}
}

// 部署目标也是「针对某个应用」的端点，因此同样受应用白名单约束。
//
// 一条 PUT /targets/{app} 改的是**这个应用该发到哪些机器**，它比一次部署影响更大：
// 一次部署只动一台机上的一个版本，而改目标会改变以后每一次批量发布的范围。
func TestApplicationAllowlistCoversTargets(t *testing.T) {
	f := newAuthzFixture(t, staticIdentities{
		"opsctl-central": {
			CN:           "opsctl-central",
			Scope:        domain.ScopeWrite,
			Applications: []string{"orders-api"},
		},
	})

	inList := f.callRemote(t, "opsctl-central", http.MethodPut,
		"/api/v1/targets/orders-api", `{"hosts":["local"]}`)
	if inList.Code == http.StatusForbidden {
		t.Fatalf("orders-api 在名单里，不该被授权拒掉：%s", inList.Body.String())
	}

	outList := f.callRemote(t, "opsctl-central", http.MethodPut,
		"/api/v1/targets/billing-api", `{"hosts":["local"]}`)
	if outList.Code != http.StatusForbidden {
		t.Fatalf("名单外的应用应当被拒，得到 %d %s", outList.Code, outList.Body.String())
	}
	if decoded := decodeErrorResponse(t, outList); decoded.Code != v1.CodeRemoteForbidden {
		t.Fatalf("want REMOTE_FORBIDDEN，得到 %s", decoded.Code)
	}

	// 列出全部目标是一次跨应用的读：与其它跨应用的读一样，只受档位约束。
	all := f.callRemote(t, "opsctl-central", http.MethodGet, "/api/v1/targets", "")
	if all.Code == http.StatusForbidden {
		t.Fatalf("跨应用的读不该被应用白名单拒掉：%s", all.Body.String())
	}
}

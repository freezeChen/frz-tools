package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fmt"
	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/backup/files"
	"github.com/freezeChen/frz-tools/internal/adapters/blob"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

const validPolicyManifest = `apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: orders
resource:
  kind: files
  paths:
    - /srv/data
encoding:
  encryption:
    enabled: false
retention:
  keepLast: 3
`

// withBackups 给测试服务装上备份能力（独立的存储根 + files 适配器）。
//
// seed 会在装上之后、请求之前拿到仓储，用来预置策略与备份行——恢复类端点的前置条件
// 是「有一份 succeeded 的备份」，而那是执行路径的产物，不该由端点用例自己跑一遍。
func withBackups(t *testing.T, seed func(t *testing.T, repo application.Repository)) func(*application.Options) {
	t.Helper()
	return func(options *application.Options) {
		store, err := blob.NewLocal(t.TempDir(), 0o640, 0o750)
		if err != nil {
			t.Fatalf("backup store: %v", err)
		}
		options.BackupStore = store
		options.BackupAdapters = []application.BackupAdapter{files.New(files.WithRoot(t.TempDir()))}
		if seed != nil && options.Repo != nil {
			seed(t, options.Repo)
		}
	}
}

func TestBackupPolicyRoundTripThroughAPI(t *testing.T) {
	server, _ := newFullServer(t, withBackups(t, nil))

	body, err := json.Marshal(v1.PutBackupPolicyRequest{Manifest: validPolicyManifest, UpdatedBy: "alice"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request, err := http.NewRequest(http.MethodPut, server.URL+"/api/v1/backup-policies/orders",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("put policy: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("want 200, got %d: %s", response.StatusCode, string(raw))
	}

	var created v1.BackupPolicyResponse
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Policy.Name != "orders" {
		t.Fatalf("name want orders, got %q", created.Policy.Name)
	}
	if created.Policy.ID == "" {
		t.Fatal("落库的策略必须有 id")
	}
	if created.Policy.Resource.Kind != string(domain.BackupResourceFiles) {
		t.Fatalf("resource.kind want files, got %q", created.Policy.Resource.Kind)
	}
	// 默认值应当被填上，而不是留空让调用方自己猜。
	if created.Policy.Timeout.BackupSeconds != int(domain.DefaultBackupTimeout.Seconds()) {
		t.Fatalf("timeout 应当取默认值，got %d", created.Policy.Timeout.BackupSeconds)
	}

	// 读回：GET 单个 + GET 列表。
	getResponse, raw := getForTest(t, server.URL+"/api/v1/backup-policies/orders")
	if getResponse.StatusCode != http.StatusOK {
		t.Fatalf("get policy want 200, got %d: %s", getResponse.StatusCode, string(raw))
	}
	listResponse, listRaw := getForTest(t, server.URL+"/api/v1/backup-policies")
	if listResponse.StatusCode != http.StatusOK {
		t.Fatalf("list policies want 200, got %d: %s", listResponse.StatusCode, string(listRaw))
	}
	var list v1.BackupPolicyListResponse
	if err := json.Unmarshal(listRaw, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Name != "orders" {
		t.Fatalf("列表内容不对: %+v", list.Items)
	}
}

func TestBackupPolicyRejectsBadManifest(t *testing.T) {
	server, _ := newFullServer(t, withBackups(t, nil))

	cases := map[string]string{
		"未知字段": `apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: orders
resource: {kind: files, paths: [/srv]}
encoding: {encryption: {enabled: false}}
retention: {keepLast: 1}
bogus: 1
`,
		"路径含 ..": `apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: orders
resource: {kind: files, paths: ["/srv/../etc"]}
encoding: {encryption: {enabled: false}}
retention: {keepLast: 1}
`,
		// 加密默认开启，因此没给密钥的 manifest 必须被拒。
		"缺密钥": `apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: orders
resource: {kind: files, paths: [/srv]}
retention: {keepLast: 1}
`,
	}
	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			body, _ := json.Marshal(v1.PutBackupPolicyRequest{Manifest: manifest})
			request, err := http.NewRequest(http.MethodPut, server.URL+"/api/v1/backup-policies/orders",
				strings.NewReader(string(body)))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("put: %v", err)
			}
			defer response.Body.Close()
			raw, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", response.StatusCode, string(raw))
			}
			assertEnvelopeCode(t, raw, v1.CodeManifestInvalid)
		})
	}
}

// 路径里的名字与 manifest 里的不一致时拒绝：不拒绝的话，「改的是哪一份」会变得含糊。
func TestBackupPolicyRejectsNameMismatch(t *testing.T) {
	server, _ := newFullServer(t, withBackups(t, nil))

	body, _ := json.Marshal(v1.PutBackupPolicyRequest{Manifest: validPolicyManifest})
	request, err := http.NewRequest(http.MethodPut, server.URL+"/api/v1/backup-policies/other-name",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", response.StatusCode, string(raw))
	}
	assertEnvelopeCode(t, raw, v1.CodeManifestInvalid)
}

func TestRunBackupCreatesOperation(t *testing.T) {
	server, _ := newFullServer(t, withBackups(t, nil))
	putPolicy(t, server, "orders")

	raw, status := postJSONBody(t, server.URL+"/api/v1/backups",
		v1.BackupRunRequest{Policy: "orders", CreatedBy: "alice"})
	if status != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", status, string(raw))
	}
	var decoded v1.OperationResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Operation.Kind != v1.KindBackupRun {
		t.Fatalf("kind want %s, got %s", v1.KindBackupRun, decoded.Operation.Kind)
	}
	// 资源用的是**策略名**：同一份策略的两次备份天然互斥。
	if decoded.Operation.Resource != "orders" {
		t.Fatalf("resource want orders, got %s", decoded.Operation.Resource)
	}
}

// 原地恢复的确认是**服务端**的硬门槛：不能只靠 CLI 拦，否则直连 API 就绕过了。
func TestRestoreRequiresConfirmationServerSide(t *testing.T) {
	server, _ := newFullServer(t, withBackups(t, seedSucceededBackup))

	raw, status := postJSONBody(t, server.URL+"/api/v1/backups/bkp_seeded/restore",
		v1.BackupRestoreRequest{Mode: "inPlace"})
	if status != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", status, string(raw))
	}
	assertEnvelopeCode(t, raw, v1.CodeBackupRestoreUnconfirmed)

	// 隔离恢复不需要确认。
	if _, status := postJSONBody(t, server.URL+"/api/v1/backups/bkp_seeded/restore",
		v1.BackupRestoreRequest{Mode: "isolated"}); status != http.StatusAccepted {
		t.Fatalf("隔离恢复 want 202, got %d", status)
	}

	// 加了确认的原地恢复可以提交。用第二份备份：第一份上已经排了一个未执行的操作，
	// 再提交会撞 resource 锁（那也是对的，但会把这条用例的意图搅浑）。
	if _, status := postJSONBody(t, server.URL+"/api/v1/backups/bkp_seeded_2/restore",
		v1.BackupRestoreRequest{Mode: "inPlace", Confirm: true}); status != http.StatusAccepted {
		t.Fatalf("已确认的原地恢复 want 202, got %d", status)
	}
}

// 恢复模式随 Operation 存下来：执行时不该再问一次「用哪种模式」。
func TestRestoreModeIsPersistedWithOperation(t *testing.T) {
	server, _ := newFullServer(t, withBackups(t, seedSucceededBackup))

	raw, _ := postJSONBody(t, server.URL+"/api/v1/backups/bkp_seeded/restore",
		v1.BackupRestoreRequest{Mode: "isolated"})
	var decoded v1.OperationResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}

	options, err := application.DecodeRestoreOptions(decoded.Operation.Spec)
	if err != nil {
		t.Fatalf("Operation 里应当带可解析的恢复参数: %v", err)
	}
	if options.Mode != domain.RestoreIsolated {
		t.Fatalf("mode want isolated, got %q", options.Mode)
	}
}

// 未配备份存储根时的行为分两类，刻意不同：
//
//   - **读取**类端点照常工作——看一眼有哪些策略、哪些备份不需要存储根，
//     把这些也拒掉只会让排查更难；
//   - **执行**类端点明确拒绝（CONFIG_INVALID），而不是 500 或 panic。
func TestBackupEndpointsWithoutStore(t *testing.T) {
	server, _ := newFullServer(t)

	// 读取类：200。
	for _, path := range []string{"/api/v1/backup-policies", "/api/v1/backups"} {
		response, raw := getForTest(t, server.URL+path)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s 未配备份根时也应当可用，got %d: %s", path, response.StatusCode, string(raw))
		}
	}

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/backups", `{"policy":"orders"}`},
		{http.MethodPost, "/api/v1/backups/bkp_1/verify", `{}`},
		{http.MethodPost, "/api/v1/backups/bkp_1/restore", `{"mode":"isolated"}`},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var (
				status int
				raw    []byte
			)
			if tc.method == http.MethodGet {
				response, body := getForTest(t, server.URL+tc.path)
				status, raw = response.StatusCode, body
			} else {
				raw, status = postRawJSON(t, server.URL+tc.path, tc.body)
			}
			if status != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", status, string(raw))
			}
			assertEnvelopeCode(t, raw, v1.CodeConfigInvalid)
		})
	}
}

// postRawJSON 发一个原始 JSON 字符串的 POST。既有的 postJSONBody 接收的是会被
// marshal 的 payload，而这里刻意要传「原样的、可能是畸形 JSON」的body。
func postRawJSON(t *testing.T, url, body string) ([]byte, int) {
	t.Helper()
	response, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return raw, response.StatusCode
}

func putPolicy(t *testing.T, server *httptest.Server, name string) {
	t.Helper()
	body, err := json.Marshal(v1.PutBackupPolicyRequest{Manifest: validPolicyManifest})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request, err := http.NewRequest(http.MethodPut, server.URL+"/api/v1/backup-policies/"+name,
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("put policy: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("put policy want 200, got %d: %s", response.StatusCode, string(raw))
	}
}

// seedSucceededBackup 预置一份 succeeded 的备份：恢复类端点的前置条件。
//
// 直接写库而不是跑一遍备份——端点用例要验的是「请求被翻译成什么调用」，
// 备份本身的执行已经由 application 层与合约测试覆盖。
func seedSucceededBackup(t *testing.T, repo application.Repository) {
	t.Helper()
	ctx := context.Background()

	policy, err := repo.SaveBackupPolicy(ctx, &domain.BackupPolicy{
		APIVersion: domain.BackupPolicyAPIVersion,
		Kind:       domain.BackupPolicyKind,
		Name:       "orders",
		Resource:   domain.BackupResource{Kind: domain.BackupResourceFiles, Paths: []string{"/srv/data"}},
		Encoding:   domain.BackupEncoding{Encryption: domain.BackupEncryption{Enabled: false}},
		Retention:  domain.BackupRetention{KeepLast: 3},
	}, "bpl_seed", time.Now().UTC(), "tester")
	if err != nil {
		t.Fatalf("seed policy: %v", err)
	}

	// 预置**两份**：同一份备份上的两个操作会撞 resource 锁（那是正确行为），
	// 而这里的用例要在不执行任何操作的前提下分别验「未确认被拒」与「已确认可提交」。
	now := time.Now().UTC()
	for _, id := range []string{"bkp_seeded", "bkp_seeded_2"} {
		if err := repo.CreateBackup(ctx, &domain.Backup{
			ID:            id,
			PolicyID:      policy.ID,
			Status:        domain.BackupSucceeded,
			StorageDigest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
			ResourceKind:  domain.BackupResourceFiles,
			StartedAt:     now,
			FinishedAt:    &now,
		}); err != nil {
			t.Fatalf("seed backup: %v", err)
		}
	}
}

// seedPrunableBackups 预置两份**可被清理**的备份：keepLast=1 时应当删掉较旧的那份。
//
// 两份用不同的 digest，是为了让「删内容」这条路径真的走到：同一个 digest 会被
// 跨备份引用检查拦下（那是另一条用例覆盖的行为）。
func seedPrunableBackups(t *testing.T, repo application.Repository) {
	t.Helper()
	ctx := context.Background()

	policy, err := repo.SaveBackupPolicy(ctx, &domain.BackupPolicy{
		APIVersion: domain.BackupPolicyAPIVersion,
		Kind:       domain.BackupPolicyKind,
		Name:       "orders",
		Resource:   domain.BackupResource{Kind: domain.BackupResourceFiles, Paths: []string{"/srv/data"}},
		Encoding:   domain.BackupEncoding{Encryption: domain.BackupEncryption{Enabled: false}},
		Retention:  domain.BackupRetention{KeepLast: 1},
	}, "bpl_seed", time.Now().UTC(), "tester")
	if err != nil {
		t.Fatalf("seed policy: %v", err)
	}

	now := time.Now().UTC()
	older := now.Add(-2 * time.Hour)
	seeds := []struct {
		id         string
		digestChar string
		finishedAt time.Time
	}{
		{"bkp_old", "a", older},
		{"bkp_new", "b", now},
	}
	for _, seed := range seeds {
		finished := seed.finishedAt
		if err := repo.CreateBackup(ctx, &domain.Backup{
			ID:            seed.id,
			PolicyID:      policy.ID,
			Status:        domain.BackupSucceeded,
			StorageDigest: domain.Digest("sha256:" + strings.Repeat(seed.digestChar, 64)),
			StoredBytes:   1024,
			ResourceKind:  domain.BackupResourceFiles,
			StartedAt:     finished,
			FinishedAt:    &finished,
		}); err != nil {
			t.Fatalf("seed backup: %v", err)
		}
	}
}

func pruneRequest(t *testing.T, server *httptest.Server, body any) (*http.Response, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/backups/prune", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response, payload
}

// prune 是按策略跑的：没有策略名就不知道该按哪套规则清，必须在入口就拒绝。
func TestPruneEndpointRequiresPolicy(t *testing.T) {
	server, _ := newFullServer(t, withBackups(t, seedPrunableBackups))

	response, payload := pruneRequest(t, server, v1.BackupPruneRequest{})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", response.StatusCode, string(payload))
	}
	if !strings.Contains(string(payload), string(v1.CodeInvalidRequest)) {
		t.Fatalf("错误码应当是 INVALID_REQUEST：%s", string(payload))
	}
}

func TestPruneEndpointDryRunThenReal(t *testing.T) {
	server, _ := newFullServer(t, withBackups(t, seedPrunableBackups))

	// 预演：报出将要标记的那一份，但什么都不动。
	response, payload := pruneRequest(t, server, v1.BackupPruneRequest{Policy: "orders", DryRun: true})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", response.StatusCode, string(payload))
	}
	var preview v1.BackupPruneResponse
	if err := json.Unmarshal(payload, &preview); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !preview.DryRun || preview.Policy != "orders" {
		t.Fatalf("响应应当标明预演与策略名：%+v", preview)
	}
	if len(preview.Removed) != 1 || preview.Removed[0] != "bkp_old" {
		t.Fatalf("keepLast=1 应当只报出较旧的那份，got %v", preview.Removed)
	}
	if preview.Kept != 1 {
		t.Fatalf("应当报告保留 1 份，got %d", preview.Kept)
	}
	if preview.FreedBytes != 1024 {
		t.Fatalf("应当报告预计释放的字节数，got %d", preview.FreedBytes)
	}

	// 预演之后那份应当还是 succeeded。
	backup, err := getBackupThroughAPI(t, server, "bkp_old")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if backup.Status != string(domain.BackupSucceeded) {
		t.Fatalf("预演不该改状态，got %s", backup.Status)
	}

	// 真跑。
	response, payload = pruneRequest(t, server, v1.BackupPruneRequest{Policy: "orders"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", response.StatusCode, string(payload))
	}
	var real v1.BackupPruneResponse
	if err := json.Unmarshal(payload, &real); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if real.DryRun || len(real.Removed) != 1 || real.Removed[0] != "bkp_old" {
		t.Fatalf("实跑结果不对：%+v", real)
	}
	backup, err = getBackupThroughAPI(t, server, "bkp_old")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if backup.Status != string(domain.BackupPruned) {
		t.Fatalf("被清理的备份状态应当是 pruned，got %s", backup.Status)
	}
	// 保留的那份不受影响。
	backup, err = getBackupThroughAPI(t, server, "bkp_new")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if backup.Status != string(domain.BackupSucceeded) {
		t.Fatalf("保留的那份不该被动，got %s", backup.Status)
	}
}

// P7：声明了 gfs 的策略在**提交期**被拒（HTTP 层的证明）。
func TestPutPolicyRejectsGFS(t *testing.T) {
	server, _ := newFullServer(t, withBackups(t, nil))

	manifest := `apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: orders
resource:
  kind: files
  paths:
    - /srv/data
encoding:
  encryption:
    enabled: false
retention:
  keepLast: 7
  gfs:
    daily: 7
`
	body, err := json.Marshal(v1.PutBackupPolicyRequest{Manifest: manifest})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request, err := http.NewRequest(http.MethodPut, server.URL+"/api/v1/backup-policies/orders",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("put policy: %v", err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", response.StatusCode, string(payload))
	}
	if !strings.Contains(string(payload), string(v1.CodeManifestInvalid)) {
		t.Fatalf("错误码应当是 MANIFEST_INVALID：%s", string(payload))
	}
	if !strings.Contains(string(payload), "gfs") {
		t.Fatalf("错误信息要指明是 gfs：%s", string(payload))
	}
}

func getBackupThroughAPI(t *testing.T, server *httptest.Server, id string) (*v1.Backup, error) {
	t.Helper()
	response, err := http.Get(server.URL + "/api/v1/backups/" + id)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("want 200, got %d: %s", response.StatusCode, string(raw))
	}
	var out v1.BackupResponse
	if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out.Backup, nil
}

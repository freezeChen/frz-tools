package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/blob"
	"github.com/freezeChen/frz-tools/internal/adapters/executor"
	"github.com/freezeChen/frz-tools/internal/adapters/sqlite"
	"github.com/freezeChen/frz-tools/internal/application"
)

// newFullServer 装配一套完整的服务端：spec、host/env 与带真实本地存储的制品服务都在，
// 因为提交 manifest 时要按制品的 mediaType 判定 unpack 冲突、下载端点也要真实存储。
// 一并返回制品存储，便于用例直接改坏 blob 来验证摘要校验。
//
// mutators 用来替换个别端口（例如注入 RuntimeAdapter）；默认不注入运行时适配器，
// 这正是非 Linux 上的真实部署形态，runtime 用例会据此验证 RUNTIME_UNSUPPORTED。
func newFullServer(t *testing.T, mutators ...func(*application.Options)) (*httptest.Server, *blob.Local) {
	t.Helper()

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "opsd.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	blobs, err := blob.NewLocal(filepath.Join(t.TempDir(), "artifacts"), 0o640, 0o750)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	allow := func(string) bool { return true }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	options := application.Options{
		Repo:            store,
		Executor:        executor.New(allow, 5*time.Second, 4096, nil),
		Store:           blobs,
		AllowExecutable: allow,
		Defaults:        application.Defaults{Timeout: 5 * time.Second, MaxOutputBytes: 4096},
		ArtifactPolicy:  application.ArtifactPolicy{MaxUploadBytes: 1 << 20, QuotaBytes: 1 << 22},
		Workers:         1,
		Idle:            10 * time.Millisecond,
		Logger:          logger,
	}
	for _, mutate := range mutators {
		mutate(&options)
	}
	runtime := application.NewRuntime(options)

	server := httptest.NewServer(NewServer(Dependencies{
		Service:   runtime.Service,
		Artifacts: runtime.Artifacts,
		Catalogs:  runtime.Catalogs,
		Specs:     runtime.Specs,
		Hosts:     runtime.Hosts,
		Runtimes:  runtime.Runtimes,
		Backups:   runtime.Backups,
		Store:     store,
		Workers:   1,
		Logger:    logger,
	}).Handler())
	t.Cleanup(server.Close)
	return server, blobs
}

// billingManifest 是一份字段齐全的合法 manifest，artifactRef 由调用方填入制品 ID 或摘要。
func billingManifest(artifactRef string, extraArtifact string) string {
	return fmt.Sprintf(`apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: billing-api
runtime: go
artifact:
  id: %s
%s
exec:
  argv: [/opt/billing-api/bin/billing-api, --config, /etc/billing-api/config.yaml]
  workingDirectory: /var/lib/billing-api
  runUser: billing-api
  environment:
    GOMEMLIMIT: 40MiB
  ports: [8080]
health:
  readiness:
    type: tcp
    target: "127.0.0.1:8080"
    consecutiveSuccesses: 2
  startTimeoutSeconds: 90
logs:
  directory: /var/log/billing-api
`, artifactRef, extraArtifact)
}

func createApplication(t *testing.T, server *httptest.Server, name string) v1.Application {
	t.Helper()

	payload, err := json.Marshal(v1.CreateApplicationRequest{Name: name})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	response, err := http.Post(server.URL+"/api/v1/applications", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create application want 201, got %d", response.StatusCode)
	}
	var decoded v1.ApplicationResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode application: %v", err)
	}
	return decoded.Application
}

func putSpec(t *testing.T, server *httptest.Server, appRef, manifest string) (*http.Response, []byte) {
	t.Helper()

	payload, err := json.Marshal(v1.PutApplicationSpecRequest{Manifest: manifest, UpdatedBy: "test"})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	request, err := http.NewRequest(http.MethodPut,
		server.URL+"/api/v1/applications/"+appRef+"/spec", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("put spec: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return response, body
}

func getSpec(t *testing.T, server *httptest.Server, appRef string) (*http.Response, []byte) {
	t.Helper()

	response, err := http.Get(server.URL + "/api/v1/applications/" + appRef + "/spec")
	if err != nil {
		t.Fatalf("get spec: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return response, body
}

func decodeSpec(t *testing.T, body []byte) v1.ApplicationSpec {
	t.Helper()
	var decoded v1.ApplicationSpecResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode spec from %q: %v", string(body), err)
	}
	if decoded.APIVersion != v1.APIVersion {
		t.Fatalf("spec response must carry apiVersion, got %q", decoded.APIVersion)
	}
	return decoded.Spec
}

func TestPutSpecAppliesDefaultsAndMediaTypeStrategy(t *testing.T) {
	server, _ := newFullServer(t)
	createApplication(t, server, "billing-api")
	artifact, status, _ := putArtifact(t, server, "billing tarball", map[string]string{
		"X-Artifact-Media-Type": "application/gzip",
	})
	if status != http.StatusCreated {
		t.Fatalf("upload artifact want 201, got %d", status)
	}

	response, body := putSpec(t, server, "billing-api", billingManifest(artifact.ID, ""))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("put spec want 200, got %d: %s", response.StatusCode, string(body))
	}

	spec := decodeSpec(t, body)
	if spec.Application != "billing-api" || spec.Artifact.ID != artifact.ID {
		t.Fatalf("提交内容未被原样保留：%+v", spec)
	}
	// 未显式声明 unpack 时按 mediaType 推断：application/gzip → tar-gz。
	if spec.Artifact.Unpack.Strategy != "tar-gz" {
		t.Fatalf("want strategy tar-gz inferred from mediaType, got %q", spec.Artifact.Unpack.Strategy)
	}
	// 未声明的字段由领域层的 applyDefaults 补齐，而不是留空。
	if spec.Systemd.UnitName != "billing-api.service" || spec.Systemd.RestartPolicy != "on-failure" {
		t.Fatalf("默认 unitName/restartPolicy 未生效：%+v", spec.Systemd)
	}
	if spec.Health.StartTimeoutSeconds != 90 || spec.Health.StopTimeoutSeconds != 30 {
		t.Fatalf("健康检查超时不符：%+v", spec.Health)
	}
	if len(spec.Exec.Argv) != 3 || spec.Exec.RunUser != "billing-api" {
		t.Fatalf("exec 段不符：%+v", spec.Exec)
	}
}

func TestPutSpecRejectsExplicitStrategyConflict(t *testing.T) {
	server, _ := newFullServer(t)
	createApplication(t, server, "billing-api")
	artifact, _, _ := putArtifact(t, server, "another tarball", map[string]string{
		"X-Artifact-Media-Type": "application/gzip",
	})

	// 显式声明 zip，但制品 mediaType 是 application/gzip（对应 tar-gz）：必须报冲突。
	response, body := putSpec(t, server, "billing-api",
		billingManifest(artifact.ID, "  unpack:\n    strategy: zip\n"))
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", response.StatusCode, string(body))
	}
	assertEnvelopeCode(t, body, v1.CodeManifestConflict)
}

func TestPutSpecRejectsUnknownField(t *testing.T) {
	server, _ := newFullServer(t)
	createApplication(t, server, "billing-api")

	// 严格解码必须拦住未知字段，否则用户会以为它生效了。
	manifest := billingManifest("art_missing", "") + "  unexpected: 1\n"
	response, body := putSpec(t, server, "billing-api", manifest)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", response.StatusCode, string(body))
	}
	assertEnvelopeCode(t, body, v1.CodeManifestInvalid)

	// 非法的提交不得留下任何副作用。
	specResponse, specBody := getSpec(t, server, "billing-api")
	if specResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("非法 manifest 不应落库，want 404, got %d: %s", specResponse.StatusCode, string(specBody))
	}
	assertEnvelopeCode(t, specBody, v1.CodeSpecNotFound)
}

func TestPutSpecRejectsRelativeArgv(t *testing.T) {
	server, _ := newFullServer(t)
	createApplication(t, server, "billing-api")

	manifest := `apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: billing-api
runtime: go
artifact:
  id: art_whatever
exec:
  argv: [bin/billing-api]
  workingDirectory: /var/lib/billing-api
  runUser: billing-api
logs:
  directory: /var/log/billing-api
health:
  readiness:
    type: tcp
    target: "127.0.0.1:8080"
`
	response, body := putSpec(t, server, "billing-api", manifest)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", response.StatusCode, string(body))
	}
	assertEnvelopeCode(t, body, v1.CodeManifestInvalid)
}

func TestPutSpecRequiresApplicationToExist(t *testing.T) {
	server, _ := newFullServer(t)

	response, body := putSpec(t, server, "missing-app", billingManifest("art_missing", ""))
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", response.StatusCode, string(body))
	}
	assertEnvelopeCode(t, body, v1.CodeApplicationNotFound)
}

func TestGetSpecRoundTripsAndReportsMissingSpec(t *testing.T) {
	server, _ := newFullServer(t)
	createApplication(t, server, "billing-api")
	createApplication(t, server, "inventory-api")

	response, body := getSpec(t, server, "billing-api")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("未提交 manifest 的应用 want 404, got %d", response.StatusCode)
	}
	assertEnvelopeCode(t, body, v1.CodeSpecNotFound)

	manifest := billingManifest("art_0001", "  unpack:\n    strategy: tar\n    stripComponents: 1\n")
	if putResponse, putBody := putSpec(t, server, "billing-api", manifest); putResponse.StatusCode != http.StatusOK {
		t.Fatalf("put spec want 200, got %d: %s", putResponse.StatusCode, string(putBody))
	}

	// 规格按应用隔离：给 billing-api 提交 manifest 不能让 inventory-api 也「有规格」。
	otherResponse, otherBody := getSpec(t, server, "inventory-api")
	if otherResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("另一个应用不应有规格，want 404, got %d", otherResponse.StatusCode)
	}
	assertEnvelopeCode(t, otherBody, v1.CodeSpecNotFound)

	readResponse, readBody := getSpec(t, server, "billing-api")
	defer readResponse.Body.Close()
	if readResponse.StatusCode != http.StatusOK {
		t.Fatalf("get spec want 200, got %d: %s", readResponse.StatusCode, string(readBody))
	}
	spec := decodeSpec(t, readBody)
	// 制品不存在时不做 mediaType 冲突判定，声明的策略原样保留。
	if spec.Artifact.Unpack.Strategy != "tar" || spec.Artifact.Unpack.StripComponents != 1 {
		t.Fatalf("unpack 段不符：%+v", spec.Artifact.Unpack)
	}
	if spec.Exec.Environment["GOMEMLIMIT"] != "40MiB" || spec.Health.Readiness.ConsecutiveSuccesses != 2 {
		t.Fatalf("取回的规格与提交内容不一致：%+v", spec)
	}

	if repeated, repeatedBody := getSpec(t, server, "billing-api"); repeated.StatusCode != http.StatusOK {
		t.Fatalf("重复 GET want 200, got %d: %s", repeated.StatusCode, string(repeatedBody))
	}
}

func TestPutSpecOverwritesPreviousSpec(t *testing.T) {
	server, _ := newFullServer(t)
	createApplication(t, server, "billing-api")

	if response, body := putSpec(t, server, "billing-api", billingManifest("art_first", "")); response.StatusCode != http.StatusOK {
		t.Fatalf("put spec want 200, got %d: %s", response.StatusCode, string(body))
	}
	second := billingManifest("art_second", "")
	if response, body := putSpec(t, server, "billing-api", second); response.StatusCode != http.StatusOK {
		t.Fatalf("put spec want 200, got %d: %s", response.StatusCode, string(body))
	}

	readResponse, readBody := getSpec(t, server, "billing-api")
	defer readResponse.Body.Close()
	if readResponse.StatusCode != http.StatusOK {
		t.Fatalf("get spec want 200, got %d: %s", readResponse.StatusCode, string(readBody))
	}
	spec := decodeSpec(t, readBody)
	// 一个应用一份「当前」规格：第二次提交整体覆盖第一次，不做字段合并。
	if spec.Artifact.ID != "art_second" {
		t.Fatalf("want the latest spec to win, got artifact %q", spec.Artifact.ID)
	}
}

func TestPutSpecRejectsEmptyManifest(t *testing.T) {
	server, _ := newFullServer(t)
	createApplication(t, server, "billing-api")

	response, body := putSpec(t, server, "billing-api", "   \n")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", response.StatusCode, string(body))
	}
	assertEnvelopeCode(t, body, v1.CodeManifestInvalid)
}

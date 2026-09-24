package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// newUnixServer 在 unix socket 上跑 handler。客户端只认 socket，因此测试必须真的走
// socket 而不是 httptest 的 TCP 监听，否则 DialContext 的接线永远不会被测到。
func newUnixServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	socket := filepath.Join(t.TempDir(), "opsd.sock")
	// macOS 的 unix socket 路径上限是 104 字节，t.TempDir() 在 CI 上可能更长。
	if len(socket) > 100 {
		dir, err := os.MkdirTemp("/tmp", "frz-client-")
		if err != nil {
			t.Fatalf("temp dir: %v", err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		socket = filepath.Join(dir, "opsd.sock")
	}

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Close()
		listener.Close()
	})

	return New(socket, 5*time.Second)
}

// writeEnvelope 输出一个成功的 JSON 响应。
func writeEnvelope(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

// writeErrorEnvelope 输出错误信封，与 opsd 的 wire 形态一致。
func writeErrorEnvelope(t *testing.T, w http.ResponseWriter, status int, code v1.ErrorCode, message string) {
	t.Helper()
	writeEnvelope(t, w, status, v1.ErrorResponse{APIVersion: v1.APIVersion, Code: code, Message: message})
}

func specEnvelope(spec v1.ApplicationSpec) v1.ApplicationSpecResponse {
	return v1.ApplicationSpecResponse{APIVersion: v1.APIVersion, Spec: spec}
}

func TestPutApplicationSpecSendsManifestAndDecodesSpec(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   v1.PutApplicationSpecRequest
	)

	client := newUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writeEnvelope(t, w, http.StatusOK, specEnvelope(v1.ApplicationSpec{
			APIVersion:  v1.APIVersion,
			Kind:        "ApplicationSpec",
			Application: "billing-api",
			Runtime:     "go",
			Exec:        v1.SpecExec{Argv: []string{"/opt/billing-api/bin/billing-api"}, RunUser: "billing-api"},
		}))
	})

	manifest := "apiVersion: ops.frz.io/v1alpha1\nkind: ApplicationSpec\n"
	spec, err := client.PutApplicationSpec(context.Background(), "billing-api", []byte(manifest), "tester")
	if err != nil {
		t.Fatalf("put spec: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v1/applications/billing-api/spec" {
		t.Fatalf("unexpected request %s %s", gotMethod, gotPath)
	}
	// manifest 必须原样上行：CLI 不做任何解码，严格解码与迁移链只在 opsd 侧。
	if gotBody.Manifest != manifest || gotBody.UpdatedBy != "tester" {
		t.Fatalf("unexpected request body: %+v", gotBody)
	}
	if spec.Application != "billing-api" || spec.Exec.RunUser != "billing-api" {
		t.Fatalf("unexpected spec: %+v", spec)
	}
}

func TestPutApplicationSpecSurfacesCodedError(t *testing.T) {
	client := newUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeErrorEnvelope(t, w, http.StatusBadRequest, v1.CodeManifestInvalid, "manifest 字段非法")
	})

	_, err := client.PutApplicationSpec(context.Background(), "billing-api", []byte("bad"), "")
	if err == nil {
		t.Fatal("want an error for an invalid manifest")
	}
	if code := domain.CodeOf(err); code != v1.CodeManifestInvalid {
		t.Fatalf("want %s, got %s", v1.CodeManifestInvalid, code)
	}
	if v1.ExitCode(domain.CodeOf(err)) != 17 {
		t.Fatalf("MANIFEST_INVALID 的退出码应为 17，got %d", v1.ExitCode(domain.CodeOf(err)))
	}
}

func TestGetApplicationSpecDecodesSpecAndReportsMissing(t *testing.T) {
	client := newUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/applications/missing/spec" {
			writeErrorEnvelope(t, w, http.StatusNotFound, v1.CodeSpecNotFound, "no spec")
			return
		}
		writeEnvelope(t, w, http.StatusOK, specEnvelope(v1.ApplicationSpec{
			APIVersion:  v1.APIVersion,
			Application: "billing-api",
			Systemd:     v1.SpecSystemd{UnitName: "billing-api.service"},
		}))
	})

	spec, err := client.GetApplicationSpec(context.Background(), "billing-api")
	if err != nil {
		t.Fatalf("get spec: %v", err)
	}
	if spec.Systemd.UnitName != "billing-api.service" {
		t.Fatalf("unexpected spec: %+v", spec)
	}

	if _, err := client.GetApplicationSpec(context.Background(), "missing"); domain.CodeOf(err) != v1.CodeSpecNotFound {
		t.Fatalf("want SPEC_NOT_FOUND, got %v", err)
	}
}

func TestListHostsAndEnvironments(t *testing.T) {
	client := newUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/hosts":
			writeEnvelope(t, w, http.StatusOK, v1.HostListResponse{
				APIVersion: v1.APIVersion,
				Items: []v1.Host{
					{ID: "host_local", Name: "local"},
					{ID: "host_web", Name: "web-01", Address: "10.0.0.11", Labels: map[string]string{"role": "web"}},
				},
			})
		case "/api/v1/environments":
			writeEnvelope(t, w, http.StatusOK, v1.EnvironmentListResponse{
				APIVersion: v1.APIVersion,
				Items:      []v1.Environment{{ID: "env_prod", Name: "production", Labels: map[string]string{"tier": "prod"}}},
			})
		case "/api/v1/hosts/no-such":
			writeErrorEnvelope(t, w, http.StatusNotFound, v1.CodeHostNotFound, "host not found")
		case "/api/v1/environments/no-such":
			writeErrorEnvelope(t, w, http.StatusNotFound, v1.CodeEnvNotFound, "environment not found")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			writeErrorEnvelope(t, w, http.StatusNotFound, v1.CodeInvalidRequest, "unexpected")
		}
	})

	hosts, err := client.ListHosts(context.Background())
	if err != nil {
		t.Fatalf("list hosts: %v", err)
	}
	if len(hosts.Items) != 2 || hosts.Items[0].Name != "local" || hosts.Items[0].Address != "" {
		t.Fatalf("unexpected hosts: %+v", hosts.Items)
	}

	environments, err := client.ListEnvironments(context.Background())
	if err != nil {
		t.Fatalf("list environments: %v", err)
	}
	if len(environments.Items) != 1 || environments.Items[0].Name != "production" {
		t.Fatalf("unexpected environments: %+v", environments.Items)
	}

	if _, err := client.GetHost(context.Background(), "no-such"); domain.CodeOf(err) != v1.CodeHostNotFound {
		t.Fatalf("want HOST_NOT_FOUND, got %v", err)
	}
	if _, err := client.GetEnvironment(context.Background(), "no-such"); domain.CodeOf(err) != v1.CodeEnvNotFound {
		t.Fatalf("want ENVIRONMENT_NOT_FOUND, got %v", err)
	}
}

// artifactServer 提供制品元数据与内容两个端点，metadata 决定客户端认为应该是什么。
func artifactServer(t *testing.T, metadata v1.Artifact, content []byte, declaredContentType string) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/artifacts/"+metadata.Digest || r.URL.Path == "/api/v1/artifacts/"+metadata.ID:
			writeEnvelope(t, w, http.StatusOK, v1.ArtifactResponse{APIVersion: v1.APIVersion, Artifact: metadata})
		case r.URL.Path == "/api/v1/artifacts/"+metadata.ID+"/content":
			w.Header().Set("Content-Type", declaredContentType)
			w.Header().Set("ETag", `"`+metadata.Digest+`"`)
			w.WriteHeader(http.StatusOK)
			w.Write(content)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			writeErrorEnvelope(t, w, http.StatusNotFound, v1.CodeArtifactNotFound, "not found")
		}
	}
}

func digestOf(t *testing.T, content []byte) string {
	t.Helper()
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestDownloadArtifactWritesFileAtomically(t *testing.T) {
	content := []byte("billing-api tarball")
	metadata := v1.Artifact{
		ID:        "art_1",
		Digest:    digestOf(t, content),
		Size:      int64(len(content)),
		MediaType: "application/gzip",
		Name:      "billing-api.tar.gz",
	}
	client := newUnixServer(t, artifactServer(t, metadata, content, "application/gzip"))

	dir := t.TempDir()
	dest := filepath.Join(dir, "download.tar.gz")
	artifact, err := client.DownloadArtifact(context.Background(), "art_1", dest)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if artifact.ID != "art_1" || artifact.Digest != metadata.Digest {
		t.Fatalf("unexpected artifact: %+v", artifact)
	}

	written, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(written) != string(content) {
		t.Fatalf("落盘内容与制品不一致：%q", string(written))
	}

	// 原子写入的中间产物不得留在目标目录里。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "download.tar.gz" {
		t.Fatalf("目标目录应只有最终文件，got %v", entryNames(entries))
	}
}

func TestDownloadArtifactRejectsDigestMismatchAndKeepsTarget(t *testing.T) {
	content := []byte("what the server actually returns")
	// 元数据声明的摘要与实际内容不同，模拟存储被改坏或两端认知不一致。
	metadata := v1.Artifact{
		ID:     "art_2",
		Digest: digestOf(t, []byte("what the record claims")),
		Size:   int64(len(content)),
	}
	client := newUnixServer(t, artifactServer(t, metadata, content, ""))

	dir := t.TempDir()
	dest := filepath.Join(dir, "app.tar.gz")
	original := []byte("先前已经存在的完整文件")
	if err := os.WriteFile(dest, original, 0o644); err != nil {
		t.Fatalf("seed destination: %v", err)
	}

	_, err := client.DownloadArtifact(context.Background(), "art_2", dest)
	if err == nil {
		t.Fatal("want a checksum error")
	}
	if code := domain.CodeOf(err); code != v1.CodeArtifactChecksum {
		t.Fatalf("want %s, got %s (%v)", v1.CodeArtifactChecksum, code, err)
	}
	if v1.ExitCode(domain.CodeOf(err)) != 5 {
		t.Fatalf("ARTIFACT_CHECKSUM_MISMATCH 的退出码应为 5，got %d", v1.ExitCode(domain.CodeOf(err)))
	}

	kept, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(kept) != string(original) {
		t.Fatalf("下载失败不得改写已有文件：%q", string(kept))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("失败的下载不应留下临时文件，got %v", entryNames(entries))
	}
}

func TestDownloadArtifactLeavesNoFileWhenTargetAbsent(t *testing.T) {
	content := []byte("truncated body that does not match the record")
	metadata := v1.Artifact{
		ID:     "art_3",
		Digest: digestOf(t, []byte("expected content")),
		Size:   int64(len(content)),
	}
	client := newUnixServer(t, artifactServer(t, metadata, content, ""))

	dir := t.TempDir()
	dest := filepath.Join(dir, "app.tar.gz")

	if _, err := client.DownloadArtifact(context.Background(), "art_3", dest); domain.CodeOf(err) != v1.CodeArtifactChecksum {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("目标路径不应存在半截文件，stat err=%v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("失败的下载不应留下临时文件，got %v", entryNames(entries))
	}
}

func TestDownloadArtifactRejectsMismatchedETag(t *testing.T) {
	content := []byte("payload")
	metadata := v1.Artifact{
		ID:     "art_4",
		Digest: digestOf(t, content),
		Size:   int64(len(content)),
	}
	client := newUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/artifacts/art_4" {
			writeEnvelope(t, w, http.StatusOK, v1.ArtifactResponse{APIVersion: v1.APIVersion, Artifact: metadata})
			return
		}
		// ETag 与元数据记录的摘要不同：两端对同一个制品 ID 的理解已经不一致。
		w.Header().Set("ETag", `"`+digestOf(t, []byte("something else"))+`"`)
		w.WriteHeader(http.StatusOK)
		w.Write(content)
	})

	dest := filepath.Join(t.TempDir(), "app.bin")
	if _, err := client.DownloadArtifact(context.Background(), "art_4", dest); domain.CodeOf(err) != v1.CodeArtifactChecksum {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ETag 不一致时不应落盘")
	}
}

func TestDownloadArtifactReportsServerError(t *testing.T) {
	client := newUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/artifacts/art_5" {
			writeErrorEnvelope(t, w, http.StatusNotFound, v1.CodeArtifactNotFound, "artifact not found")
			return
		}
		t.Errorf("unexpected path %s", r.URL.Path)
	})

	dest := filepath.Join(t.TempDir(), "app.bin")
	if _, err := client.DownloadArtifact(context.Background(), "art_5", dest); domain.CodeOf(err) != v1.CodeArtifactNotFound {
		t.Fatalf("want ARTIFACT_NOT_FOUND, got %v", err)
	}
}

func TestClientReportsUnreachableSocket(t *testing.T) {
	client := New(filepath.Join(t.TempDir(), "missing.sock"), time.Second)

	_, err := client.ListHosts(context.Background())
	if err == nil {
		t.Fatal("want an error when the socket is unreachable")
	}
	if domain.CodeOf(err) != v1.CodeInternal {
		t.Fatalf("want INTERNAL, got %s", domain.CodeOf(err))
	}
}

func TestRuntimeClientMethods(t *testing.T) {
	var startBody v1.RuntimeActionRequest

	client := newUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/applications/billing-api/runtime/validate":
			if r.Method != http.MethodPost {
				t.Errorf("validate want POST, got %s", r.Method)
			}
			writeEnvelope(t, w, http.StatusOK, v1.RuntimeValidateResponse{
				APIVersion: v1.APIVersion, Application: "app_1", Valid: true,
			})
		case "/api/v1/applications/billing-api/runtime/prepare":
			writeEnvelope(t, w, http.StatusOK, v1.RuntimePrepareResponse{
				APIVersion: v1.APIVersion, Application: "app_1", Prepared: true,
				Decision: &v1.RuntimeDecision{UnitName: "billing-api.service", Tier: "strict", SystemdVersion: 255},
			})
		case "/api/v1/applications/billing-api/runtime/health":
			if r.Method != http.MethodGet {
				t.Errorf("health want GET, got %s", r.Method)
			}
			writeEnvelope(t, w, http.StatusOK, v1.RuntimeHealthResponse{
				APIVersion: v1.APIVersion, Application: "app_1", Ready: false, Detail: "unit 未在运行（inactive）",
			})
		case "/api/v1/applications/billing-api/runtime/start":
			if err := json.NewDecoder(r.Body).Decode(&startBody); err != nil {
				t.Errorf("decode start request: %v", err)
			}
			writeEnvelope(t, w, http.StatusAccepted, v1.OperationResponse{
				APIVersion: v1.APIVersion,
				Operation:  v1.Operation{ID: "op_1", Kind: v1.KindRuntimeStart, Resource: "app_1", Status: "pending"},
			})
		case "/api/v1/applications/billing-api/runtime/stop":
			writeEnvelope(t, w, http.StatusOK, v1.OperationResponse{
				APIVersion: v1.APIVersion,
				Operation:  v1.Operation{ID: "op_2", Kind: v1.KindRuntimeStop, Resource: "app_1", Status: "pending"},
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			writeErrorEnvelope(t, w, http.StatusNotFound, v1.CodeInvalidRequest, "unexpected")
		}
	})

	validate, err := client.ValidateRuntime(context.Background(), "billing-api")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !validate.Valid || validate.Application != "app_1" {
		t.Fatalf("unexpected validate response: %+v", validate)
	}

	prepare, err := client.PrepareRuntime(context.Background(), "billing-api")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !prepare.Prepared || prepare.Decision == nil || prepare.Decision.Tier != "strict" {
		t.Fatalf("unexpected prepare response: %+v", prepare)
	}

	health, err := client.RuntimeHealth(context.Background(), "billing-api")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Ready || health.Detail == "" {
		t.Fatalf("unexpected health response: %+v", health)
	}

	op, created, err := client.StartRuntime(context.Background(), "billing-api",
		RuntimeActionInput{IdempotencyKey: "k1", CreatedBy: "alice"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !created || op.ID != "op_1" || op.Kind != v1.KindRuntimeStart {
		t.Fatalf("unexpected start result: %+v created=%v", op, created)
	}
	if startBody.IdempotencyKey != "k1" || startBody.CreatedBy != "alice" {
		t.Fatalf("start 请求体不对：%+v", startBody)
	}

	stopOp, created, err := client.StopRuntime(context.Background(), "billing-api", RuntimeActionInput{})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	// 200 表示复用既有操作（幂等键命中），202 才是新建。
	if created || stopOp.Kind != v1.KindRuntimeStop {
		t.Fatalf("unexpected stop result: %+v created=%v", stopOp, created)
	}
}

func TestRuntimeClientSurfacesCodedErrors(t *testing.T) {
	client := newUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/applications/billing-api/runtime/start":
			writeErrorEnvelope(t, w, http.StatusConflict, v1.CodeRuntimeUnsupport, "本机没有可用的运行时适配器")
		case "/api/v1/applications/billing-api/runtime/health":
			writeErrorEnvelope(t, w, http.StatusConflict, v1.CodeRuntimeNotReady, "unit 处于 failed 状态")
		default:
			writeErrorEnvelope(t, w, http.StatusNotFound, v1.CodeSpecNotFound, "no spec")
		}
	})

	_, _, err := client.StartRuntime(context.Background(), "billing-api", RuntimeActionInput{})
	if domain.CodeOf(err) != v1.CodeRuntimeUnsupport || v1.ExitCode(domain.CodeOf(err)) != 21 {
		t.Fatalf("want RUNTIME_UNSUPPORTED/21, got %s/%d (%v)", domain.CodeOf(err), v1.ExitCode(domain.CodeOf(err)), err)
	}
	if _, err := client.RuntimeHealth(context.Background(), "billing-api"); domain.CodeOf(err) != v1.CodeRuntimeNotReady {
		t.Fatalf("want RUNTIME_NOT_READY, got %v", err)
	}
	if _, err := client.ValidateRuntime(context.Background(), "billing-api"); domain.CodeOf(err) != v1.CodeSpecNotFound {
		t.Fatalf("want SPEC_NOT_FOUND, got %v", err)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

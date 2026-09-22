package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/adapters/blob"
	"frz-tools/internal/adapters/executor"
	"frz-tools/internal/adapters/sqlite"
	"frz-tools/internal/application"
)

func newArtifactServer(t *testing.T, policy application.ArtifactPolicy) (*httptest.Server, *sqlite.Store) {
	t.Helper()

	sqlStore, err := sqlite.Open(filepath.Join(t.TempDir(), "opsd.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { sqlStore.Close() })
	if err := sqlStore.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store, err := blob.NewLocal(filepath.Join(t.TempDir(), "artifacts"), 0o640, 0o750)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	if policy.MaxUploadBytes == 0 {
		policy = application.ArtifactPolicy{MaxUploadBytes: 1 << 20, QuotaBytes: 1 << 22}
	}

	allow := func(string) bool { return true }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtime := application.NewRuntime(application.Options{
		Repo:            sqlStore,
		Executor:        executor.New(allow, 5*time.Second, 4096, nil),
		Store:           store,
		AllowExecutable: allow,
		Defaults:        application.Defaults{Timeout: 5 * time.Second, MaxOutputBytes: 4096},
		ArtifactPolicy:  policy,
		Workers:         1,
		Idle:            10 * time.Millisecond,
		Logger:          logger,
	})

	server := httptest.NewServer(NewServer(Dependencies{
		Service:   runtime.Service,
		Artifacts: runtime.Artifacts,
		Store:     sqlStore,
		Workers:   1,
		Logger:    logger,
	}).Handler())
	t.Cleanup(server.Close)
	return server, sqlStore
}

// putArtifact 上传并读完响应体后再返回。若只在辅助函数里 defer 关闭响应体，
// 调用方再去读错误信封会拿到已关闭的 body，所以这里把状态码和原始 body 一并交出去。
func putArtifact(t *testing.T, server *httptest.Server, content string, headers map[string]string) (v1.Artifact, int, []byte) {
	t.Helper()

	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/artifacts", bytes.NewBufferString(content))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	for key, value := range headers {
		request.Header.Set(key, value)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	if response.StatusCode >= 400 {
		return v1.Artifact{}, response.StatusCode, body
	}

	var decoded v1.ArtifactResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode %q: %v", string(body), err)
	}
	return decoded.Artifact, response.StatusCode, body
}

func TestUploadArtifactIsIdempotentByDigest(t *testing.T) {
	server, _ := newArtifactServer(t, application.ArtifactPolicy{})

	artifact, status, _ := putArtifact(t, server, "artifact payload", map[string]string{
		"X-Artifact-Name":       "app.tar.gz",
		"X-Artifact-Media-Type": "application/gzip",
	})
	if status != http.StatusCreated {
		t.Fatalf("首次上传 want 201, got %d", status)
	}
	if artifact.Digest == "" || artifact.Size != int64(len("artifact payload")) {
		t.Fatalf("响应内容不符：%+v", artifact)
	}
	if artifact.Name != "app.tar.gz" || artifact.MediaType != "application/gzip" {
		t.Fatalf("请求头中的元数据未被保留：%+v", artifact)
	}

	second, status, _ := putArtifact(t, server, "artifact payload", nil)
	if status != http.StatusOK {
		t.Fatalf("重复上传 want 200, got %d", status)
	}
	if second.ID != artifact.ID {
		t.Fatalf("相同内容应复用记录：%s != %s", second.ID, artifact.ID)
	}
}

func TestUploadArtifactVerifiesDeclaredDigest(t *testing.T) {
	server, _ := newArtifactServer(t, application.ArtifactPolicy{})

	wrong := "sha256:" + strings.Repeat("ab", 32)
	_, status, body := putArtifact(t, server, "content", map[string]string{"X-Artifact-SHA256": wrong})
	if status != http.StatusConflict {
		t.Fatalf("want 409, got %d", status)
	}
	assertEnvelopeCode(t, body, v1.CodeArtifactChecksum)

	// 被拒绝的上传不得留下记录。
	_, status, _ = putArtifact(t, server, "other content", nil)
	if status != http.StatusCreated {
		t.Fatalf("后续合法上传 want 201, got %d", status)
	}
	list, err := http.Get(server.URL + "/api/v1/artifacts")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer list.Body.Close()

	var decoded v1.ArtifactListResponse
	if err := json.NewDecoder(list.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Items) != 1 {
		t.Fatalf("被拒绝的上传不得留下记录，got %d", len(decoded.Items))
	}
}

func TestArtifactLifecycleEndpoints(t *testing.T) {
	server, sqlStore := newArtifactServer(t, application.ArtifactPolicy{})
	created, _, _ := putArtifact(t, server, "lifecycle payload", nil)

	t.Run("get by id", func(t *testing.T) {
		response, err := http.Get(server.URL + "/api/v1/artifacts/" + created.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", response.StatusCode)
		}
	})

	t.Run("get by digest", func(t *testing.T) {
		response, err := http.Get(server.URL + "/api/v1/artifacts/" + created.Digest)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", response.StatusCode)
		}
	})

	t.Run("list", func(t *testing.T) {
		response, err := http.Get(server.URL + "/api/v1/artifacts")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		defer response.Body.Close()
		var decoded v1.ArtifactListResponse
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(decoded.Items) != 1 || decoded.Items[0].ID != created.ID {
			t.Fatalf("unexpected list: %+v", decoded.Items)
		}
		if decoded.NextCursor != created.ID {
			t.Fatalf("nextCursor 应指向最后一条，got %q", decoded.NextCursor)
		}
	})

	t.Run("verify", func(t *testing.T) {
		response, err := http.Post(server.URL+"/api/v1/artifacts/"+created.ID+"/verify", "application/json", nil)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		defer response.Body.Close()
		var decoded v1.VerifyArtifactResponse
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !decoded.Verified || decoded.Digest != created.Digest {
			t.Fatalf("unexpected verify: %+v", decoded)
		}
	})

	t.Run("delete refused while referenced", func(t *testing.T) {
		now := time.Now().UTC()
		if _, err := sqlStore.DB().ExecContext(context.Background(),
			`INSERT INTO applications (id, name, created_at, updated_at) VALUES ('app_1', 'demo', ?, ?)`,
			now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("insert application: %v", err)
		}
		if _, err := sqlStore.DB().ExecContext(context.Background(),
			`INSERT INTO releases (id, application_id, artifact_id, version, created_at) VALUES ('rel_1', 'app_1', ?, '1.0.0', ?)`,
			created.ID, now.Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("insert release: %v", err)
		}

		request, err := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/artifacts/"+created.ID, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		defer response.Body.Close()

		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if response.StatusCode != http.StatusConflict {
			t.Fatalf("want 409, got %d", response.StatusCode)
		}
		assertEnvelopeCode(t, body, v1.CodeArtifactInUse)
	})
}

func TestArtifactUploadTooLarge(t *testing.T) {
	server, _ := newArtifactServer(t, application.ArtifactPolicy{MaxUploadBytes: 4, QuotaBytes: 64})

	_, status, body := putArtifact(t, server, "way too long", nil)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", status)
	}
	assertEnvelopeCode(t, body, v1.CodeUploadTooLarge)
}

func TestArtifactEndpointsWithoutStore(t *testing.T) {
	server, _ := newTestServer(t)

	_, status, body := putArtifact(t, server, "x", nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("未装配制品服务时 want 500, got %d", status)
	}
	assertEnvelopeCode(t, body, v1.CodeInternal)
}

func TestCollectArtifactsEndpoint(t *testing.T) {
	server, _ := newArtifactServer(t, application.ArtifactPolicy{})
	for _, content := range []string{"a", "b", "c"} {
		putArtifact(t, server, content, nil)
	}

	response, err := http.Post(server.URL+"/api/v1/artifacts/gc", "application/json",
		bytes.NewBufferString(`{"keepLast":1,"dryRun":true}`))
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	defer response.Body.Close()

	var decoded v1.CollectArtifactsResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decoded.DryRun || len(decoded.Removed) != 2 {
		t.Fatalf("unexpected gc result: %+v", decoded)
	}
	if decoded.APIVersion != v1.APIVersion {
		t.Fatalf("响应必须带 apiVersion，got %q", decoded.APIVersion)
	}
}

func TestDeletedArtifactIsReportedAsNotFound(t *testing.T) {
	server, sqlStore := newArtifactServer(t, application.ArtifactPolicy{})
	created, _, _ := putArtifact(t, server, "to be deleted", nil)

	now := time.Now().UTC()
	if _, err := sqlStore.DB().ExecContext(context.Background(),
		`UPDATE artifacts SET deleted_at = ? WHERE id = ?`, now.Format(time.RFC3339Nano), created.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	response, err := http.Get(server.URL + "/api/v1/artifacts/" + created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", response.StatusCode)
	}
	assertEnvelopeCode(t, body, v1.CodeArtifactNotFound)
}

// assertEnvelopeCode 校验已经读出的错误信封，避免依赖响应体的所有权时机。
func assertEnvelopeCode(t *testing.T, body []byte, want v1.ErrorCode) {
	t.Helper()

	var envelope v1.ErrorResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error envelope from %q: %v", string(body), err)
	}
	if envelope.Code != want {
		t.Fatalf("want %s, got %s", want, envelope.Code)
	}
	if envelope.APIVersion != v1.APIVersion {
		t.Fatalf("错误信封必须带 apiVersion，got %q", envelope.APIVersion)
	}
}

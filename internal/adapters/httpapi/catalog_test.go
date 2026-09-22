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

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/blob"
	"github.com/freezeChen/frz-tools/internal/adapters/executor"
	"github.com/freezeChen/frz-tools/internal/adapters/sqlite"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/idgen"
)

// newCatalogServer 装配齐全三种服务，覆盖应用与发布的完整链路。
func newCatalogServer(t *testing.T) (*httptest.Server, *sqlite.Store) {
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

	allow := func(string) bool { return true }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtime := application.NewRuntime(application.Options{
		Repo:            sqlStore,
		Executor:        executor.New(allow, 5*time.Second, 4096, nil),
		Store:           store,
		AllowExecutable: allow,
		Defaults:        application.Defaults{Timeout: 5 * time.Second, MaxOutputBytes: 4096},
		ArtifactPolicy:  application.ArtifactPolicy{MaxUploadBytes: 1 << 20, QuotaBytes: 1 << 22},
		Workers:         1,
		Idle:            10 * time.Millisecond,
		Logger:          logger,
	})

	server := httptest.NewServer(NewServer(Dependencies{
		Service:   runtime.Service,
		Artifacts: runtime.Artifacts,
		Catalogs:  runtime.Catalogs,
		Store:     sqlStore,
		Workers:   1,
		Logger:    logger,
	}).Handler())
	t.Cleanup(server.Close)
	return server, sqlStore
}

func postJSONBody(t *testing.T, url string, payload any) ([]byte, int) {
	t.Helper()

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	response, err := http.Post(url, "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body, response.StatusCode
}

func TestCreateApplicationLifecycle(t *testing.T) {
	server, _ := newCatalogServer(t)

	body, status := postJSONBody(t, server.URL+"/api/v1/applications", v1.CreateApplicationRequest{
		Name:   "billing-api",
		Labels: map[string]string{"env": "prod"},
	})
	if status != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", status, string(body))
	}

	var decoded v1.ApplicationResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.APIVersion != v1.APIVersion || decoded.Application.ID == "" || decoded.Application.Name != "billing-api" {
		t.Fatalf("unexpected application: %+v", decoded)
	}

	// 重名应用必须被拒绝，且复用已有的 INVALID_REQUEST 码。
	duplicate, status := postJSONBody(t, server.URL+"/api/v1/applications", v1.CreateApplicationRequest{Name: "billing-api"})
	if status != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", status)
	}
	assertEnvelopeCode(t, duplicate, v1.CodeInvalidRequest)

	t.Run("按名称查询", func(t *testing.T) {
		response, err := http.Get(server.URL + "/api/v1/applications/billing-api")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", response.StatusCode)
		}
	})

	t.Run("未知应用", func(t *testing.T) {
		response, err := http.Get(server.URL + "/api/v1/applications/no-such-app")
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
		assertEnvelopeCode(t, body, v1.CodeApplicationNotFound)
	})

	t.Run("列表", func(t *testing.T) {
		response, err := http.Get(server.URL + "/api/v1/applications")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		defer response.Body.Close()

		var decoded v1.ApplicationListResponse
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(decoded.Items) != 1 || decoded.Items[0].Name != "billing-api" {
			t.Fatalf("unexpected list: %+v", decoded.Items)
		}
	})
}

func TestReleaseLifecycle(t *testing.T) {
	server, _ := newCatalogServer(t)

	artifact, status, _ := putArtifact(t, server, "release payload", nil)
	if status != http.StatusCreated {
		t.Fatalf("upload want 201, got %d", status)
	}
	if _, status := postJSONBody(t, server.URL+"/api/v1/applications", v1.CreateApplicationRequest{Name: "billing-api"}); status != http.StatusCreated {
		t.Fatalf("create application want 201, got %d", status)
	}

	body, status := postJSONBody(t, server.URL+"/api/v1/releases", v1.CreateReleaseRequest{
		Application: "billing-api",
		Artifact:    artifact.ID,
		Version:     "1.0.0",
	})
	if status != http.StatusCreated {
		t.Fatalf("登记发布 want 201, got %d: %s", status, string(body))
	}

	var created v1.ReleaseResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Release.ApplicationID == "" || created.Release.ArtifactID != artifact.ID {
		t.Fatalf("unexpected release: %+v", created.Release)
	}

	t.Run("同版本重复登记被拒绝", func(t *testing.T) {
		duplicate, status := postJSONBody(t, server.URL+"/api/v1/releases", v1.CreateReleaseRequest{
			Application: "billing-api",
			Artifact:    artifact.ID,
			Version:     "1.0.0",
		})
		if status != http.StatusConflict {
			t.Fatalf("want 409, got %d", status)
		}
		assertEnvelopeCode(t, duplicate, v1.CodeReleaseConflict)
	})

	t.Run("引用不存在的制品", func(t *testing.T) {
		missing, status := postJSONBody(t, server.URL+"/api/v1/releases", v1.CreateReleaseRequest{
			Application: "billing-api",
			Artifact:    "art_missing",
			Version:     "2.0.0",
		})
		if status != http.StatusNotFound {
			t.Fatalf("want 404, got %d", status)
		}
		assertEnvelopeCode(t, missing, v1.CodeArtifactNotFound)
	})

	t.Run("引用不存在的应用", func(t *testing.T) {
		missing, status := postJSONBody(t, server.URL+"/api/v1/releases", v1.CreateReleaseRequest{
			Application: "no-such-app",
			Artifact:    artifact.ID,
			Version:     "3.0.0",
		})
		if status != http.StatusNotFound {
			t.Fatalf("want 404, got %d", status)
		}
		assertEnvelopeCode(t, missing, v1.CodeApplicationNotFound)
	})

	t.Run("按 ID 查询", func(t *testing.T) {
		response, err := http.Get(server.URL + "/api/v1/releases/" + created.Release.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", response.StatusCode)
		}
	})

	t.Run("按应用列出发布", func(t *testing.T) {
		response, err := http.Get(server.URL + "/api/v1/applications/billing-api/releases")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		defer response.Body.Close()

		var decoded v1.ReleaseListResponse
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(decoded.Items) != 1 {
			t.Fatalf("unexpected releases: %+v", decoded.Items)
		}
	})

	t.Run("未知发布", func(t *testing.T) {
		response, err := http.Get(server.URL + "/api/v1/releases/rel_missing")
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
		assertEnvelopeCode(t, body, v1.CodeReleaseNotFound)
	})
}

func TestApplicationCreateRejectsUnknownField(t *testing.T) {
	server, _ := newCatalogServer(t)

	body, status := postJSONBody(t, server.URL+"/api/v1/applications",
		map[string]any{"name": "x", "bogus": 1})
	if status != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", status)
	}
	assertEnvelopeCode(t, body, v1.CodeInvalidRequest)
}

func TestApplicationCreateRejectsMissingName(t *testing.T) {
	server, _ := newCatalogServer(t)

	body, status := postJSONBody(t, server.URL+"/api/v1/applications", v1.CreateApplicationRequest{Name: "   "})
	if status != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", status)
	}
	assertEnvelopeCode(t, body, v1.CodeInvalidRequest)
}

// 引用一个被软删除的制品不得登记发布。
func TestReleaseRefusesDeletedArtifact(t *testing.T) {
	server, sqlStore := newCatalogServer(t)

	artifact, status, _ := putArtifact(t, server, "will be deleted", nil)
	if status != http.StatusCreated {
		t.Fatalf("upload want 201, got %d", status)
	}
	if _, status := postJSONBody(t, server.URL+"/api/v1/applications", v1.CreateApplicationRequest{Name: "demo"}); status != http.StatusCreated {
		t.Fatalf("create application want 201, got %d", status)
	}

	now := time.Now().UTC()
	if _, err := sqlStore.DB().ExecContext(context.Background(),
		`UPDATE artifacts SET deleted_at = ? WHERE id = ?`, now.Format(time.RFC3339Nano), artifact.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	body, status := postJSONBody(t, server.URL+"/api/v1/releases", v1.CreateReleaseRequest{
		Application: "demo",
		Artifact:    artifact.ID,
		Version:     "1.0.0",
	})
	if status != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", status, string(body))
	}
	assertEnvelopeCode(t, body, v1.CodeArtifactNotFound)
}

// 通过仓储与服务直接调用，验证 CatalogService 的入参校验。
func TestCatalogServiceValidatesReferences(t *testing.T) {
	sqlStore, err := sqlite.Open(filepath.Join(t.TempDir(), "opsd.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { sqlStore.Close() })
	if err := sqlStore.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	rt := application.NewRuntime(application.Options{
		Repo:    sqlStore,
		Workers: 1,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx := context.Background()

	if _, err := rt.Catalogs.GetApplication(ctx, "  "); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("空引用 want INVALID_REQUEST, got %v", err)
	}
	if _, err := rt.Catalogs.GetRelease(ctx, ""); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("空发布 ID want INVALID_REQUEST, got %v", err)
	}

	now := time.Now().UTC()
	app := &domain.Application{ID: idgen.NewApplicationID(), Name: "demo", CreatedAt: now, UpdatedAt: now}
	if err := sqlStore.CreateApplication(ctx, app); err != nil {
		t.Fatalf("create application: %v", err)
	}
	if _, err := sqlStore.DB().ExecContext(ctx,
		`INSERT INTO artifacts (id, digest, size, created_at) VALUES (?, ?, ?, ?)`,
		"art_used", "sha256:"+strings.Repeat("ab", 32), 10, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}

	release, err := rt.Catalogs.CreateRelease(ctx, "demo", "art_used", "1.0.0", nil, "tester")
	if err != nil {
		t.Fatalf("create release: %v", err)
	}
	if release.ApplicationID != app.ID || release.ArtifactID != "art_used" {
		t.Fatalf("unexpected release: %+v", release)
	}
}

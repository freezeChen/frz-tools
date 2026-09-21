package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/adapters/executor"
	"frz-tools/internal/adapters/sqlite"
	"frz-tools/internal/application"
	"frz-tools/internal/domain"
)

func newTestServer(t *testing.T) (*httptest.Server, *sqlite.Store) {
	t.Helper()

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "opsd.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	allow := func(string) bool { return true }
	exec := executor.New(allow, 5*time.Second, 4096, nil)
	defaults := application.Defaults{Timeout: 5 * time.Second, MaxOutputBytes: 4096}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtime := application.NewRuntime(store, exec, allow, defaults, 1, 10*time.Millisecond, logger)

	server := httptest.NewServer(NewServer(runtime.Service, store, 1, logger).Handler())
	t.Cleanup(server.Close)
	return server, store
}

func postJSON(t *testing.T, url string, body string) (*http.Response, v1.OperationResponse) {
	t.Helper()
	response, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	var decoded v1.OperationResponse
	if response.StatusCode < 400 {
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return response, decoded
}

func TestHealthEndpoint(t *testing.T) {
	server, _ := newTestServer(t)
	response, err := http.Get(server.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", response.StatusCode)
	}
	var health v1.HealthResponse
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if health.APIVersion != v1.APIVersion || health.Daemon != "ok" || health.Database != "ok" {
		t.Fatalf("unexpected health: %+v", health)
	}
	if health.Workers != 1 {
		t.Fatalf("want 1 worker, got %d", health.Workers)
	}
}

func TestCreateOperationReturnsAccepted(t *testing.T) {
	server, _ := newTestServer(t)
	response, body := postJSON(t, server.URL+"/api/v1/operations",
		`{"kind":"executor.command","resource":"demo","spec":{"argv":["/usr/bin/true"]}}`)

	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", response.StatusCode)
	}
	if body.APIVersion != v1.APIVersion {
		t.Fatalf("response must carry apiVersion, got %q", body.APIVersion)
	}
	if body.Operation.Status != string(domain.StatusPending) {
		t.Fatalf("want pending, got %s", body.Operation.Status)
	}
	if body.Operation.ID == "" {
		t.Fatal("operation id must be set")
	}
}

func TestCreateOperationRejectsUnknownField(t *testing.T) {
	server, _ := newTestServer(t)
	response, err := http.Post(server.URL+"/api/v1/operations", "application/json",
		bytes.NewBufferString(`{"kind":"executor.command","resource":"demo","unexpected":1}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", response.StatusCode)
	}
	assertErrorCode(t, response, v1.CodeInvalidRequest)
}

func TestCreateOperationRejectsDisallowedExecutable(t *testing.T) {
	server, _ := newTestServer(t)
	response, _ := postJSON(t, server.URL+"/api/v1/operations",
		`{"kind":"executor.command","resource":"demo","spec":{"argv":["/usr/bin/true"]}}`)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", response.StatusCode)
	}

	denyResponse, err := http.Post(server.URL+"/api/v1/operations", "application/json",
		bytes.NewBufferString(`{"kind":"executor.command","resource":"other","spec":{"argv":["relative"]}}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer denyResponse.Body.Close()
	if denyResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("relative executable must be rejected with 400, got %d", denyResponse.StatusCode)
	}
}

func TestKnownOperationCanBeFetched(t *testing.T) {
	server, _ := newTestServer(t)
	_, created := postJSON(t, server.URL+"/api/v1/operations",
		`{"kind":"executor.command","resource":"demo","spec":{"argv":["/usr/bin/true"]}}`)

	response, err := http.Get(server.URL + "/api/v1/operations/" + created.Operation.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", response.StatusCode)
	}
}

func TestUnknownOperationReturnsNotFoundEnvelope(t *testing.T) {
	server, _ := newTestServer(t)
	response, err := http.Get(server.URL + "/api/v1/operations/op_missing")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", response.StatusCode)
	}
	assertErrorCode(t, response, v1.CodeOperationNotFound)
}

func TestUnknownRouteReturnsNotFoundEnvelope(t *testing.T) {
	server, _ := newTestServer(t)
	response, err := http.Get(server.URL + "/api/v1/nope")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", response.StatusCode)
	}
	assertErrorCode(t, response, v1.CodeOperationNotFound)
}

func TestLogsEndpointReturnsCursor(t *testing.T) {
	server, store := newTestServer(t)
	_, created := postJSON(t, server.URL+"/api/v1/operations",
		`{"kind":"executor.command","resource":"demo","spec":{"argv":["/usr/bin/true"]}}`)

	if err := store.AppendLog(context.Background(), domain.LogEntry{
		OperationID: created.Operation.ID,
		Level:       "info",
		Message:     "hello",
		Phase:       domain.PhaseExecute,
		Time:        time.Now().UTC(),
	}); err != nil {
		t.Fatalf("append log: %v", err)
	}

	response, err := http.Get(server.URL + "/api/v1/operations/" + created.Operation.ID + "/logs")
	if err != nil {
		t.Fatalf("get logs: %v", err)
	}
	defer response.Body.Close()

	var logs v1.LogsResponse
	if err := json.NewDecoder(response.Body).Decode(&logs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(logs.Items) != 1 || logs.Items[0].Message != "hello" {
		t.Fatalf("unexpected logs: %+v", logs.Items)
	}
	if logs.NextCursor != logs.Items[0].ID {
		t.Fatalf("nextCursor must point at the last entry, got %d", logs.NextCursor)
	}
}

func TestCancelPendingOperationEndpoint(t *testing.T) {
	server, _ := newTestServer(t)
	_, created := postJSON(t, server.URL+"/api/v1/operations",
		`{"kind":"executor.command","resource":"demo","spec":{"argv":["/usr/bin/true"]}}`)

	response, err := http.Post(server.URL+"/api/v1/operations/"+created.Operation.ID+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	defer response.Body.Close()

	var body v1.OperationResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Operation.Status != string(domain.StatusCancelled) {
		t.Fatalf("want cancelled, got %s", body.Operation.Status)
	}
}

func TestRetryRejectsSucceededOperation(t *testing.T) {
	server, store := newTestServer(t)
	_, created := postJSON(t, server.URL+"/api/v1/operations",
		`{"kind":"executor.command","resource":"demo","spec":{"argv":["/usr/bin/true"]}}`)

	if _, err := store.ClaimNextPending(context.Background(), time.Now().UTC()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.Finish(context.Background(), domain.FinishInput{
		OperationID: created.Operation.ID,
		Status:      domain.StatusSucceeded,
	}, time.Now().UTC()); err != nil {
		t.Fatalf("finish: %v", err)
	}

	response, err := http.Post(server.URL+"/api/v1/operations/"+created.Operation.ID+"/retry", "application/json", nil)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", response.StatusCode)
	}
	assertErrorCode(t, response, v1.CodeInvalidRequest)
}

func TestListenUnixRefusesLiveSocket(t *testing.T) {
	// t.TempDir 生成的路径会带上测试名，可能超出 unix socket 的路径长度限制。
	dir, err := os.MkdirTemp("", "frz-sock-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	path := filepath.Join(dir, "opsd.sock")

	first, err := ListenUnix(path, 0o660)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	defer first.Close()

	if _, err := ListenUnix(path, 0o660); err == nil {
		t.Fatal("a second listener on a live socket must be refused")
	}
}

func TestListenUnixReplacesStaleSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "frz-sock-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	path := filepath.Join(dir, "opsd.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create stale socket file: %v", err)
	}

	listener, err := ListenUnix(path, 0o660)
	if err != nil {
		t.Fatalf("stale socket must be replaced: %v", err)
	}
	defer listener.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode().Perm() != 0o660 {
		t.Fatalf("want socket mode 0660, got %04o", info.Mode().Perm())
	}
}

func assertErrorCode(t *testing.T, response *http.Response, want v1.ErrorCode) {
	t.Helper()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var apiError v1.ErrorResponse
	if err := json.Unmarshal(raw, &apiError); err != nil {
		t.Fatalf("decode error envelope from %q: %v", string(raw), err)
	}
	if apiError.Code != want {
		t.Fatalf("want error code %s, got %s", want, apiError.Code)
	}
	if apiError.APIVersion != v1.APIVersion {
		t.Fatalf("error envelope must carry apiVersion, got %q", apiError.APIVersion)
	}
	if strings.TrimSpace(apiError.Message) == "" {
		t.Fatal("error envelope must carry a message")
	}
}

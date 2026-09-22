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
	"testing"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/adapters/blob"
	"frz-tools/internal/adapters/executor"
	"frz-tools/internal/adapters/sqlite"
	"frz-tools/internal/application"
)

func newScheduleServer(t *testing.T) *httptest.Server {
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

	// 注意：这里刻意不启动调度器与 worker，端点测试只验证「创建了什么」，
	// 实际触发由 application 层的 fake clock 测试与 e2e 覆盖。
	server := httptest.NewServer(NewServer(Dependencies{
		Service:   runtime.Service,
		Artifacts: runtime.Artifacts,
		Catalogs:  runtime.Catalogs,
		Schedules: runtime.Schedules,
		Store:     sqlStore,
		Workers:   1,
		Logger:    logger,
	}).Handler())
	t.Cleanup(server.Close)
	return server
}

func createSchedule(t *testing.T, server *httptest.Server, req v1.CreateScheduleRequest) (v1.Schedule, int, []byte) {
	t.Helper()

	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	response, err := http.Post(server.URL+"/api/v1/schedules", "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if response.StatusCode >= 400 {
		return v1.Schedule{}, response.StatusCode, body
	}
	var decoded v1.ScheduleResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode %q: %v", string(body), err)
	}
	return decoded.Schedule, response.StatusCode, body
}

func intervalScheduleRequest(name string) v1.CreateScheduleRequest {
	return v1.CreateScheduleRequest{
		Name:            name,
		Kind:            "interval",
		IntervalSeconds: 300,
		Timezone:        "UTC",
		Resource:        "sched-" + name,
		Spec:            json.RawMessage(`{"argv":["/usr/bin/true"]}`),
		MissedRunPolicy: "skip",
	}
}

func TestCreateScheduleEndpoint(t *testing.T) {
	server := newScheduleServer(t)

	schedule, status, body := createSchedule(t, server, intervalScheduleRequest("nightly"))
	if status != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", status, string(body))
	}
	if schedule.NextRunAt == nil {
		t.Fatal("创建响应必须带上 nextRunAt，否则用户看不到下一次触发时间")
	}
	if schedule.IntervalSeconds != 300 || !schedule.Enabled {
		t.Fatalf("unexpected schedule: %+v", schedule)
	}
	if schedule.MissedRunPolicy != "skip" {
		t.Fatalf("默认策略应为 skip，got %q", schedule.MissedRunPolicy)
	}
}

func TestCreateScheduleRejectsInvalidInput(t *testing.T) {
	server := newScheduleServer(t)

	t.Run("cron 非法", func(t *testing.T) {
		req := intervalScheduleRequest("bad-cron")
		req.Kind = "cron"
		req.IntervalSeconds = 0
		req.Cron = "not a cron"
		_, status, body := createSchedule(t, server, req)
		if status != http.StatusBadRequest {
			t.Fatalf("want 400, got %d", status)
		}
		assertEnvelopeCode(t, body, v1.CodeScheduleInvalid)
	})

	t.Run("时区非法", func(t *testing.T) {
		req := intervalScheduleRequest("bad-tz")
		req.Timezone = "Mars/Olympus"
		_, status, body := createSchedule(t, server, req)
		if status != http.StatusBadRequest {
			t.Fatalf("want 400, got %d", status)
		}
		assertEnvelopeCode(t, body, v1.CodeScheduleInvalid)
	})

	t.Run("spec 非法", func(t *testing.T) {
		req := intervalScheduleRequest("bad-spec")
		req.Spec = json.RawMessage(`{"argv":["relative"]}`)
		_, status, body := createSchedule(t, server, req)
		if status != http.StatusBadRequest {
			t.Fatalf("want 400, got %d", status)
		}
		assertEnvelopeCode(t, body, v1.CodeInvalidRequest)
	})

	t.Run("重名", func(t *testing.T) {
		if _, status, _ := createSchedule(t, server, intervalScheduleRequest("dup")); status != http.StatusCreated {
			t.Fatalf("首次创建 want 201, got %d", status)
		}
		_, status, body := createSchedule(t, server, intervalScheduleRequest("dup"))
		if status != http.StatusBadRequest {
			t.Fatalf("want 400, got %d", status)
		}
		assertEnvelopeCode(t, body, v1.CodeScheduleInvalid)
	})
}

func TestScheduleLifecycleEndpoints(t *testing.T) {
	server := newScheduleServer(t)
	created, _, _ := createSchedule(t, server, intervalScheduleRequest("lifecycle"))

	t.Run("按名称查询", func(t *testing.T) {
		response, err := http.Get(server.URL + "/api/v1/schedules/lifecycle")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", response.StatusCode)
		}
	})

	t.Run("列表", func(t *testing.T) {
		response, err := http.Get(server.URL + "/api/v1/schedules")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		defer response.Body.Close()

		var decoded v1.ScheduleListResponse
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(decoded.Items) != 1 || decoded.Items[0].ID != created.ID {
			t.Fatalf("unexpected list: %+v", decoded.Items)
		}
	})

	t.Run("启用中的计划拒绝删除", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/schedules/"+created.ID, nil)
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
		assertEnvelopeCode(t, body, v1.CodeScheduleEnabled)
	})

	t.Run("停用后可以删除", func(t *testing.T) {
		response, err := http.Post(server.URL+"/api/v1/schedules/"+created.ID+"/disable", "application/json", nil)
		if err != nil {
			t.Fatalf("disable: %v", err)
		}
		defer response.Body.Close()

		var decoded v1.ScheduleResponse
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if decoded.Schedule.Enabled {
			t.Fatal("停用后 enabled 应为 false")
		}

		request, err := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/schedules/"+created.ID, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		deleted, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		defer deleted.Body.Close()
		if deleted.StatusCode != http.StatusNoContent {
			t.Fatalf("want 204, got %d", deleted.StatusCode)
		}
	})
}

func TestScheduleRunsEndpointStartsEmpty(t *testing.T) {
	server := newScheduleServer(t)
	created, _, _ := createSchedule(t, server, intervalScheduleRequest("runs"))

	response, err := http.Get(server.URL + "/api/v1/schedules/" + created.ID + "/runs")
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	defer response.Body.Close()

	var decoded v1.ScheduleRunListResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Items) != 0 {
		t.Fatalf("尚未触发时不应有记录: %+v", decoded.Items)
	}
	if decoded.Schedule != created.ID {
		t.Fatalf("响应应回显计划 ID，got %q", decoded.Schedule)
	}
}

func TestUnknownScheduleReturnsNotFound(t *testing.T) {
	server := newScheduleServer(t)

	response, err := http.Get(server.URL + "/api/v1/schedules/missing")
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
	assertEnvelopeCode(t, body, v1.CodeScheduleNotFound)
}

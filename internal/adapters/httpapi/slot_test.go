package httpapi

import (
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
	"github.com/freezeChen/frz-tools/internal/adapters/manifest"
	"github.com/freezeChen/frz-tools/internal/adapters/sqlite"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// 迭代 4c：槽位端点的 HTTP 形态。
//
// 语义（两份事实怎么算、对账改什么）由 application 层的用例钉住；这里验的是**翻译**：
// 端点把请求读成什么、把视图写成什么、错误怎么映射。

// stubNginxAdapter 只回答「受管文件现在指向哪一侧」。
type stubNginxAdapter struct {
	current domain.Slot
	err     error
}

func (s *stubNginxAdapter) Validate(_ context.Context, _ *domain.ApplicationSpec) error { return nil }
func (s *stubNginxAdapter) CheckLoaded(_ context.Context, _ *domain.ApplicationSpec) error {
	return nil
}
func (s *stubNginxAdapter) Apply(_ context.Context, _ *domain.ApplicationSpec, _ domain.Slot) error {
	return nil
}
func (s *stubNginxAdapter) Current(_ context.Context, _ *domain.ApplicationSpec) (domain.Slot, error) {
	return s.current, s.err
}

// idleRuntimeAdapter 报告「哪一侧都没有进程在跑」。蓝绿应用还没部署过时就是这个形态，
// 因此端点用例默认用它——用「两侧都在 active」的桩会让「没部署过」看起来像「跑着但没就绪」。
type idleRuntimeAdapter struct{ *stubRuntimeAdapter }

func (idleRuntimeAdapter) Status(_ context.Context, _ *domain.ApplicationSpec, _ domain.Slot) (domain.RuntimeStatus, error) {
	return domain.RuntimeInactive, nil
}

// slotServer 起一个只装配了槽位用例的服务器，并把 store 交回给调用方。
//
// 时间线的事件**没有写它的 API**（它们由部署与对账产生），因此用例要直接落几条才能验端点。
// 复用 newFullServer 拿不到 store，所以这里自己搭一个最小的。
func slotServer(t *testing.T, runtimeAdapter application.RuntimeAdapter, nginx application.NginxAdapter) (*httptest.Server, *sqlite.Store, *application.Runtime) {
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
	rt := application.NewRuntime(application.Options{
		Repo:            store,
		Executor:        executor.New(allow, 5*time.Second, 4096, nil),
		Store:           blobs,
		AllowExecutable: allow,
		Defaults:        application.Defaults{Timeout: 5 * time.Second, MaxOutputBytes: 4096},
		RuntimeAdapter:  runtimeAdapter,
		NginxAdapter:    nginx,
		Workers:         1,
		Idle:            10 * time.Millisecond,
		Logger:          logger,
	})
	server := httptest.NewServer(NewServer(Dependencies{Slots: rt.Slots, Logger: logger}).Handler())
	t.Cleanup(server.Close)
	return server, store, rt
}

// blueGreenHTTPManifest 是一份最小的蓝绿 manifest（应用名固定 orders-api）。
const blueGreenHTTPManifest = `apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: orders-api
runtime: go
artifact:
  id: art_XXXX
  version: 1.0.0
  unpack:
    strategy: tar-gz
exec:
  argv: [bin/server]
  workingDirectory: /var/lib/orders-api
  runUser: orders-api
  slots:
    blue:
      ports: [18081]
      readiness:
        type: tcp
        target: "127.0.0.1:18081"
    green:
      ports: [18082]
      readiness:
        type: tcp
        target: "127.0.0.1:18082"
health:
  startTimeoutSeconds: 60
logs:
  directory: /var/log/orders-api
nginx:
  listen: 8080
`

// seedBlueGreenApp 建一个蓝绿应用并登记它的规格。
func seedBlueGreenApp(t *testing.T, rt *application.Runtime) *domain.Application {
	t.Helper()
	ctx := context.Background()
	app, err := rt.Catalogs.CreateApplication(ctx, "orders-api", nil)
	if err != nil {
		t.Fatalf("创建应用: %v", err)
	}
	spec, err := manifest.ParseReader(strings.NewReader(blueGreenHTTPManifest))
	if err != nil {
		t.Fatalf("解析 manifest: %v", err)
	}
	if _, err := rt.Specs.PutSpec(ctx, app.Name, spec, "tester"); err != nil {
		t.Fatalf("登记规格: %v", err)
	}
	return app
}

func getJSON(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return response, raw
}

func TestListSlotsEndpoint(t *testing.T) {
	nginx := &stubNginxAdapter{current: domain.SlotBlue}
	server, _, rt := slotServer(t, idleRuntimeAdapter{&stubRuntimeAdapter{}}, nginx)
	app := seedBlueGreenApp(t, rt)

	response, body := getJSON(t, server.URL+"/api/v1/applications/orders-api/slots")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", response.StatusCode, string(body))
	}
	var decoded v1.SlotListResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode from %q: %v", string(body), err)
	}
	if decoded.APIVersion != v1.APIVersion || decoded.Application != app.ID {
		t.Fatalf("unexpected response: %+v", decoded)
	}
	if !decoded.OnlineKnown || decoded.ServingSlot != string(domain.SlotBlue) {
		t.Fatalf("线上事实应当报 blue，got %+v", decoded)
	}
	// 从没部署过：库里没有记录，这与「线上在 blue」不一致——端点必须如实标出来，
	// 而不是挑一边显示。
	if decoded.RecordedSlot != "" || !decoded.Inconsistent {
		t.Fatalf("两份事实不一致时要标出来，got %+v", decoded)
	}
	if len(decoded.Items) != 2 {
		t.Fatalf("两侧都要出现，got %d", len(decoded.Items))
	}
	blue, green := decoded.Items[0], decoded.Items[1]
	if blue.Slot != string(domain.SlotBlue) || green.Slot != string(domain.SlotGreen) {
		t.Fatalf("槽位顺序应当固定，got %s → %s", blue.Slot, green.Slot)
	}
	if blue.UnitName != "orders-api-blue.service" || green.UnitName != "orders-api-green.service" {
		t.Fatalf("unit 名要按槽位派生，got %+v", decoded.Items)
	}
	if len(blue.Ports) != 1 || blue.Ports[0] != 18081 || green.Ports[0] != 18082 {
		t.Fatalf("端口要按槽位给出，got %+v", decoded.Items)
	}
	// 进程状态照样问：两侧都没在跑（unit 还不存在），而 inactive 是一条事实——
	// 比「没问」的 unknown 有用。
	for _, item := range decoded.Items {
		if !item.ProcessKnown || item.ProcessState != string(domain.RuntimeInactive) {
			t.Fatalf("进程状态应当报 inactive 且为已知，got %+v", item)
		}
	}
	// 没部署过的那一侧不该探活：null 与 false 是两件事。
	if blue.Ready != nil || green.Ready != nil {
		t.Fatalf("停着的槽位不探活，ready 应当是 null，got %+v", decoded.Items)
	}
}

// 时间线端点：字段要一个不少地出去，limit 要能透传。
func TestSlotHistoryEndpoint(t *testing.T) {
	server, store, rt := slotServer(t, idleRuntimeAdapter{&stubRuntimeAdapter{}}, &stubNginxAdapter{})
	app := seedBlueGreenApp(t, rt)

	// 直接落三条事件：没有写时间线的 API，它们由部署与对账产生。
	base := time.Now().UTC()
	for i, event := range []domain.SlotEvent{
		{Slot: domain.SlotBlue, ReleaseID: "rel_1", Version: "1.0.0",
			Kind: domain.SlotEventSwitched, Detail: "流量切到这一侧（部署）",
			OperationID: "op_1", At: base},
		{Slot: domain.SlotBlue, ReleaseID: "rel_1", Version: "1.0.0",
			Kind: domain.SlotEventObserved, Detail: "观察窗口通过：150 次采样，窗口 30 秒",
			OperationID: "op_1", At: base.Add(time.Second)},
		{Slot: domain.SlotGreen, Kind: domain.SlotEventReconciled,
			Detail: "对账：按线上事实把状态从 serving 改成 standby", At: base.Add(2 * time.Second)},
	} {
		event.ApplicationID = app.ID
		if err := store.AppendSlotEvent(context.Background(), &event); err != nil {
			t.Fatalf("AppendSlotEvent #%d: %v", i, err)
		}
	}

	response, body := getJSON(t, server.URL+"/api/v1/applications/orders-api/slots/history")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", response.StatusCode, string(body))
	}
	var decoded v1.SlotHistoryResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode from %q: %v", string(body), err)
	}
	if decoded.Application != app.ID || len(decoded.Items) != 3 {
		t.Fatalf("unexpected response: %+v", decoded)
	}
	newest := decoded.Items[0]
	if newest.Kind != string(domain.SlotEventReconciled) || newest.Slot != string(domain.SlotGreen) {
		t.Fatalf("应当按时间倒序（新的在前），got %+v", decoded.Items)
	}
	switched := decoded.Items[2]
	if switched.Version != "1.0.0" || switched.OperationID != "op_1" || switched.ReleaseID != "rel_1" {
		t.Fatalf("字段要一个不少地出去，got %+v", switched)
	}
	// 可空字段留空而不是被填上零值。
	if newest.ReleaseID != "" || newest.OperationID != "" {
		t.Fatalf("对账事件的 release/operation 是空的，got %+v", newest)
	}

	// limit 透传：只要最新的一条。
	response, body = getJSON(t, server.URL+"/api/v1/applications/orders-api/slots/history?limit=1")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", response.StatusCode, string(body))
	}
	decoded = v1.SlotHistoryResponse{}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode from %q: %v", string(body), err)
	}
	if len(decoded.Items) != 1 || decoded.Items[0].Kind != string(domain.SlotEventReconciled) {
		t.Fatalf("limit 应当生效，got %+v", decoded.Items)
	}
}

func TestSlotEndpointsRejectNonBlueGreenApplication(t *testing.T) {
	// 单槽应用没有槽位：与 --slot 那处口径一致，报 INVALID_REQUEST 而不是给一份空视图。
	server, _ := newFullServer(t)
	createApplication(t, server, "billing-api")
	if response, body := putSpec(t, server, "billing-api", billingManifest("art_runtime", "")); response.StatusCode != http.StatusOK {
		t.Fatalf("put spec want 200, got %d: %s", response.StatusCode, string(body))
	}

	response, body := getJSON(t, server.URL+"/api/v1/applications/billing-api/slots")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", response.StatusCode, string(body))
	}
	assertEnvelopeCode(t, body, v1.CodeInvalidRequest)

	response, body = getJSON(t, server.URL+"/api/v1/applications/billing-api/slots/history")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("history 也要拒绝，want 400, got %d", response.StatusCode)
	}
	assertEnvelopeCode(t, body, v1.CodeInvalidRequest)
}

func TestSlotEndpointsRejectUnknownApplication(t *testing.T) {
	server, _, _ := slotServer(t, idleRuntimeAdapter{&stubRuntimeAdapter{}}, &stubNginxAdapter{})

	response, body := getJSON(t, server.URL+"/api/v1/applications/no-such-app/slots")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", response.StatusCode, string(body))
	}
	assertEnvelopeCode(t, body, v1.CodeApplicationNotFound)
}

func TestSlotHistoryEndpointRejectsBadLimit(t *testing.T) {
	server, _, rt := slotServer(t, idleRuntimeAdapter{&stubRuntimeAdapter{}}, &stubNginxAdapter{})
	seedBlueGreenApp(t, rt)

	response, body := getJSON(t, server.URL+"/api/v1/applications/orders-api/slots/history?limit=nope")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", response.StatusCode, string(body))
	}
	assertEnvelopeCode(t, body, v1.CodeInvalidRequest)
}

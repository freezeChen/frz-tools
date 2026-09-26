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

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// stubRuntimeAdapter 是 HTTP 层的假适配器：用例要验的是「端点把请求翻译成什么调用、
// 又把适配器的回答翻译成什么响应」，真机语义由 A7 的 Linux 容器证据覆盖。
type stubRuntimeAdapter struct {
	validateErr error
	prepareErr  error
	health      domain.RuntimeHealth
	healthErr   error
	calls       []string
}

func (s *stubRuntimeAdapter) Validate(_ context.Context, _ *domain.ApplicationSpec) error {
	s.calls = append(s.calls, "validate")
	return s.validateErr
}

func (s *stubRuntimeAdapter) Prepare(_ context.Context, _ *domain.ApplicationSpec, _ domain.Slot) error {
	s.calls = append(s.calls, "prepare")
	return s.prepareErr
}

func (s *stubRuntimeAdapter) Start(_ context.Context, _ *domain.ApplicationSpec, _ domain.Slot) error {
	s.calls = append(s.calls, "start")
	return nil
}

func (s *stubRuntimeAdapter) Stop(_ context.Context, _ *domain.ApplicationSpec, _ domain.Slot) error {
	s.calls = append(s.calls, "stop")
	return nil
}

func (s *stubRuntimeAdapter) Health(_ context.Context, _ *domain.ApplicationSpec, _ domain.Slot) (domain.RuntimeHealth, error) {
	s.calls = append(s.calls, "health")
	return s.health, s.healthErr
}

func (s *stubRuntimeAdapter) Status(_ context.Context, _ *domain.ApplicationSpec, _ domain.Slot) (domain.RuntimeStatus, error) {
	s.calls = append(s.calls, "status")
	return domain.RuntimeActive, nil
}

type stubReporter struct {
	decision application.RuntimeDecision
	ok       bool
}

func (s stubReporter) ReportRuntimePrepare(_ context.Context, _ *domain.ApplicationSpec, _ domain.Slot) (application.RuntimeDecision, bool) {
	return s.decision, s.ok
}

func withRuntime(adapter application.RuntimeAdapter, reporter application.RuntimePrepareReporter) func(*application.Options) {
	return func(options *application.Options) {
		options.RuntimeAdapter = adapter
		options.PrepareReporter = reporter
	}
}

// runtimeReadyApp 建一个应用并提交一份合法规格，返回应用记录。
func runtimeReadyApp(t *testing.T, server *httptest.Server, name string) v1.Application {
	t.Helper()

	app := createApplication(t, server, name)
	response, body := putSpec(t, server, app.Name, billingManifest("art_runtime", ""))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("put spec want 200, got %d: %s", response.StatusCode, string(body))
	}
	return app
}

func postRuntimeAction(t *testing.T, url, body string) (*http.Response, []byte) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(http.MethodPost, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return response, raw
}

func TestValidateRuntimeEndpoint(t *testing.T) {
	adapter := &stubRuntimeAdapter{}
	server, _ := newFullServer(t, withRuntime(adapter, nil))
	app := runtimeReadyApp(t, server, "billing-api")

	response, body := postRuntimeAction(t, server.URL+"/api/v1/applications/billing-api/runtime/validate", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", response.StatusCode, string(body))
	}
	var decoded v1.RuntimeValidateResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode from %q: %v", string(body), err)
	}
	if decoded.APIVersion != v1.APIVersion || !decoded.Valid {
		t.Fatalf("unexpected response: %+v", decoded)
	}
	if decoded.Application != app.ID {
		t.Fatalf("application want %s, got %s", app.ID, decoded.Application)
	}
	if strings.Join(adapter.calls, ",") != "validate" {
		t.Fatalf("validate 端点不得触发 prepare/start，got %v", adapter.calls)
	}
}

func TestValidateRuntimeReportsSpecNotFound(t *testing.T) {
	server, _ := newFullServer(t, withRuntime(&stubRuntimeAdapter{}, nil))
	createApplication(t, server, "empty-app")

	response, body := postRuntimeAction(t, server.URL+"/api/v1/applications/empty-app/runtime/validate", "")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", response.StatusCode)
	}
	assertEnvelopeCode(t, body, v1.CodeSpecNotFound)
}

func TestRuntimeEndpointsReportUnsupportedWithoutAdapter(t *testing.T) {
	// 非 Linux 的部署形态：没有注入适配器。所有 runtime 端点都必须给出
	// RUNTIME_UNSUPPORTED，而不是 500 或 panic。
	server, _ := newFullServer(t)
	runtimeReadyApp(t, server, "billing-api")

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/runtime/validate", ""},
		{http.MethodPost, "/runtime/prepare", ""},
		{http.MethodPost, "/runtime/start", ""},
		{http.MethodPost, "/runtime/stop", ""},
		{http.MethodGet, "/runtime/health", ""},
	}
	for _, tc := range cases {
		url := server.URL + "/api/v1/applications/billing-api" + tc.path
		var (
			response *http.Response
			body     []byte
		)
		if tc.method == http.MethodGet {
			response, body = getForTest(t, url)
		} else {
			response, body = postRuntimeAction(t, url, tc.body)
		}
		if response.StatusCode != http.StatusConflict {
			t.Fatalf("%s %s want 409, got %d: %s", tc.method, tc.path, response.StatusCode, string(body))
		}
		assertEnvelopeCode(t, body, v1.CodeRuntimeUnsupport)
	}
}

func TestPrepareRuntimeReturnsDecision(t *testing.T) {
	adapter := &stubRuntimeAdapter{}
	reporter := stubReporter{ok: true, decision: application.RuntimeDecision{
		UnitName:       "billing-api.service",
		UnitPath:       "/etc/systemd/system/billing-api.service",
		Tier:           "strict",
		SystemdVersion: 255,
		Degradations:   []string{"日志改为 journal"},
		DecidedAt:      time.Now().UTC(),
	}}
	server, _ := newFullServer(t, withRuntime(adapter, reporter))
	app := runtimeReadyApp(t, server, "billing-api")

	response, body := postRuntimeAction(t, server.URL+"/api/v1/applications/billing-api/runtime/prepare", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", response.StatusCode, string(body))
	}
	var decoded v1.RuntimePrepareResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode from %q: %v", string(body), err)
	}
	if !decoded.Prepared || decoded.Application != app.ID {
		t.Fatalf("unexpected response: %+v", decoded)
	}
	if decoded.Decision == nil {
		t.Fatal("decision 字段缺失：档位必须可追溯")
	}
	if decoded.Decision.Tier != "strict" || decoded.Decision.SystemdVersion != 255 {
		t.Fatalf("unexpected decision: %+v", decoded.Decision)
	}
	if decoded.Decision.UnitPath != "/etc/systemd/system/billing-api.service" {
		t.Fatalf("unitPath 缺失：%+v", decoded.Decision)
	}
	if strings.Join(adapter.calls, ",") != "prepare" {
		t.Fatalf("want only prepare, got %v", adapter.calls)
	}
}

func TestPrepareRuntimeOmitsUnknownDecision(t *testing.T) {
	// 适配器没有这次 Prepare 的记录时不应编一个空档位，字段直接缺省。
	server, _ := newFullServer(t, withRuntime(&stubRuntimeAdapter{}, stubReporter{ok: false}))
	runtimeReadyApp(t, server, "billing-api")

	response, body := postRuntimeAction(t, server.URL+"/api/v1/applications/billing-api/runtime/prepare", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", response.StatusCode, string(body))
	}
	if strings.Contains(string(body), `"decision"`) {
		t.Fatalf("决策未知时不应出现 decision 字段：%s", string(body))
	}
}

func TestStartRuntimeCreatesOperation(t *testing.T) {
	server, _ := newFullServer(t, withRuntime(&stubRuntimeAdapter{}, nil))
	app := runtimeReadyApp(t, server, "billing-api")

	request, err := http.NewRequest(http.MethodPost,
		server.URL+"/api/v1/applications/billing-api/runtime/start", strings.NewReader(`{"createdBy":"alice"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "start-1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post start: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", response.StatusCode)
	}

	var decoded v1.OperationResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Operation.Kind != v1.KindRuntimeStart {
		t.Fatalf("kind want %s, got %s", v1.KindRuntimeStart, decoded.Operation.Kind)
	}
	// 资源是规范化后的应用 ID，不是路径里传进来的名称。
	if decoded.Operation.Resource != app.ID {
		t.Fatalf("resource want %s, got %s", app.ID, decoded.Operation.Resource)
	}
	if decoded.Operation.Status != string(domain.StatusPending) {
		t.Fatalf("want pending, got %s", decoded.Operation.Status)
	}
	if decoded.Operation.CreatedBy != "alice" {
		t.Fatalf("createdBy 未被透传：%+v", decoded.Operation)
	}

	// 同一个幂等键重复提交返回同一个操作（200 而不是 202）。
	response2, err := http.DefaultClient.Do(mustRequest(t, server.URL+"/api/v1/applications/billing-api/runtime/start", "start-1"))
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	defer response2.Body.Close()
	if response2.StatusCode != http.StatusOK {
		t.Fatalf("重复提交 want 200, got %d", response2.StatusCode)
	}
	var repeated v1.OperationResponse
	if err := json.NewDecoder(response2.Body).Decode(&repeated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if repeated.Operation.ID != decoded.Operation.ID {
		t.Fatalf("幂等键应复用既有操作：%s != %s", repeated.Operation.ID, decoded.Operation.ID)
	}

	// 操作可用既有的查询端点读到——runtime.* 没有第二条查询路径。
	getResponse, body := getForTest(t, server.URL+"/api/v1/operations/"+decoded.Operation.ID)
	if getResponse.StatusCode != http.StatusOK {
		t.Fatalf("get operation want 200, got %d", getResponse.StatusCode)
	}
	var fetched v1.OperationResponse
	if err := json.Unmarshal(body, &fetched); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fetched.Operation.Kind != v1.KindRuntimeStart || fetched.Operation.Resource != app.ID {
		t.Fatalf("unexpected operation: %+v", fetched.Operation)
	}
}

func TestStopRuntimeCreatesOperation(t *testing.T) {
	server, _ := newFullServer(t, withRuntime(&stubRuntimeAdapter{}, nil))
	app := runtimeReadyApp(t, server, "billing-api")

	response, body := postRuntimeAction(t, server.URL+"/api/v1/applications/billing-api/runtime/stop", "")
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", response.StatusCode, string(body))
	}
	var decoded v1.OperationResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode from %q: %v", string(body), err)
	}
	if decoded.Operation.Kind != v1.KindRuntimeStop || decoded.Operation.Resource != app.ID {
		t.Fatalf("unexpected operation: %+v", decoded.Operation)
	}
}

func TestRuntimeActionRejectsDryRun(t *testing.T) {
	server, _ := newFullServer(t, withRuntime(&stubRuntimeAdapter{}, nil))
	runtimeReadyApp(t, server, "billing-api")

	response, body := postRuntimeAction(t, server.URL+"/api/v1/applications/billing-api/runtime/start",
		`{"dryRun":true}`)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", response.StatusCode, string(body))
	}
	assertEnvelopeCode(t, body, v1.CodeInvalidRequest)
	if !strings.Contains(string(body), "runtime validate") {
		t.Fatalf("错误信息应指路 runtime validate：%s", string(body))
	}
}

func TestRuntimeActionReportsSpecNotFound(t *testing.T) {
	server, _ := newFullServer(t, withRuntime(&stubRuntimeAdapter{}, nil))
	createApplication(t, server, "empty-app")

	response, body := postRuntimeAction(t, server.URL+"/api/v1/applications/empty-app/runtime/start", "")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", response.StatusCode)
	}
	assertEnvelopeCode(t, body, v1.CodeSpecNotFound)
}

func TestRuntimeHealthEndpoint(t *testing.T) {
	checkedAt := time.Now().UTC().Truncate(time.Second)
	adapter := &stubRuntimeAdapter{
		health: domain.RuntimeHealth{Ready: true, CheckedAt: checkedAt, Detail: "127.0.0.1:8080 可连接"},
	}
	server, _ := newFullServer(t, withRuntime(adapter, nil))
	app := runtimeReadyApp(t, server, "billing-api")

	response, body := getForTest(t, server.URL+"/api/v1/applications/billing-api/runtime/health")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", response.StatusCode, string(body))
	}
	var decoded v1.RuntimeHealthResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode from %q: %v", string(body), err)
	}
	if !decoded.Ready || decoded.Application != app.ID {
		t.Fatalf("unexpected response: %+v", decoded)
	}
	if !decoded.CheckedAt.Equal(checkedAt) || decoded.Detail == "" {
		t.Fatalf("health 快照不完整：%+v", decoded)
	}

	// 未就绪快照不是错误：ready=false 与 200 共存，调用方据此轮询。
	adapter.health = domain.RuntimeHealth{CheckedAt: checkedAt, Detail: "unit 未在运行（inactive）"}
	notReadyResponse, notReadyBody := getForTest(t, server.URL+"/api/v1/applications/billing-api/runtime/health")
	if notReadyResponse.StatusCode != http.StatusOK {
		t.Fatalf("未就绪快照 want 200, got %d", notReadyResponse.StatusCode)
	}
	var snapshot v1.RuntimeHealthResponse
	if err := json.Unmarshal(notReadyBody, &snapshot); err != nil {
		t.Fatalf("decode from %q: %v", string(notReadyBody), err)
	}
	if snapshot.Ready || snapshot.Detail == "" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}

	// 确定性失败（unit failed、启动超时）才是错误码。
	adapter.healthErr = domain.NewError(v1.CodeRuntimeNotReady, "unit 处于 failed 状态")
	errorResponse, errorBody := getForTest(t, server.URL+"/api/v1/applications/billing-api/runtime/health")
	if errorResponse.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", errorResponse.StatusCode)
	}
	assertEnvelopeCode(t, errorBody, v1.CodeRuntimeNotReady)
}

func TestRuntimeActionRejectsUnknownField(t *testing.T) {
	server, _ := newFullServer(t, withRuntime(&stubRuntimeAdapter{}, nil))
	runtimeReadyApp(t, server, "billing-api")

	response, body := postRuntimeAction(t, server.URL+"/api/v1/applications/billing-api/runtime/start",
		`{"unknown":1}`)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", response.StatusCode, string(body))
	}
	assertEnvelopeCode(t, body, v1.CodeInvalidRequest)
}

func mustRequest(t *testing.T, url, idempotencyKey string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Idempotency-Key", idempotencyKey)
	return request
}

// runtime.* 也允许声明重试（1d 决策 3），因此 retry 段必须一路透传到 Service.Create，
// 而不是在 HTTP 层被静默丢掉。
func TestRuntimeActionCarriesRetryPolicy(t *testing.T) {
	server, _ := newFullServer(t, withRuntime(&stubRuntimeAdapter{}, nil))
	runtimeReadyApp(t, server, "billing-api")

	request, err := http.NewRequest(http.MethodPost,
		server.URL+"/api/v1/applications/billing-api/runtime/start",
		strings.NewReader(`{"createdBy":"alice","retry":{"maxAttempts":3,"baseDelaySeconds":5}}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post start: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", response.StatusCode)
	}

	var decoded v1.OperationResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// maxAttempts 来自请求而不是默认的 1，这就是「策略真的落到了操作上」的证据。
	if decoded.Operation.MaxAttempts != 3 {
		t.Fatalf("maxAttempts want 3（retry 未被透传到 Service.Create）, got %d", decoded.Operation.MaxAttempts)
	}
	if decoded.Operation.Attempt != 1 {
		t.Fatalf("第一跳的 attempt want 1, got %d", decoded.Operation.Attempt)
	}
}

// 反面对照：非法策略必须被报成 RETRY_POLICY_INVALID。
//
// 刻意不注入适配器——校验排在「适配器是否可用」之前，所以如果 HTTP 层把 retry 段丢了，
// 这个用例会拿到 RUNTIME_UNSUPPORTED 而失败，正好钉住透传这件事。
func TestRuntimeActionRejectsBadRetryPolicy(t *testing.T) {
	server, _ := newFullServer(t)
	runtimeReadyApp(t, server, "billing-api")

	request, err := http.NewRequest(http.MethodPost,
		server.URL+"/api/v1/applications/billing-api/runtime/start",
		strings.NewReader(`{"retry":{"maxAttempts":99}}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post start: %v", err)
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", response.StatusCode, string(raw))
	}
	assertEnvelopeCode(t, raw, v1.CodeRetryPolicyInvalid)
}

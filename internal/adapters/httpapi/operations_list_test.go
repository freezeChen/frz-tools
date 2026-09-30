package httpapi

import (
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
	"github.com/freezeChen/frz-tools/internal/adapters/executor"
	"github.com/freezeChen/frz-tools/internal/adapters/sqlite"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// newListTestServer 与 server_test.go 的 newTestServer 同一装配，唯一区别是
// **启动了 worker 池**：status 过滤的用例需要操作真的被领取、执行到终态
// （与真实 opsd 的形态一致），而不是停在 pending。
func newListTestServer(t *testing.T) *httptest.Server {
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
	runtime := application.NewRuntime(application.Options{
		Repo:            store,
		Executor:        exec,
		AllowExecutable: allow,
		Defaults:        defaults,
		Workers:         1,
		Idle:            10 * time.Millisecond,
		Logger:          logger,
	})

	ctx, cancel := context.WithCancel(context.Background())
	runtime.Pool.Start(ctx)
	t.Cleanup(cancel)

	server := httptest.NewServer(NewServer(Dependencies{
		Service: runtime.Service,
		Store:   store,
		Workers: 1,
		Logger:  logger,
	}).Handler())
	t.Cleanup(server.Close)
	return server
}

// submitListOperation 经 POST /api/v1/operations 提交一个 executor.command 操作，
// 返回创建出的 Operation。
func submitListOperation(t *testing.T, baseURL, resource string, argv ...string) v1.Operation {
	t.Helper()
	encoded := make([]string, 0, len(argv))
	for _, arg := range argv {
		encodedArg, err := json.Marshal(arg)
		if err != nil {
			t.Fatalf("marshal argv: %v", err)
		}
		encoded = append(encoded, string(encodedArg))
	}
	_, body := postJSON(t, baseURL+"/api/v1/operations", fmt.Sprintf(
		`{"kind":"executor.command","resource":%q,"spec":{"argv":[%s]}}`, resource, joinComma(encoded)))
	return body.Operation
}

func joinComma(parts []string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += ","
		}
		out += part
	}
	return out
}

func getOperationsList(t *testing.T, baseURL, query string) (int, v1.ErrorResponse, v1.OperationListResponse) {
	t.Helper()
	response, err := http.Get(baseURL + "/api/v1/operations" + query)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	var errBody v1.ErrorResponse
	var list v1.OperationListResponse
	if response.StatusCode >= 400 {
		if err := json.NewDecoder(response.Body).Decode(&errBody); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
	} else if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return response.StatusCode, errBody, list
}

// 列表端点的基本契约（迭代 6 规格 §5）：默认 10 条、按创建时间倒序、
// limit 越界/非数字报 400 INVALID_REQUEST（不新增错误码）。
func TestOperationsListDefaultsAndLimits(t *testing.T) {
	server := newListTestServer(t)

	const total = 12
	created := make([]string, 0, total)
	for i := 0; i < total; i++ {
		op := submitListOperation(t, server.URL, fmt.Sprintf("list-demo-%d", i), "/usr/bin/true")
		created = append(created, op.ID)
	}

	// 不带参数：默认只回 10 条。
	_, _, list := getOperationsList(t, server.URL, "")
	if len(list.Operations) != 10 {
		t.Fatalf("默认应当返回 10 条，得到 %d", len(list.Operations))
	}
	if list.APIVersion != v1.APIVersion {
		t.Fatalf("response must carry apiVersion, got %q", list.APIVersion)
	}

	// 按创建时间倒序：后创建的在前。
	for i := 1; i < len(list.Operations); i++ {
		if list.Operations[i-1].CreatedAt.Before(list.Operations[i].CreatedAt) {
			t.Fatalf("列表应当按创建时间倒序：%s 晚于 %s 却排在后面",
				list.Operations[i-1].ID, list.Operations[i].ID)
		}
	}

	// limit=100：全部 12 条都拿得到，元素与单条查询同构。
	status, _, full := getOperationsList(t, server.URL, "?limit=100")
	if status != http.StatusOK || len(full.Operations) != total {
		t.Fatalf("limit=100 应当返回全部 %d 条，得到 %d 条（HTTP %d）", total, len(full.Operations), status)
	}
	seen := map[string]bool{}
	for _, op := range full.Operations {
		seen[op.ID] = true
		if op.Kind != "executor.command" || op.Status == "" {
			t.Fatalf("列表元素应当与单条查询同构：%+v", op)
		}
	}
	for _, id := range created {
		if !seen[id] {
			t.Fatalf("limit=100 应当包含 %s", id)
		}
	}

	// 越界与非数字：400 INVALID_REQUEST。
	for query, why := range map[string]string{
		"?limit=101": "上限 100",
		"?limit=0":   "必须至少 1",
		"?limit=-3":  "必须至少 1",
		"?limit=abc": "必须是整数",
	} {
		status, errBody, _ := getOperationsList(t, server.URL, query)
		if status != http.StatusBadRequest || errBody.Code != v1.CodeInvalidRequest {
			t.Fatalf("%s（%s）应当是 400 INVALID_REQUEST，得到 HTTP %d code %s", query, why, status, errBody.Code)
		}
	}
}

// status 过滤按单个状态值生效；取值必须是状态机里存在的状态。
func TestOperationsListStatusFilter(t *testing.T) {
	server := newListTestServer(t)

	const succeededCount = 2
	succeeded := make(map[string]bool, succeededCount)
	for i := 0; i < succeededCount; i++ {
		op := submitListOperation(t, server.URL, fmt.Sprintf("filter-ok-%d", i), "/usr/bin/true")
		succeeded[op.ID] = true
	}
	failed := submitListOperation(t, server.URL, "filter-fail", "/usr/bin/false")
	// sleep 让操作停在 running：1 秒足够下面的轮询窗口。
	running := submitListOperation(t, server.URL, "filter-running", "/bin/sleep", "1")

	// 等 quickly-exit 的操作落到终态，避免过滤结果带时序。
	deadline := time.Now().Add(20 * time.Second)
	for {
		status, _, list := getOperationsList(t, server.URL, "?limit=100&status=succeeded")
		if status != http.StatusOK {
			t.Fatalf("status=succeeded 应当是 200，得到 %d", status)
		}
		if len(list.Operations) == succeededCount {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("终态操作没有如期出现：想要 %d 条 succeeded，得到 %d", succeededCount, len(list.Operations))
		}
		time.Sleep(25 * time.Millisecond)
	}

	// succeeded：只有那两条。
	_, _, list := getOperationsList(t, server.URL, "?status=succeeded")
	if len(list.Operations) != succeededCount {
		t.Fatalf("status=succeeded 应当只有 %d 条，得到 %d", succeededCount, len(list.Operations))
	}
	for _, op := range list.Operations {
		if op.Status != string(domain.StatusSucceeded) {
			t.Fatalf("status=succeeded 不该混进 %s 的操作：%s", op.Status, op.ID)
		}
	}

	// failed / running：同样是「有没有如期出现」的轮询，因为 worker 领取与执行
	// 都有窗口，单次查询的结果带时序。
	deadlineFailed := time.Now().Add(20 * time.Second)
	for {
		_, _, list = getOperationsList(t, server.URL, "?status=failed")
		if len(list.Operations) == 1 && list.Operations[0].ID == failed.ID {
			break
		}
		if time.Now().After(deadlineFailed) {
			t.Fatalf("status=failed 应当只回 %s，一直没等到，最后得到 %+v", failed.ID, list.Operations)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// running：轮询到 sleep 操作被 worker 领走。
	deadline = time.Now().Add(15 * time.Second)
	for {
		_, _, list = getOperationsList(t, server.URL, "?status=running")
		found := false
		for _, op := range list.Operations {
			if op.Status != string(domain.StatusRunning) {
				t.Fatalf("status=running 不该混进 %s 的操作：%s", op.Status, op.ID)
			}
			if op.ID == running.ID {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("running 操作没有被列出：%s 一直不在 status=running 的结果里", running.ID)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// 未知状态值：400 INVALID_REQUEST（取值即状态机的状态值，不新增错误码）。
	status, errBody, _ := getOperationsList(t, server.URL, "?status=bogus")
	if status != http.StatusBadRequest || errBody.Code != v1.CodeInvalidRequest {
		t.Fatalf("未知状态应当是 400 INVALID_REQUEST，得到 HTTP %d code %s", status, errBody.Code)
	}

	// 收尾：等 sleep 操作到终态，别把仍在跑的操作留给同包后面的用例。
	deadline = time.Now().Add(20 * time.Second)
	for {
		response, err := http.Get(server.URL + "/api/v1/operations/" + running.ID)
		if err != nil {
			t.Fatalf("get operation: %v", err)
		}
		var body v1.OperationResponse
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			response.Body.Close()
			t.Fatalf("decode operation: %v", err)
		}
		response.Body.Close()
		if body.Operation.Status == string(domain.StatusSucceeded) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("sleep 操作没有如期结束，最后状态 %s", body.Operation.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

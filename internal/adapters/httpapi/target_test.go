package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// putTargets 发一次 PUT /targets/{app}。
func putTargets(t *testing.T, baseURL, application, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut,
		baseURL+"/api/v1/targets/"+application, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("put targets: %v", err)
	}
	return response
}

func mustPutTargets(t *testing.T, baseURL, application, body string) {
	t.Helper()
	response := putTargets(t, baseURL, application, body)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("put targets %s: %d", application, response.StatusCode)
	}
}

func mustCreateHost(t *testing.T, baseURL, name string) {
	t.Helper()
	mustCreateHostWithAddress(t, baseURL, name, "")
}

func mustCreateHostWithAddress(t *testing.T, baseURL, name, address string) {
	t.Helper()
	body, err := json.Marshal(v1.CreateHostRequest{Name: name, Address: address})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	response, err := http.Post(baseURL+"/api/v1/hosts", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("create host: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create host %s: %d", name, response.StatusCode)
	}
}

// getTargets / getAllTargets 走一次读端点并解出来。用已有的 getJSON 拿状态码与原文，
// 好让失败时的报错里带上服务端说了什么。
func getTargets(t *testing.T, url string) v1.TargetsResponse {
	t.Helper()
	response, body := getJSON(t, url)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("get %s: %d %s", url, response.StatusCode, string(body))
	}
	var out v1.TargetsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return out
}

func getAllTargets(t *testing.T, url string) v1.TargetsListResponse {
	t.Helper()
	response, body := getJSON(t, url)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("get %s: %d %s", url, response.StatusCode, string(body))
	}
	var out v1.TargetsListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return out
}

// 目标登记的最小闭环：先建主机记录，再登记，再读回来。
func TestTargetsRoundTripThroughAPI(t *testing.T) {
	server, _ := newFullServer(t)
	mustCreateHostWithAddress(t, server.URL, "web-1", "10.0.0.5:9443")
	mustCreateHost(t, server.URL, "web-2")

	mustPutTargets(t, server.URL, "orders-api", `{"hosts":["web-1","web-2"]}`)

	got := getTargets(t, server.URL+"/api/v1/targets/orders-api")
	if got.Targets.Application != "orders-api" || len(got.Targets.Hosts) != 2 {
		t.Fatalf("登记结果不对：%+v", got.Targets)
	}
	// 地址从主机表解析：web-2 的 address 为空 = 本机，这是 5a 就定下的语义。
	if got.Targets.Hosts[0].Address != "10.0.0.5:9443" || got.Targets.Hosts[1].Address != "" {
		t.Fatalf("地址没解析对：%+v", got.Targets.Hosts)
	}
}

// 写入是**替换**语义：再 set 一次只给一台，读回来就只有一台。
func TestTargetsSetReplacesTheWholeList(t *testing.T) {
	server, _ := newFullServer(t)
	mustCreateHost(t, server.URL, "web-1")
	mustCreateHost(t, server.URL, "web-2")

	mustPutTargets(t, server.URL, "orders-api", `{"hosts":["web-1","web-2"]}`)
	mustPutTargets(t, server.URL, "orders-api", `{"hosts":["web-2"]}`)

	got := getTargets(t, server.URL+"/api/v1/targets/orders-api")
	if len(got.Targets.Hosts) != 1 || got.Targets.Hosts[0].Name != "web-2" {
		t.Fatalf("替换语义没生效：%+v", got.Targets)
	}
}

// 登记一个不存在的主机名当场被拒，而且**注册表一字未改**——写进去一半比不写更糟：
// 那会让下一次批量发布少发一台，而少发的那台看起来像是「本来就不该发」。
func TestTargetsRejectsUnknownHostAndChangesNothing(t *testing.T) {
	server, _ := newFullServer(t)
	mustCreateHost(t, server.URL, "web-1")
	mustPutTargets(t, server.URL, "orders-api", `{"hosts":["web-1"]}`)

	response := putTargets(t, server.URL, "orders-api", `{"hosts":["web-1","typo-host"]}`)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 (HOST_NOT_FOUND), got %d", response.StatusCode)
	}
	var apiError v1.ErrorResponse
	if err := json.NewDecoder(response.Body).Decode(&apiError); err != nil {
		t.Fatalf("decode: %v", err)
	}
	response.Body.Close()
	if apiError.Code != v1.CodeHostNotFound {
		t.Fatalf("want HOST_NOT_FOUND, got %s", apiError.Code)
	}

	got := getTargets(t, server.URL+"/api/v1/targets/orders-api")
	if len(got.Targets.Hosts) != 1 || got.Targets.Hosts[0].Name != "web-1" {
		t.Fatalf("失败的写入动了注册表：%+v", got.Targets)
	}
}

func TestTargetsRejectsEmptyList(t *testing.T) {
	server, _ := newFullServer(t)
	response := putTargets(t, server.URL, "orders-api", `{"hosts":[]}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", response.StatusCode)
	}
}

// 跨主机汇总：一次列出所有应用的部署目标——这是 5a 欠下的那一条。
func TestTargetsListCoversAllApplications(t *testing.T) {
	server, _ := newFullServer(t)
	mustCreateHost(t, server.URL, "web-1")
	mustCreateHost(t, server.URL, "web-2")

	mustPutTargets(t, server.URL, "orders-api", `{"hosts":["web-1"]}`)
	mustPutTargets(t, server.URL, "billing-api", `{"hosts":["web-1","web-2"]}`)

	all := getAllTargets(t, server.URL+"/api/v1/targets")
	if len(all.Items) != 2 {
		t.Fatalf("应当有两个应用，得到 %d", len(all.Items))
	}
	if all.Items[0].Application != "billing-api" || len(all.Items[0].Hosts) != 2 {
		t.Fatalf("按应用名排序、带全部目标：%+v", all.Items)
	}
	if all.Items[1].Application != "orders-api" || len(all.Items[1].Hosts) != 1 {
		t.Fatalf("%+v", all.Items[1])
	}
}

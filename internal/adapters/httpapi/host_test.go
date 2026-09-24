package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

func postJSONForTest(t *testing.T, url string, body any) (*http.Response, []byte) {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	response, err := http.Post(url, "application/json", bytes.NewReader(payload))
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

func getForTest(t *testing.T, url string) (*http.Response, []byte) {
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

func TestCreateAndListHosts(t *testing.T) {
	server, _ := newFullServer(t)

	response, body := postJSONForTest(t, server.URL+"/api/v1/hosts", v1.CreateHostRequest{
		Name:    "web-01",
		Address: "10.0.0.11",
		Labels:  map[string]string{"role": "web"},
	})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create host want 201, got %d: %s", response.StatusCode, string(body))
	}
	var created v1.HostResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode host from %q: %v", string(body), err)
	}
	if created.APIVersion != v1.APIVersion {
		t.Fatalf("host response must carry apiVersion, got %q", created.APIVersion)
	}
	if created.Host.ID == "" || created.Host.Name != "web-01" || created.Host.Address != "10.0.0.11" {
		t.Fatalf("unexpected host: %+v", created.Host)
	}
	if created.Host.Labels["role"] != "web" {
		t.Fatalf("labels were not persisted: %+v", created.Host.Labels)
	}

	listResponse, listBody := getForTest(t, server.URL+"/api/v1/hosts")
	if listResponse.StatusCode != http.StatusOK {
		t.Fatalf("list hosts want 200, got %d", listResponse.StatusCode)
	}
	var list v1.HostListResponse
	if err := json.Unmarshal(listBody, &list); err != nil {
		t.Fatalf("decode host list from %q: %v", string(listBody), err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != created.Host.ID {
		t.Fatalf("want exactly the created host, got %+v", list.Items)
	}

	// 单个主机同时接受 ID 与名称引用。
	byName, byNameBody := getForTest(t, server.URL+"/api/v1/hosts/web-01")
	if byName.StatusCode != http.StatusOK {
		t.Fatalf("get host by name want 200, got %d: %s", byName.StatusCode, string(byNameBody))
	}
	var fetched v1.HostResponse
	if err := json.Unmarshal(byNameBody, &fetched); err != nil {
		t.Fatalf("decode host from %q: %v", string(byNameBody), err)
	}
	if fetched.Host.ID != created.Host.ID {
		t.Fatalf("按名称取到的主机与创建结果不一致：%+v", fetched.Host)
	}
}

func TestGetHostReportsNotFound(t *testing.T) {
	server, _ := newFullServer(t)

	response, body := getForTest(t, server.URL+"/api/v1/hosts/no-such-host")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", response.StatusCode)
	}
	assertEnvelopeCode(t, body, v1.CodeHostNotFound)
}

func TestCreateHostRejectsInvalidInput(t *testing.T) {
	server, _ := newFullServer(t)

	emptyResponse, emptyBody := postJSONForTest(t, server.URL+"/api/v1/hosts", v1.CreateHostRequest{Name: "  "})
	if emptyResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("空名称 want 400, got %d", emptyResponse.StatusCode)
	}
	assertEnvelopeCode(t, emptyBody, v1.CodeInvalidRequest)

	if response, body := postJSONForTest(t, server.URL+"/api/v1/hosts", v1.CreateHostRequest{Name: "web-01"}); response.StatusCode != http.StatusCreated {
		t.Fatalf("create host want 201, got %d: %s", response.StatusCode, string(body))
	}
	// name 有唯一约束；重复创建必须是可诊断的错误，而不是 500。
	duplicateResponse, duplicateBody := postJSONForTest(t, server.URL+"/api/v1/hosts", v1.CreateHostRequest{Name: "web-01"})
	if duplicateResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("重名 want 400, got %d: %s", duplicateResponse.StatusCode, string(duplicateBody))
	}
	assertEnvelopeCode(t, duplicateBody, v1.CodeInvalidRequest)
}

func TestCreateHostRejectsUnknownField(t *testing.T) {
	server, _ := newFullServer(t)

	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/hosts",
		bytes.NewBufferString(`{"name":"web-01","unexpected":1}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", response.StatusCode)
	}
	assertErrorCode(t, response, v1.CodeInvalidRequest)
}

func TestCreateAndGetEnvironment(t *testing.T) {
	server, _ := newFullServer(t)

	response, body := postJSONForTest(t, server.URL+"/api/v1/environments", v1.CreateEnvironmentRequest{
		Name:   "production",
		Labels: map[string]string{"tier": "prod"},
	})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create environment want 201, got %d: %s", response.StatusCode, string(body))
	}
	var created v1.EnvironmentResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode environment from %q: %v", string(body), err)
	}
	if created.APIVersion != v1.APIVersion || created.Environment.ID == "" {
		t.Fatalf("unexpected environment: %+v", created)
	}

	listResponse, listBody := getForTest(t, server.URL+"/api/v1/environments")
	if listResponse.StatusCode != http.StatusOK {
		t.Fatalf("list environments want 200, got %d", listResponse.StatusCode)
	}
	var list v1.EnvironmentListResponse
	if err := json.Unmarshal(listBody, &list); err != nil {
		t.Fatalf("decode environment list from %q: %v", string(listBody), err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != created.Environment.ID || list.Items[0].Labels["tier"] != "prod" {
		t.Fatalf("want exactly the created environment, got %+v", list.Items)
	}

	byName, byNameBody := getForTest(t, server.URL+"/api/v1/environments/production")
	if byName.StatusCode != http.StatusOK {
		t.Fatalf("get environment by name want 200, got %d: %s", byName.StatusCode, string(byNameBody))
	}
	var fetched v1.EnvironmentResponse
	if err := json.Unmarshal(byNameBody, &fetched); err != nil {
		t.Fatalf("decode environment from %q: %v", string(byNameBody), err)
	}
	if fetched.Environment.ID != created.Environment.ID {
		t.Fatalf("按名称取到的环境与创建结果不一致：%+v", fetched.Environment)
	}
}

func TestGetEnvironmentReportsNotFound(t *testing.T) {
	server, _ := newFullServer(t)

	response, body := getForTest(t, server.URL+"/api/v1/environments/no-such-env")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", response.StatusCode)
	}
	assertEnvelopeCode(t, body, v1.CodeEnvNotFound)
}

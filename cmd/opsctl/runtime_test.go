package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// newStubCLI 在 unix socket 上跑一个桩 handler，并返回可直接喂给命令行的 socket 路径。
// runtime 子命令的接线（路径、旗标、退出码、输出）都在这里被钉住，真机语义属于 A7。
func newStubCLI(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()

	socket := filepath.Join(t.TempDir(), "opsd.sock")
	// macOS 的 unix socket 路径上限是 104 字节，$TMPDIR 在 CI 上可能更长。
	if len(socket) > 100 {
		dir, err := os.MkdirTemp("/tmp", "frz-cli-")
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
	return socket
}

// runCommand 执行一次 opsctl 并捕获 stdout：命令的第一手产物是给人看的文本，
// 退出码则由返回的 error 决定（main 用它调用 v1.ExitCode）。
func runCommand(t *testing.T, socket string, args ...string) (string, error) {
	t.Helper()

	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = original })

	root := newRootCommand()
	root.SetArgs(append([]string{"--socket", socket}, args...))
	executeErr := root.Execute()

	writer.Close()
	os.Stdout = original
	captured, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	return string(captured), executeErr
}

func writeJSONForCLI(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode: %v", err)
	}
}

func writeErrorForCLI(t *testing.T, w http.ResponseWriter, status int, code v1.ErrorCode, message string) {
	t.Helper()
	writeJSONForCLI(t, w, status, v1.ErrorResponse{APIVersion: v1.APIVersion, Code: code, Message: message})
}

func TestRuntimeHealthCommandExitsNotReady(t *testing.T) {
	never := make(chan struct{})
	defer close(never)

	socket := newStubCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/applications/billing-api/runtime/health" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		writeJSONForCLI(t, w, http.StatusOK, v1.RuntimeHealthResponse{
			APIVersion: v1.APIVersion, Application: "app_1", Ready: false, Detail: "unit 未在运行（inactive）",
		})
	})

	stdout, err := runCommand(t, socket, "runtime", "health", "--app", "billing-api")
	if err == nil {
		t.Fatal("未就绪必须让命令失败，否则脚本无法用它判断")
	}
	if domain.CodeOf(err) != v1.CodeRuntimeNotReady {
		t.Fatalf("want RUNTIME_NOT_READY, got %s", domain.CodeOf(err))
	}
	if v1.ExitCode(domain.CodeOf(err)) != 19 {
		t.Fatalf("退出码 want 19, got %d", v1.ExitCode(domain.CodeOf(err)))
	}
	if !strings.Contains(stdout, "未就绪") || !strings.Contains(stdout, "inactive") {
		t.Fatalf("输出应说明未就绪与原因：%q", stdout)
	}
}

func TestRuntimeHealthCommandSucceedsWhenReady(t *testing.T) {
	socket := newStubCLI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONForCLI(t, w, http.StatusOK, v1.RuntimeHealthResponse{
			APIVersion: v1.APIVersion, Application: "app_1", Ready: true,
			CheckedAt: time.Now().UTC(), Detail: "127.0.0.1:8080 可连接",
		})
	})

	stdout, err := runCommand(t, socket, "runtime", "health", "--app", "billing-api", "--json")
	if err != nil {
		t.Fatalf("ready 时不应失败：%v", err)
	}
	var decoded v1.RuntimeHealthResponse
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("--json 输出必须是响应本身：%q", stdout)
	}
	if !decoded.Ready {
		t.Fatalf("unexpected response: %+v", decoded)
	}
}

func TestRuntimeStartCommandPrintsOperationID(t *testing.T) {
	socket := newStubCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("start want POST, got %s", r.Method)
		}
		writeJSONForCLI(t, w, http.StatusAccepted, v1.OperationResponse{
			APIVersion: v1.APIVersion,
			Operation: v1.Operation{
				ID: "op_abc", Kind: v1.KindRuntimeStart, Resource: "app_1",
				Status: string(domain.StatusPending), CreatedAt: time.Now().UTC(),
			},
		})
	})

	stdout, err := runCommand(t, socket, "runtime", "start", "--app", "billing-api")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(stdout, "op_abc") || !strings.Contains(stdout, "operation logs op_abc") {
		t.Fatalf("start 必须打印 operationId 与查询方式：%q", stdout)
	}
}

func TestRuntimeValidateAndPrepareCommands(t *testing.T) {
	socket := newStubCLI(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/applications/billing-api/runtime/validate":
			writeJSONForCLI(t, w, http.StatusOK, v1.RuntimeValidateResponse{
				APIVersion: v1.APIVersion, Application: "app_1", Valid: true,
			})
		case "/api/v1/applications/billing-api/runtime/prepare":
			writeJSONForCLI(t, w, http.StatusOK, v1.RuntimePrepareResponse{
				APIVersion: v1.APIVersion, Application: "app_1", Prepared: true,
				Decision: &v1.RuntimeDecision{
					UnitName: "billing-api.service", UnitPath: "/etc/systemd/system/billing-api.service",
					Tier: "legacy", SystemdVersion: 232, Degradations: []string{"日志改为 journal"},
				},
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	validateOut, err := runCommand(t, socket, "runtime", "validate", "--app", "billing-api")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !strings.Contains(validateOut, "billing-api") {
		t.Fatalf("validate 输出不对：%q", validateOut)
	}

	prepareOut, err := runCommand(t, socket, "runtime", "prepare", "--app", "billing-api")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// 档位与版本必须能被人看到：这是「同一份 manifest 在不同主机上 unit 不同」的解释来源。
	for _, want := range []string{"billing-api.service", "legacy", "232", "journal"} {
		if !strings.Contains(prepareOut, want) {
			t.Fatalf("prepare 输出缺少 %q：%q", want, prepareOut)
		}
	}
}

func TestRuntimeCommandRequiresApp(t *testing.T) {
	socket := newStubCLI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("缺少 --app 时不应发出请求")
	})

	_, err := runCommand(t, socket, "runtime", "start")
	if domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}

	_, err = runCommand(t, socket, "runtime", "health")
	if domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
}

func TestRuntimeCommandSurfacesUnsupported(t *testing.T) {
	socket := newStubCLI(t, func(w http.ResponseWriter, r *http.Request) {
		writeErrorForCLI(t, w, http.StatusConflict, v1.CodeRuntimeUnsupport, "本机没有可用的运行时适配器")
	})

	_, err := runCommand(t, socket, "runtime", "start", "--app", "billing-api")
	if domain.CodeOf(err) != v1.CodeRuntimeUnsupport || v1.ExitCode(domain.CodeOf(err)) != 21 {
		t.Fatalf("want RUNTIME_UNSUPPORTED/21, got %s/%d", domain.CodeOf(err), v1.ExitCode(domain.CodeOf(err)))
	}
}

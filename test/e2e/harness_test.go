package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

var (
	opsdBinary   string
	opsctlBinary string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "frz-tools-e2e-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create temp dir:", err)
		os.Exit(1)
	}
	opsdBinary = filepath.Join(dir, "opsd")
	opsctlBinary = filepath.Join(dir, "opsctl")

	for _, target := range [][2]string{
		{"./cmd/opsd", opsdBinary},
		{"./cmd/opsctl", opsctlBinary},
	} {
		build := exec.Command("go", "build", "-o", target[1], target[0])
		build.Dir = "../.."
		if out, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "cannot build %s: %v\n%s\n", target[0], err, out)
			os.RemoveAll(dir)
			os.Exit(1)
		}
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type daemon struct {
	cmd          *exec.Cmd
	dir          string
	socket       string
	database     string
	workDir      string
	logDir       string
	artifactsDir string
	backupsDir   string
	secretsDir   string
	configPath   string
	// baseConfig 是不含 remote 段的那份配置；remote 是追加段（迭代 5a）。
	baseConfig string
	remote     string
}

// shortTempDir 返回路径足够短的临时目录，以满足 unix socket 的路径长度限制
// （macOS 为 104 字节）；当 $TMPDIR 过长时回落到 /tmp。
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "frz-opsd-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	if len(filepath.Join(dir, "opsd.sock")) > 100 {
		os.RemoveAll(dir)
		dir, err = os.MkdirTemp("/tmp", "frz-opsd-")
		if err != nil {
			t.Fatalf("temp dir: %v", err)
		}
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newDaemon(t *testing.T) *daemon {
	t.Helper()
	dir := shortTempDir(t)
	d := &daemon{
		dir:          dir,
		socket:       filepath.Join(dir, "opsd.sock"),
		database:     filepath.Join(dir, "opsd.db"),
		workDir:      filepath.Join(dir, "work"),
		logDir:       filepath.Join(dir, "log"),
		artifactsDir: filepath.Join(dir, "artifacts"),
		backupsDir:   filepath.Join(dir, "backups"),
		secretsDir:   filepath.Join(dir, "secrets"),
		configPath:   filepath.Join(dir, "opsd.yaml"),
	}
	if err := os.MkdirAll(d.secretsDir, 0o700); err != nil {
		t.Fatalf("create secrets dir: %v", err)
	}
	cfg := fmt.Sprintf(`apiVersion: ops.frz.io/v1alpha1
kind: OpsdConfig
socket:
  path: %s
  mode: "0660"
database:
  path: %s
runtime:
  workDirectory: %s
  logDirectory: %s
  workers: 2
execution:
  defaultTimeoutSeconds: 30
  maxOutputBytes: 65536
  allowedPaths:
    - /bin
    - /usr/bin
    - /usr/local/bin
  sensitiveEnvKeys:
    - TOKEN
artifactStore:
  root: %s
  fileMode: "0640"
  dirMode: "0750"
  maxUploadBytes: 1048576
  quotaBytes: 16777216
# 备份用**独立的**存储根：与制品共用会让 artifact gc 把备份当孤儿删掉。
backupStore:
  root: %s
  fileMode: "0640"
  dirMode: "0750"
  quotaBytes: 16777216
secrets:
  allowedFileDirectories:
    - %s
sudo:
  allowedCommands: []
`, d.socket, d.database, d.workDir, d.logDir, d.artifactsDir, d.backupsDir, d.secretsDir)

	d.baseConfig = cfg
	d.writeConfig(t)
	return d
}

// writeConfig 把基础配置与可能存在的 remote 段写下去。允许重复调用：
// 「先起一个不听远程的守护进程，再补上远程段重启」这类用例需要它。
func (d *daemon) writeConfig(t *testing.T) {
	t.Helper()
	body := d.baseConfig
	if d.remote != "" {
		body += "\n" + d.remote
	}
	if err := os.WriteFile(d.configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// setRemote 补上 remote 段（必须在 start 之前调用）。
func (d *daemon) setRemote(t *testing.T, remote string) {
	t.Helper()
	d.remote = remote
	d.writeConfig(t)
}

func (d *daemon) start(t *testing.T) {
	t.Helper()
	cmd := exec.Command(opsdBinary, "--config", d.configPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start opsd: %v", err)
	}
	d.cmd = cmd
	t.Cleanup(func() { d.stop(t) })
	d.waitHealthy(t)
}

func (d *daemon) waitHealthy(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, _, err := runOpsctl(t, d.socket, "health"); err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return
	}
	t.Fatalf("opsd never became healthy: %v", lastErr)
}

func (d *daemon) stop(t *testing.T) {
	t.Helper()
	if d.cmd == nil || d.cmd.Process == nil {
		return
	}
	_ = d.cmd.Process.Kill()
	_, _ = d.cmd.Process.Wait()
	d.cmd = nil
}

func (d *daemon) kill(t *testing.T) {
	t.Helper()
	if d.cmd == nil || d.cmd.Process == nil {
		t.Fatal("daemon is not running")
	}
	if err := d.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill opsd: %v", err)
	}
	state, err := d.cmd.Process.Wait()
	if err != nil {
		t.Fatalf("wait for killed opsd: %v", err)
	}
	if state.Exited() && state.ExitCode() == 0 {
		t.Fatal("expected a non-zero exit from a killed daemon")
	}
	d.cmd = nil
}

// runOpsdExpectingFailure 起一个**注定起不来**的 opsd，等它退出并取回它的输出。
//
// 「拒绝启动」这件事必须真的去起一次才知道：配置校验通过、而启动路径上还有一道
// 自己的检查（私钥文件模式）时，只测配置解析会漏掉后者。
func runOpsdExpectingFailure(t *testing.T, d *daemon) string {
	t.Helper()
	cmd := exec.Command(opsdBinary, "--config", d.configPath)
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start opsd: %v", err)
	}
	// 必须用 cmd.Wait 而不是 cmd.Process.Wait：前者会等输出拷贝的 goroutine 结束，
	// 后者不会——那样在 -race 下就是一次「测试在读 strings.Builder、exec 在写它」。
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("opsd 应当因为配置非法而失败退出，但它正常退出了：\n%s", output.String())
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("opsd 应当因为配置非法立刻退出，但它还在跑：\n%s", output.String())
	}
	return output.String()
}

func runOpsctl(t *testing.T, socket string, args ...string) (string, int, error) {
	t.Helper()
	full := append([]string{"--socket", socket}, args...)
	cmd := exec.Command(opsctlBinary, full...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
		err = fmt.Errorf("opsctl %v failed with exit code %d: %s", args, code, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), code, err
}

// runOpsctlRemote 走 `--remote` 那条路：不给 socket，给地址与身份三件套。
//
// 与 runOpsctl 分开而不是塞进同一个，是为了让用例一眼看出这次请求走的是哪条路
// ——「我明明给了 --remote，怎么打到了本机」正是这一片要防的错。
func runOpsctlRemote(t *testing.T, remoteAddr string, pki *testPKI, args ...string) (string, int, error) {
	t.Helper()
	full := append([]string{
		"--remote", remoteAddr,
		"--client-cert", pki.clientCert,
		"--client-key", pki.clientKey,
		"--ca-cert", pki.ca.certPath,
	}, args...)
	return runOpsctlBinary(t, full...)
}

// runOpsctlRemoteWith 允许覆盖身份三件套，用于「证书不对」那几类用例。
func runOpsctlRemoteWith(t *testing.T, args ...string) (string, int, error) {
	t.Helper()
	return runOpsctlBinary(t, args...)
}

func runOpsctlBinary(t *testing.T, args ...string) (string, int, error) {
	t.Helper()
	cmd := exec.Command(opsctlBinary, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
		err = fmt.Errorf("opsctl %v failed with exit code %d: %s", args, code, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), code, err
}

func submit(t *testing.T, socket, resource string, argv ...string) v1.Operation {
	t.Helper()
	args := append([]string{"operation", "submit", "--kind", v1.KindExecutorCommand, "--resource", resource, "--json", "--"}, argv...)
	stdout, _, err := runOpsctl(t, socket, args...)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return decodeOperation(t, stdout)
}

func getOperation(t *testing.T, socket, id string) v1.Operation {
	t.Helper()
	stdout, _, err := runOpsctl(t, socket, "operation", "get", id, "--json")
	if err != nil {
		t.Fatalf("get operation: %v", err)
	}
	return decodeOperation(t, stdout)
}

func decodeOperation(t *testing.T, raw string) v1.Operation {
	t.Helper()
	var op v1.Operation
	if err := json.Unmarshal([]byte(raw), &op); err != nil {
		t.Fatalf("cannot decode operation from %q: %v", raw, err)
	}
	return op
}

func waitForStatus(t *testing.T, socket, id string, want ...string) v1.Operation {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last v1.Operation
	for time.Now().Before(deadline) {
		last = getOperation(t, socket, id)
		for _, status := range want {
			if last.Status == status {
				return last
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("operation %s never reached %v, last status %s", id, want, last.Status)
	return last
}

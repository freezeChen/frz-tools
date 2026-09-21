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

	v1 "frz-tools/api/v1"
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
	cmd        *exec.Cmd
	dir        string
	socket     string
	database   string
	workDir    string
	logDir     string
	configPath string
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
		dir:        dir,
		socket:     filepath.Join(dir, "opsd.sock"),
		database:   filepath.Join(dir, "opsd.db"),
		workDir:    filepath.Join(dir, "work"),
		logDir:     filepath.Join(dir, "log"),
		configPath: filepath.Join(dir, "opsd.yaml"),
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
sudo:
  allowedCommands: []
`, d.socket, d.database, d.workDir, d.logDir)

	if err := os.WriteFile(d.configPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return d
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

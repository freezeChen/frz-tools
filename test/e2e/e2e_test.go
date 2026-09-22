package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

func TestHealthViaCLI(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	stdout, code, err := runOpsctl(t, d.socket, "health", "--json")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if code != 0 {
		t.Fatalf("want exit code 0, got %d", code)
	}
	if !strings.Contains(stdout, `"daemon": "ok"`) || !strings.Contains(stdout, `"database": "ok"`) {
		t.Fatalf("unexpected health output: %s", stdout)
	}
}

func TestDuplicateIdempotentSubmissionExecutesOnce(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	marker := filepath.Join(t.TempDir(), "executions.log")
	argv := []string{"/bin/sh", "-c", "echo run >> " + marker}

	submitArgs := func() string {
		stdout, _, err := runOpsctl(t, d.socket, "operation", "submit",
			"--kind", v1.KindExecutorCommand,
			"--resource", "idempotent-demo",
			"--idempotency-key", "fixed-key",
			"--json",
			"--", argv[0], argv[1], argv[2])
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		return stdout
	}

	first := decodeOperation(t, submitArgs())
	second := decodeOperation(t, submitArgs())

	if first.ID != second.ID {
		t.Fatalf("same idempotency key must reuse the operation: %s != %s", first.ID, second.ID)
	}

	waitForStatus(t, d.socket, first.ID, "succeeded")

	content, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker file: %v", err)
	}
	lines := strings.Count(strings.TrimSpace(string(content)), "\n") + 1
	if lines != 1 {
		t.Fatalf("the command must run exactly once, ran %d times", lines)
	}
}

func TestResourceIsFreeAgainAfterCompletion(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	first := submit(t, d.socket, "reusable", "/usr/bin/true")
	waitForStatus(t, d.socket, first.ID, "succeeded")

	second := submit(t, d.socket, "reusable", "/usr/bin/true")
	waitForStatus(t, d.socket, second.ID, "succeeded")
	if second.ID == first.ID {
		t.Fatal("the second operation must be a distinct operation")
	}
}

func TestSubmitRejectsBusyResource(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	running := submit(t, d.socket, "busy-resource", "/bin/sleep", "5")
	waitForStatus(t, d.socket, running.ID, "running")

	_, code, err := runOpsctl(t, d.socket, "operation", "submit",
		"--kind", v1.KindExecutorCommand,
		"--resource", "busy-resource",
		"--", "/usr/bin/true")
	if err == nil {
		t.Fatal("submitting against a busy resource must fail")
	}
	if code != 4 {
		t.Fatalf("LOCK_BUSY must exit with 4, got %d (%v)", code, err)
	}
}

func TestDryRunDoesNotRunTheCommand(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	marker := filepath.Join(t.TempDir(), "should-not-exist")
	_, _, err := runOpsctl(t, d.socket, "operation", "submit",
		"--kind", v1.KindExecutorCommand,
		"--resource", "dry-run-demo",
		"--dry-run",
		"--json",
		"--", "/bin/sh", "-c", "echo nope >> "+marker)
	if err != nil {
		t.Fatalf("dry-run submit: %v", err)
	}

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("dry run must not touch the filesystem, stat err = %v", err)
	}
}

func TestCancelRunningOperation(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	op := submit(t, d.socket, "cancel-me", "/bin/sleep", "30")
	waitForStatus(t, d.socket, op.ID, "running")

	if _, _, err := runOpsctl(t, d.socket, "operation", "cancel", op.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	final := waitForStatus(t, d.socket, op.ID, "cancelled")
	if final.ErrorCode != string(v1.CodeExecCancelled) {
		t.Fatalf("want EXEC_CANCELLED, got %q", final.ErrorCode)
	}
}

func TestRestartRecoversRunningOperation(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	op := submit(t, d.socket, "survivor", "/bin/sleep", "30")
	waitForStatus(t, d.socket, op.ID, "running")

	d.kill(t)
	d.start(t)

	recovered := waitForStatus(t, d.socket, op.ID, "failed")
	if recovered.ErrorCode != string(v1.CodeDaemonRestarted) {
		t.Fatalf("want DAEMON_RESTARTED, got %q", recovered.ErrorCode)
	}

	if _, _, err := runOpsctl(t, d.socket, "operation", "logs", op.ID); err != nil {
		t.Fatalf("logs after restart: %v", err)
	}
}

func TestRetryFailedOperationCreatesNewOperation(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	op := submit(t, d.socket, "retry-me", "/bin/sh", "-c", "exit 9")
	failed := waitForStatus(t, d.socket, op.ID, "failed")
	if failed.ErrorCode != string(v1.CodeExecExitNonZero) || failed.ExitCode == nil || *failed.ExitCode != 9 {
		t.Fatalf("unexpected failure record: %+v", failed)
	}

	stdout, _, err := runOpsctl(t, d.socket, "operation", "retry", op.ID, "--json")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	retried := decodeOperation(t, stdout)
	if retried.ID == op.ID || retried.RetryOf != op.ID {
		t.Fatalf("retry must create a new operation linked to %s, got %+v", op.ID, retried)
	}
}

func TestUnknownOperationExitsWithTwo(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	_, code, err := runOpsctl(t, d.socket, "operation", "get", "op_does_not_exist")
	if err == nil {
		t.Fatal("fetching an unknown operation must fail")
	}
	if code != 2 {
		t.Fatalf("OPERATION_NOT_FOUND must exit with 2, got %d (%v)", code, err)
	}
}

func TestLogsExposeExecutionTrace(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	op := submit(t, d.socket, "logs-demo", "/bin/echo", "hello-opsd")
	waitForStatus(t, d.socket, op.ID, "succeeded")

	stdout, _, err := runOpsctl(t, d.socket, "operation", "logs", op.ID)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(stdout, "operation finished") {
		t.Fatalf("logs must contain the finalisation entry: %s", stdout)
	}
	if !strings.Contains(stdout, "hello-opsd") {
		t.Fatalf("logs must contain captured stdout: %s", stdout)
	}
}

func TestConcurrentSameResourceSubmissionsOnlyOneSucceeds(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	type outcome struct {
		id  string
		err error
	}
	outcomes := make([]outcome, 3)

	var wg sync.WaitGroup
	for i := range outcomes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(opsctlBinary, "--socket", d.socket, "operation", "submit",
				"--kind", v1.KindExecutorCommand, "--resource", "contended", "--json", "--", "/bin/sleep", "0.5")
			raw, err := cmd.Output()
			if err != nil {
				outcomes[i] = outcome{err: err}
				return
			}
			var op v1.Operation
			if err := json.Unmarshal(raw, &op); err != nil {
				outcomes[i] = outcome{err: err}
				return
			}
			outcomes[i] = outcome{id: op.ID}
		}(i)
	}
	wg.Wait()

	accepted, rejected := 0, 0
	for _, result := range outcomes {
		if result.err == nil {
			accepted++
			continue
		}
		var exitErr *exec.ExitError
		if !errors.As(result.err, &exitErr) || exitErr.ExitCode() != 4 {
			t.Fatalf("a losing submission must exit with LOCK_BUSY (4), got %v", result.err)
		}
		rejected++
	}
	if accepted != 1 || rejected != 2 {
		t.Fatalf("want exactly one operation to acquire the resource, got %d accepted and %d rejected", accepted, rejected)
	}

	for _, result := range outcomes {
		if result.err == nil {
			waitForStatus(t, d.socket, result.id, "succeeded")
		}
	}
}

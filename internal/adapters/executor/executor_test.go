package executor

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/domain"
)

func allowAll(string) bool { return true }

func newExecutor(allow func(string) bool) *Executor {
	return New(allow, time.Second, 1024, []string{"TOKEN"})
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not available: %v", name, err)
	}
	return path
}

func TestRunReturnsStdoutAndZeroExit(t *testing.T) {
	sh := mustLookPath(t, "sh")
	result, err := newExecutor(allowAll).Run(context.Background(), domain.CommandSpec{
		Argv: []string{sh, "-c", "printf hello"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Stdout != "hello" {
		t.Fatalf("want stdout hello, got %q", result.Stdout)
	}
	if result.ExitCode != 0 || !result.Executed {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRunDoesNotInterpretShellMetacharacters(t *testing.T) {
	echo := mustLookPath(t, "echo")
	result, err := newExecutor(allowAll).Run(context.Background(), domain.CommandSpec{
		Argv: []string{echo, "$(whoami)"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "$(whoami)" {
		t.Fatalf("argv must not be shell-expanded, got %q", result.Stdout)
	}
}

func TestRunNonZeroExitPreservesCode(t *testing.T) {
	sh := mustLookPath(t, "sh")
	result, err := newExecutor(allowAll).Run(context.Background(), domain.CommandSpec{
		Argv: []string{sh, "-c", "exit 3"},
	})
	if domain.CodeOf(err) != v1.CodeExecExitNonZero {
		t.Fatalf("want EXEC_EXIT_NONZERO, got %v", err)
	}
	if result.ExitCode != 3 {
		t.Fatalf("want exit code 3, got %d", result.ExitCode)
	}
}

func TestRunTimeout(t *testing.T) {
	sh := mustLookPath(t, "sh")
	_, err := newExecutor(allowAll).Run(context.Background(), domain.CommandSpec{
		Argv:    []string{sh, "-c", "sleep 5"},
		Timeout: 100 * time.Millisecond,
	})
	if domain.CodeOf(err) != v1.CodeExecTimeout {
		t.Fatalf("want EXEC_TIMEOUT, got %v", err)
	}
}

func TestRunCancellation(t *testing.T) {
	sh := mustLookPath(t, "sh")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	_, err := newExecutor(allowAll).Run(ctx, domain.CommandSpec{
		Argv:    []string{sh, "-c", "sleep 5"},
		Timeout: 10 * time.Second,
	})
	if domain.CodeOf(err) != v1.CodeExecCancelled {
		t.Fatalf("want EXEC_CANCELLED, got %v", err)
	}
}

func TestRunTruncatesOutput(t *testing.T) {
	sh := mustLookPath(t, "sh")
	result, err := newExecutor(allowAll).Run(context.Background(), domain.CommandSpec{
		Argv:           []string{sh, "-c", "printf 0123456789"},
		MaxOutputBytes: 4,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Stdout != "0123" {
		t.Fatalf("want truncated stdout 0123, got %q", result.Stdout)
	}
	if !result.Truncated {
		t.Fatal("truncation must be reported")
	}
}

func TestDryRunValidatesWithoutExecuting(t *testing.T) {
	result, err := newExecutor(allowAll).Run(context.Background(), domain.CommandSpec{
		Argv:   []string{"/nonexistent/frz-tools-dry-run"},
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run must not fail on a missing binary: %v", err)
	}
	if result.Executed {
		t.Fatal("dry run must not execute a process")
	}
}

func TestDisallowedExecutableIsDenied(t *testing.T) {
	deny := func(string) bool { return false }
	_, err := newExecutor(deny).Run(context.Background(), domain.CommandSpec{
		Argv: []string{"/usr/bin/true"},
	})
	if domain.CodeOf(err) != v1.CodePermissionDenied {
		t.Fatalf("want PERMISSION_DENIED, got %v", err)
	}
}

func TestEmptyArgvIsInvalid(t *testing.T) {
	_, err := newExecutor(allowAll).Run(context.Background(), domain.CommandSpec{})
	if domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
}

func TestMissingExecutableIsInternal(t *testing.T) {
	_, err := newExecutor(allowAll).Run(context.Background(), domain.CommandSpec{
		Argv: []string{"/nonexistent/frz-tools-binary"},
	})
	if domain.CodeOf(err) != v1.CodeInternal {
		t.Fatalf("want INTERNAL, got %v", err)
	}
}

func TestRedactArgv(t *testing.T) {
	argv := []string{"/usr/bin/tool", "--token", "secret-value", "positional"}
	got := RedactArgv(argv, []int{2})
	want := []string{"/usr/bin/tool", "--token", "[redacted]", "positional"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: want %q, got %q", i, want[i], got[i])
		}
	}
	if argv[2] != "secret-value" {
		t.Fatal("RedactArgv must not mutate its input")
	}
}

func TestRedactEnv(t *testing.T) {
	env := map[string]string{"TOKEN": "abc", "PLAIN": "ok"}
	got := RedactEnv(env, []string{"TOKEN"})
	if got["TOKEN"] != "[redacted]" {
		t.Fatalf("TOKEN must be redacted, got %q", got["TOKEN"])
	}
	if got["PLAIN"] != "ok" {
		t.Fatalf("PLAIN must be preserved, got %q", got["PLAIN"])
	}
}

func TestSensitiveEnvKeysMergesDefaults(t *testing.T) {
	e := newExecutor(allowAll)
	keys := e.SensitiveEnvKeys([]string{"TOKEN", "PASSWORD"})
	if len(keys) != 2 {
		t.Fatalf("want deduplicated keys, got %v", keys)
	}
	found := map[string]bool{}
	for _, k := range keys {
		found[k] = true
	}
	if !found["TOKEN"] || !found["PASSWORD"] {
		t.Fatalf("unexpected keys: %v", keys)
	}
}

func TestMergeEnvOverridesAndKeepsRest(t *testing.T) {
	t.Setenv("FRZ_TOOLS_KEEP", "1")
	merged := mergeEnv(map[string]string{"FRZ_TOOLS_KEEP": "2", "FRZ_TOOLS_NEW": "3"})

	var keepCount int
	for _, kv := range merged {
		if strings.HasPrefix(kv, "FRZ_TOOLS_KEEP=") {
			keepCount++
			if kv != "FRZ_TOOLS_KEEP=2" {
				t.Fatalf("override must win, got %q", kv)
			}
		}
	}
	if keepCount != 1 {
		t.Fatalf("want exactly one FRZ_TOOLS_KEEP entry, got %d", keepCount)
	}
}

func TestExitErrorDetection(t *testing.T) {
	var exitErr *exec.ExitError
	if errors.As(nil, &exitErr) {
		t.Fatal("nil error must not be an ExitError")
	}
}

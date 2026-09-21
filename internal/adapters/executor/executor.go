package executor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/domain"
)

type Executor struct {
	allow            func(string) bool
	defaultTimeout   time.Duration
	defaultMaxOutput int64
	defaultSensitive []string
}

func New(allow func(string) bool, defaultTimeout time.Duration, defaultMaxOutput int64, sensitiveEnvKeys []string) *Executor {
	if defaultTimeout <= 0 {
		defaultTimeout = 300 * time.Second
	}
	if defaultMaxOutput <= 0 {
		defaultMaxOutput = 1 << 20
	}
	return &Executor{
		allow:            allow,
		defaultTimeout:   defaultTimeout,
		defaultMaxOutput: defaultMaxOutput,
		defaultSensitive: append([]string(nil), sensitiveEnvKeys...),
	}
}

// Run 以 argv 方式执行 spec.Argv，不经过 shell。返回的错误一定带有稳定错误码；
// 即使命令执行失败也会填充 Result，便于调用方持久化退出码和捕获的输出。
func (e *Executor) Run(ctx context.Context, spec domain.CommandSpec) (domain.Result, error) {
	if len(spec.Argv) == 0 || strings.TrimSpace(spec.Argv[0]) == "" {
		return domain.Result{}, domain.NewError(v1.CodeInvalidRequest, "argv must contain at least the executable path")
	}
	if e.allow != nil && !e.allow(spec.Argv[0]) {
		return domain.Result{}, domain.NewError(v1.CodePermissionDenied, "executable %q is not permitted by execution.allowedPaths", spec.Argv[0])
	}

	if spec.DryRun {
		return domain.Result{Executed: false, ExitCode: 0}, nil
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = e.defaultTimeout
	}
	maxOutput := spec.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = e.defaultMaxOutput
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, spec.Argv[0], spec.Argv[1:]...)
	if spec.WorkingDirectory != "" {
		cmd.Dir = spec.WorkingDirectory
	}
	cmd.Env = mergeEnv(spec.Environment)

	stdout := newLimitedBuffer(maxOutput)
	stderr := newLimitedBuffer(maxOutput)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	started := time.Now()
	runErr := cmd.Run()
	result := domain.Result{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		Truncated:  stdout.truncated || stderr.truncated,
		DurationMS: time.Since(started).Milliseconds(),
		Executed:   true,
	}

	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return result, domain.NewError(v1.CodeExecCancelled, "operation was cancelled")
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return result, domain.NewError(v1.CodeExecTimeout, "command exceeded the %s timeout", timeout)
	case runErr == nil:
		result.ExitCode = 0
		return result, nil
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, domain.NewError(v1.CodeExecExitNonZero, "command exited with code %d", result.ExitCode)
	}
	if errors.Is(runErr, os.ErrPermission) {
		return result, domain.NewError(v1.CodePermissionDenied, "cannot execute %q: %v", spec.Argv[0], runErr)
	}
	return result, domain.NewError(v1.CodeInternal, "cannot run %q: %v", spec.Argv[0], runErr)
}

func (e *Executor) SensitiveEnvKeys(extra []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, key := range append(append([]string(nil), e.defaultSensitive...), extra...) {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

func RedactArgv(argv []string, sensitive []int) []string {
	return domain.RedactArgv(argv, sensitive)
}

func RedactEnv(env map[string]string, sensitiveKeys []string) map[string]string {
	return domain.RedactEnv(env, sensitiveKeys)
}

type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int64
	truncated bool
}

func newLimitedBuffer(limit int64) *limitedBuffer {
	return &limitedBuffer{limit: limit}
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	remaining := w.limit - int64(w.buf.Len())
	if remaining <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		w.buf.Write(p[:remaining])
		w.truncated = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

func (w *limitedBuffer) String() string { return w.buf.String() }

func mergeEnv(overrides map[string]string) []string {
	base := os.Environ()
	if len(overrides) == 0 {
		return base
	}
	merged := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if _, overridden := overrides[key]; overridden {
			continue
		}
		merged = append(merged, kv)
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		merged = append(merged, key+"="+overrides[key])
	}
	return merged
}

package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
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
//
// stdout 被收进一个有上限的内存缓冲（maxOutputBytes），超过上限的部分会被**丢弃**。
// 因此它只适合输出不大的命令；需要把命令输出整份拿走（数据库转储、归档流）必须用
// RunStream——见那里的说明。
func (e *Executor) Run(ctx context.Context, spec domain.CommandSpec) (domain.Result, error) {
	return e.execInternal(ctx, spec, nil, nil)
}

// RunStream 与 Run 的语义**完全一致**（argv-only、allowedPaths 校验、超时、取消、
// 同一套错误码），差别只在 stdout 与 stdin 是调用方给的**流**，而不是内存缓冲。
//
// 为什么必须有它，而不是让数据库适配器去用 Run：Run 会把 stdout 收进一个有上限的缓冲，
// 超限部分被丢掉、命令却照常退出 0。拿它跑 pg_dump，得到的是一份**记录为成功、
// 内容却残缺**的备份——这类失败不会在备份时暴露，只会在真要恢复时暴露。
//
// in / out 为 nil 表示不接（备份只接 out，恢复只接 in）。
func (e *Executor) RunStream(ctx context.Context, spec domain.CommandSpec, in io.Reader, out io.Writer) (domain.Result, error) {
	return e.execInternal(ctx, spec, in, out)
}

// execInternal 是 Run 与 RunStream 的共同路径。out 为 nil 表示「stdout 收进有上限的
// 内存缓冲」，否则直接写进调用方给的流——两条路的校验、超时与错误分类必须一模一样，
// 各写一份必然会漂移。
func (e *Executor) execInternal(ctx context.Context, spec domain.CommandSpec, in io.Reader, out io.Writer) (domain.Result, error) {
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

	stderr := newLimitedBuffer(maxOutput)
	cmd.Stderr = stderr

	var stdout *limitedBuffer
	// 两个错误变量必须分开：stdin 与 stdout 的拷贝协程是并发的，共用一个变量即数据竞争。
	// 它们各自在自己的 Read/Write 里被写、在 cmd.Run() 返回之后被读，
	// 而 Run 会等两个拷贝协程收尾，因此这里的读写有明确的前后关系。
	var stdinErr, stdoutErr error

	if out == nil {
		stdout = newLimitedBuffer(maxOutput)
		cmd.Stdout = stdout
	} else {
		cmd.Stdout = &killOnWriteErrorWriter{dst: out, cancel: cancel, err: &stdoutErr}
	}
	if in != nil {
		cmd.Stdin = &errorRecordingReader{src: in, err: &stdinErr}
	}

	started := time.Now()
	runErr := cmd.Run()
	result := domain.Result{
		Stderr:     stderr.String(),
		Truncated:  stderr.truncated,
		DurationMS: time.Since(started).Milliseconds(),
		Executed:   true,
	}
	if stdout != nil {
		result.Stdout = stdout.String()
		result.Truncated = result.Truncated || stdout.truncated
	}

	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		// 只看**调用方**的上下文：stdout 写入失败时我们也会取消 runCtx 去杀进程，
		// 那不是「操作被取消」，而是「数据流断了」，下面按流错误上报。
		return result, domain.NewError(v1.CodeExecCancelled, "operation was cancelled")
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return result, domain.NewError(v1.CodeExecTimeout, "command exceeded the %s timeout", timeout)
	}

	// 流的读写失败优先于退出码：
	//   - stdin 读失败时管道关闭，子进程看到的是 EOF，psql 这类工具完全可能因为
	//     「SQL 读完了」而退出 0，于是半份数据被当成一次成功的恢复；
	//   - stdout 写失败（存储层报错、管道被关）时，真正的原因在写入方，不在命令。
	// 把原因记下来并按它上报，否则「备份残缺」会以「命令成功」的姿态过关。
	if stdinErr != nil {
		return result, wrapStreamFailure("读取喂给命令的数据失败", stdinErr)
	}
	if stdoutErr != nil {
		return result, wrapStreamFailure("命令输出写入备份流失败", stdoutErr)
	}

	if runErr == nil {
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

// wrapStreamFailure 给流错误加上上下文，但**已经是带码错误时原样返回**：
// 套一层 INTERNAL 会把「存储配额超了」这类可操作的原因改写成「内部错误」，
// 而错误码是调用方唯一能据以决策的东西。
func wrapStreamFailure(message string, err error) error {
	var coded *domain.CodedError
	if errors.As(err, &coded) {
		return err
	}
	return domain.NewError(v1.CodeInternal, "%s: %v", message, err)
}

// killOnWriteErrorWriter 把命令输出转交给调用方的流，并在写入失败时**取消运行上下文**。
//
// 这一步不是优化，是必需的：os/exec 在 Stdout 不是 *os.File 时会起一个拷贝协程，
// 而 Wait 是**先等进程退出、再收拷贝协程的错误**。写入方一旦失败，拷贝协程停下，
// 子进程却仍阻塞在写满的管道上——两边互等，命令永远不会结束。
// 取消上下文会让 exec.CommandContext 的看门狗杀掉子进程，把这个死结解开。
type killOnWriteErrorWriter struct {
	dst    io.Writer
	cancel context.CancelFunc
	err    *error
}

func (w *killOnWriteErrorWriter) Write(p []byte) (int, error) {
	if *w.err != nil {
		// 已经失败过：不再写、不再取消，把同一个错误还给 io.Copy 让它收尾。
		return 0, *w.err
	}
	n, err := w.dst.Write(p)
	if err != nil {
		*w.err = err
		w.cancel()
	}
	return n, err
}

// errorRecordingReader 记下读取失败的原因，供上层优先于退出码上报。
type errorRecordingReader struct {
	src io.Reader
	err *error
}

func (r *errorRecordingReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		*r.err = err
	}
	return n, err
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

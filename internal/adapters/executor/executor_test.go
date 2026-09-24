package executor

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
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

// ==== RunStream：流式执行（迭代 2 规格 D10） ====
//
// 这些用例的存在理由是**正确性**而不是性能：Run 会把超出上限的输出丢掉，
// 拿它跑数据库转储会得到一份「记录为成功、内容却残缺」的备份。

func TestRunStreamDoesNotTruncateLargeOutput(t *testing.T) {
	sh := mustLookPath(t, "sh")
	// 64 KiB 远超 newExecutor 的 1024 字节上限。
	const size = 64 * 1024
	argv := []string{sh, "-c", "head -c 65536 /dev/zero | tr '\\0' 'x'"}

	var streamed bytes.Buffer
	result, err := newExecutor(allowAll).RunStream(context.Background(),
		domain.CommandSpec{Argv: argv, MaxOutputBytes: 1024}, nil, &streamed)
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if streamed.Len() != size {
		t.Fatalf("流式执行必须把输出完整交出去：want %d 字节，got %d", size, streamed.Len())
	}
	if result.Truncated {
		t.Fatal("流式执行没有「截断」这回事，Truncated 必须为假")
	}

	// 同一个命令走 Run：输出被上限截掉。这条断言就是本端口存在的理由，
	// 也钉住了「哪天有人把 RunStream 实现成 Run 的转发」会立刻失败。
	buffered, err := newExecutor(allowAll).Run(context.Background(),
		domain.CommandSpec{Argv: argv, MaxOutputBytes: 1024})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(buffered.Stdout) != 1024 || !buffered.Truncated {
		t.Fatalf("Run 应当截断到 1024 字节并上报：got %d 字节 truncated=%v",
			len(buffered.Stdout), buffered.Truncated)
	}
}

func TestRunStreamPipesStdinToTheCommand(t *testing.T) {
	cat := mustLookPath(t, "cat")
	const payload = "SELECT 1;\nSELECT 2;\n"

	var out bytes.Buffer
	// cat 把 stdin 原样写回 stdout：这条用例同时证明了两侧都接上了。
	if _, err := newExecutor(allowAll).RunStream(context.Background(),
		domain.CommandSpec{Argv: []string{cat}}, strings.NewReader(payload), &out); err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if out.String() != payload {
		t.Fatalf("stdin → stdout 往返不一致：want %q, got %q", payload, out.String())
	}
}

// failingReader 模拟「读备份流读到一半就坏了」。
type failingReader struct{ readsLeft int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.readsLeft <= 0 {
		return 0, errors.New("模拟读取备份流失败")
	}
	r.readsLeft--
	return copy(p, "SELECT 1;\n"), nil
}

func TestRunStreamReportsStdinFailureOverExitCode(t *testing.T) {
	cat := mustLookPath(t, "cat")

	// cat 在 stdin 关闭时会正常退出 0——若只看退出码，半份 SQL 会被当成一次成功的恢复。
	// 因此读取失败的原因必须优先上报。
	var out bytes.Buffer
	_, err := newExecutor(allowAll).RunStream(context.Background(),
		domain.CommandSpec{Argv: []string{cat}}, &failingReader{readsLeft: 1}, &out)
	if err == nil {
		t.Fatal("stdin 读取失败必须让 RunStream 报错，即使子进程退出了 0")
	}
	if !strings.Contains(err.Error(), "模拟读取备份流失败") {
		t.Fatalf("错误里必须带上真正的原因，got %v", err)
	}
}

// failFirstWrite 第一次写入就失败，之后一直返回同一个错误。
type failFirstWrite struct{ err error }

func (w *failFirstWrite) Write([]byte) (int, error) { return 0, w.err }

func TestRunStreamStopsProcessWhenOutputWriterFails(t *testing.T) {
	yes := mustLookPath(t, "yes")

	// yes 会一直写下去。写入方一失败，拷贝协程就停了，子进程却会阻塞在写满的管道上；
	// 实现若没有杀掉它，这里就会**永久挂住**——所以用墙钟兜底，而不是等 go test 超时。
	boom := errors.New("模拟存储写入失败")
	done := make(chan error, 1)
	go func() {
		_, err := newExecutor(allowAll).RunStream(context.Background(),
			domain.CommandSpec{Argv: []string{yes}}, nil, &failFirstWrite{err: boom})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("输出写入失败必须让 RunStream 报错")
		}
		if !strings.Contains(err.Error(), "模拟存储写入失败") {
			t.Fatalf("错误里必须带上真正的原因，got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("输出写入失败后子进程没有被杀掉：RunStream 挂住了")
	}
}

func TestRunStreamKeepsRunValidationSemantics(t *testing.T) {
	// 端口的两条路径必须共用同一套前置校验，各写一份必然漂移。
	deny := func(string) bool { return false }
	if _, err := newExecutor(deny).RunStream(context.Background(),
		domain.CommandSpec{Argv: []string{"/usr/bin/true"}}, nil, &bytes.Buffer{}); domain.CodeOf(err) != v1.CodePermissionDenied {
		t.Fatalf("want PERMISSION_DENIED, got %v", err)
	}
	if _, err := newExecutor(allowAll).RunStream(context.Background(),
		domain.CommandSpec{Argv: nil}, nil, &bytes.Buffer{}); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}

	result, err := newExecutor(allowAll).RunStream(context.Background(),
		domain.CommandSpec{Argv: []string{"/nonexistent/frz-tools-dry-run"}, DryRun: true}, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("dry run 不得因二进制不存在而失败: %v", err)
	}
	if result.Executed {
		t.Fatal("dry run 不得真的执行进程")
	}
}

package mysql

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// ==== 测试替身 ====

// stubTools 造出一组同名可执行文件，并让 PATH 能解析到它们（理由见 postgres 侧的注释）。
func stubTools(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		stub := filepath.Join(dir, name)
		if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("写入桩 %s: %v", name, err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

type fakeSecrets struct {
	values map[string]string
	err    error
}

func (f fakeSecrets) Resolve(_ context.Context, ref domain.SecretRef) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.values[ref.Name], nil
}

type scriptedExec struct {
	mu    sync.Mutex
	calls []domain.CommandSpec

	stdout     map[string]string
	runErr     map[string]error
	streamErr  map[string]error
	streamData []byte
	streamRead []byte
	// failRunAt 让指定的第几次同名 Run 调用失败（下标从 0 起）。
	failRunAt map[string]map[int]bool
	runSeen   map[string]int
}

func newScriptedExec() *scriptedExec {
	return &scriptedExec{
		stdout:    map[string]string{},
		runErr:    map[string]error{},
		streamErr: map[string]error{},
		failRunAt: map[string]map[int]bool{},
		runSeen:   map[string]int{},
	}
}

func toolName(spec domain.CommandSpec) string {
	if len(spec.Argv) == 0 {
		return ""
	}
	return filepath.Base(spec.Argv[0])
}

func (e *scriptedExec) Run(_ context.Context, spec domain.CommandSpec) (domain.Result, error) {
	name := toolName(spec)
	e.mu.Lock()
	e.calls = append(e.calls, spec)
	index := e.runSeen[name]
	e.runSeen[name]++
	stdout := e.stdout[name]
	runErr := e.runErr[name]
	fail := e.failRunAt[name][index]
	e.mu.Unlock()

	if runErr != nil || fail {
		if runErr == nil {
			runErr = domain.NewError(v1.CodeExecExitNonZero, "%s 第 %d 次调用失败（桩）", name, index+1)
		}
		return domain.Result{Executed: true, ExitCode: 1}, runErr
	}
	return domain.Result{Executed: true, Stdout: stdout}, nil
}

func (e *scriptedExec) RunStream(_ context.Context, spec domain.CommandSpec, in io.Reader, out io.Writer) (domain.Result, error) {
	e.mu.Lock()
	e.calls = append(e.calls, spec)
	name := toolName(spec)
	streamErr := e.streamErr[name]
	data := e.streamData
	e.mu.Unlock()

	if in != nil {
		read, err := io.ReadAll(in)
		if err != nil {
			return domain.Result{Executed: true}, err
		}
		e.mu.Lock()
		e.streamRead = read
		e.mu.Unlock()
	}
	if out != nil && len(data) > 0 {
		if _, err := out.Write(data); err != nil {
			return domain.Result{Executed: true}, err
		}
	}
	if streamErr != nil {
		return domain.Result{Executed: true, ExitCode: 1, Stderr: "stub: 模拟工具失败"}, streamErr
	}
	return domain.Result{Executed: true}, nil
}

func (e *scriptedExec) specForArg(name, marker string) (domain.CommandSpec, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, spec := range e.calls {
		if toolName(spec) != name {
			continue
		}
		if strings.Contains(strings.Join(spec.Argv, " "), marker) {
			return spec, true
		}
	}
	return domain.CommandSpec{}, false
}

func (e *scriptedExec) callsNamed(name string) []domain.CommandSpec {
	e.mu.Lock()
	defer e.mu.Unlock()
	var matched []domain.CommandSpec
	for _, spec := range e.calls {
		if toolName(spec) == name {
			matched = append(matched, spec)
		}
	}
	return matched
}

func (e *scriptedExec) runCount(name string) int { return len(e.callsNamed(name)) }

// ==== 夹具 ====

const testDSN = "mysql://orders-user:s3cr3t-pw@db.internal:3307/orders"

func testPolicy(t *testing.T, name string) *domain.BackupPolicy {
	t.Helper()
	policy := &domain.BackupPolicy{
		APIVersion: domain.BackupPolicyAPIVersion,
		Kind:       domain.BackupPolicyKind,
		Name:       name,
		Resource: domain.BackupResource{
			Kind:      domain.BackupResourceMySQL,
			DSNSecret: domain.SecretRef{Kind: domain.SecretKindFile, Name: "orders-dsn"},
			Database:  "orders",
		},
		Encoding: domain.BackupEncoding{
			// mysqldump 输出纯 SQL 文本，默认的 gzip 是对的（与 postgres 相反）。
			Compression: domain.CompressionGzip,
			Encryption:  domain.BackupEncryption{Enabled: false},
		},
		Retention: domain.BackupRetention{KeepLast: 3},
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("夹具策略本身不合法: %v", err)
	}
	return policy
}

func newAdapter() (*Adapter, *scriptedExec) {
	exec := newScriptedExec()
	exec.stdout["mysqldump"] = "mysqldump  Ver 8.0.46 for Linux on aarch64 (MySQL Community Server - GPL)"
	exec.stdout["mysql"] = "8.0.46"
	return New(exec, fakeSecrets{values: map[string]string{"orders-dsn": testDSN}}), exec
}

// dumpStream 是一份最小的「mysqldump 输出」，头尾两行都是它自己会写的。
const dumpStream = "-- MySQL dump 10.13  Distrib 8.0.46, for Linux (aarch64)\n" +
	"--\n-- Table structure for table `t`\n--\n\nCREATE TABLE `t` (id int);\n\n" +
	"-- Dump completed on 2026-09-24  5:29:00\n"

// ==== 备份 ====

func TestBackupArgvIsSafeAndComplete(t *testing.T) {
	stubTools(t, "mysqldump", "mysql")
	adapter, exec := newAdapter()
	exec.streamData = []byte(dumpStream)

	var out bytes.Buffer
	metadata, err := adapter.Backup(context.Background(), testPolicy(t, "orders-nightly"), &out)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	spec, ok := exec.specForArg("mysqldump", "--single-transaction")
	if !ok {
		t.Fatal("必须调用 mysqldump 做转储")
	}
	joined := strings.Join(spec.Argv, " ")

	// 密码只能出现在环境变量里：argv 会出现在 ps 的输出里。
	if strings.Contains(joined, "s3cr3t-pw") {
		t.Fatalf("密码不得出现在 argv 里：%s", joined)
	}
	if got := spec.Environment["MYSQL_PWD"]; got != "s3cr3t-pw" {
		t.Fatalf("密码必须经 MYSQL_PWD 传递，got %q", got)
	}
	if len(spec.SensitiveEnvKeys) == 0 || spec.SensitiveEnvKeys[0] != "MYSQL_PWD" {
		t.Fatalf("MYSQL_PWD 必须在脱敏名单里，got %v", spec.SensitiveEnvKeys)
	}
	for _, want := range []string{
		"--single-transaction", "--routines", "--events", "--triggers",
		"--no-tablespaces", "--host=db.internal", "--port=3307", "--user=orders-user",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv 缺少 %s：%s", want, joined)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(joined), " orders") {
		t.Fatalf("库名必须是最后一个位置参数，argv=%s", joined)
	}
	if strings.Contains(joined, "--databases") {
		// 这条是本适配器最要命的一条约束：--databases 会在流里写入 CREATE DATABASE
		// 与 USE <原库>，隔离恢复把它喂给临时库时，里面的 USE 会切回**真实库**，
		// 于是「隔离」恢复直接写到生产上。见 Restore 的注释。
		t.Fatalf("绝不能用 --databases（会让隔离恢复写到生产库）：%s", joined)
	}
	if strings.Contains(joined, "--skip-comments") {
		// 加了它，结束行就没了，Verify 的判据会静默失效（规格 D15 的实测）。
		t.Fatalf("绝不能用 --skip-comments（会让校验判据失效）：%s", joined)
	}
	if metadata.ResourceKind != domain.BackupResourceMySQL || metadata.Tool != "mysqldump" {
		t.Fatalf("元数据不对: %+v", metadata)
	}
	if metadata.ClientVersion == "" || metadata.ServerVersion != "8.0.46" {
		t.Fatalf("版本元数据不对: client=%q server=%q", metadata.ClientVersion, metadata.ServerVersion)
	}
	if out.String() != dumpStream {
		t.Fatalf("适配器必须把工具输出原样交给应用层")
	}
}

func TestBackupRefusesMismatchedDatabase(t *testing.T) {
	stubTools(t, "mysqldump", "mysql")
	adapter := New(newScriptedExec(), fakeSecrets{
		values: map[string]string{"orders-dsn": "mysql://u:p@h:3306/production"},
	})

	_, err := adapter.Backup(context.Background(), testPolicy(t, "orders-nightly"), &bytes.Buffer{})
	if domain.CodeOf(err) != v1.CodeBackupPreflightFailed {
		t.Fatalf("want BACKUP_PREFLIGHT_FAILED, got %v", err)
	}
	if !strings.Contains(err.Error(), "production") || !strings.Contains(err.Error(), "orders") {
		t.Fatalf("错误里必须同时出现两个库名，got %v", err)
	}
}

func TestBackupKeepsExecutorErrorCode(t *testing.T) {
	stubTools(t, "mysqldump", "mysql")
	adapter, exec := newAdapter()
	exec.streamErr["mysqldump"] = domain.NewError(v1.CodeExecExitNonZero, "command exited with code 1")

	_, err := adapter.Backup(context.Background(), testPolicy(t, "orders-nightly"), &bytes.Buffer{})
	if domain.CodeOf(err) != v1.CodeExecExitNonZero {
		t.Fatalf("备份失败必须保留执行器的错误码（重试白名单认的是它），got %v", err)
	}
}

// ==== 校验 ====

func TestVerifyAcceptsCompleteDump(t *testing.T) {
	stubTools(t, "mysqldump", "mysql")
	adapter, _ := newAdapter()

	if err := adapter.Verify(context.Background(), testPolicy(t, "orders-nightly"),
		strings.NewReader(dumpStream)); err != nil {
		t.Fatalf("完整输出必须通过: %v", err)
	}
}

func TestVerifyRejectsTruncatedStream(t *testing.T) {
	stubTools(t, "mysqldump", "mysql")
	adapter, _ := newAdapter()

	// 截断在任何位置都会丢掉结束行——这正是选它做判据的理由。
	truncated := dumpStream[:len(dumpStream)/2]
	err := adapter.Verify(context.Background(), testPolicy(t, "orders-nightly"), strings.NewReader(truncated))
	if domain.CodeOf(err) != v1.CodeBackupVerifyFailed {
		t.Fatalf("截断的流必须被拒，got %v", err)
	}
	if !strings.Contains(err.Error(), completionMarker) {
		t.Fatalf("错误信息要指明缺的是结束行，got %v", err)
	}
}

func TestVerifyRejectsForeignStream(t *testing.T) {
	stubTools(t, "mysqldump", "mysql")
	adapter, _ := newAdapter()

	// 头尾都是别的东西：既不是 mysqldump 的输出，也不该被当成「只是缺个尾巴」。
	err := adapter.Verify(context.Background(), testPolicy(t, "orders-nightly"),
		strings.NewReader("PGDMP1234\n-- Dump completed on x\n"))
	if domain.CodeOf(err) != v1.CodeBackupVerifyFailed {
		t.Fatalf("want BACKUP_VERIFY_FAILED, got %v", err)
	}
	if !strings.Contains(err.Error(), headerMarker) {
		t.Fatalf("错误信息要指明开头不对，got %v", err)
	}
}

func TestVerifyRejectsEmptyStream(t *testing.T) {
	stubTools(t, "mysqldump", "mysql")
	adapter, exec := newAdapter()

	err := adapter.Verify(context.Background(), testPolicy(t, "orders-nightly"), strings.NewReader(""))
	if domain.CodeOf(err) != v1.CodeBackupVerifyFailed {
		t.Fatalf("空流必须被拒（空 = 静默的不可恢复），got %v", err)
	}
	// 校验不需要任何外部工具：没有客户端也能校验一份历史备份。
	if exec.runCount("mysqldump") != 0 || exec.runCount("mysql") != 0 {
		t.Fatal("校验不该调用任何 MySQL 工具")
	}
}

func TestVerifyRespectsCancellation(t *testing.T) {
	stubTools(t, "mysqldump", "mysql")
	adapter, _ := newAdapter()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := adapter.Verify(ctx, testPolicy(t, "orders-nightly"), strings.NewReader(dumpStream))
	if domain.CodeOf(err) != v1.CodeExecCancelled {
		t.Fatalf("want EXEC_CANCELLED, got %v", err)
	}
}

// ==== 恢复 ====

func TestRestoreIsolatedCreatesAndDropsTempDatabase(t *testing.T) {
	stubTools(t, "mysqldump", "mysql")
	adapter, exec := newAdapter()
	policy := testPolicy(t, "orders-nightly")

	if err := adapter.Restore(context.Background(), policy, strings.NewReader(dumpStream), domain.RestoreIsolated); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	create, ok := exec.specForArg("mysql", "CREATE DATABASE")
	if !ok {
		t.Fatal("隔离恢复必须建临时库")
	}
	temp := tempNameFromCreate(t, strings.Join(create.Argv, " "))
	if !strings.HasPrefix(temp, "frz_restore_orders_nightly_") {
		t.Fatalf("临时库名要能被认出来，got %s", temp)
	}

	// 恢复必须落到临时库上，绝不能落到声明的真实库上。
	restore, ok := exec.specForArg("mysql", temp)
	if !ok {
		t.Fatalf("必须把流恢复到临时库 %s", temp)
	}
	if strings.HasSuffix(strings.Join(restore.Argv, " "), " orders") {
		t.Fatal("隔离恢复绝不能打到真实库")
	}
	if string(exec.streamRead) != dumpStream {
		t.Fatal("备份流必须完整喂给 mysql 客户端")
	}

	drops := exec.callsNamed("mysql")
	if len(drops) != 3 {
		t.Fatalf("隔离恢复应当有「建库 + 恢复 + 删库」三次 mysql 调用，got %d", len(drops))
	}
	dropJoined := strings.Join(drops[2].Argv, " ")
	if !strings.Contains(dropJoined, "DROP DATABASE IF EXISTS") || !strings.Contains(dropJoined, temp) {
		t.Fatalf("临时库 %s 必须被删掉，argv=%s", temp, dropJoined)
	}
}

func TestRestoreIsolatedDropsTempDatabaseEvenWhenRestoreFails(t *testing.T) {
	stubTools(t, "mysqldump", "mysql")
	adapter, exec := newAdapter()
	exec.streamErr["mysql"] = domain.NewError(v1.CodeExecExitNonZero, "command exited with code 1")

	err := adapter.Restore(context.Background(), testPolicy(t, "orders-nightly"),
		strings.NewReader(dumpStream), domain.RestoreIsolated)
	if domain.CodeOf(err) != v1.CodeBackupRestoreFailed {
		t.Fatalf("want BACKUP_RESTORE_FAILED, got %v", err)
	}
	drops := exec.callsNamed("mysql")
	if len(drops) != 3 || !strings.Contains(strings.Join(drops[2].Argv, " "), "DROP DATABASE") {
		t.Fatal("恢复失败也必须删掉临时库")
	}
}

func TestRestoreInPlaceGoesToDeclaredDatabase(t *testing.T) {
	stubTools(t, "mysqldump", "mysql")
	adapter, exec := newAdapter()

	if err := adapter.Restore(context.Background(), testPolicy(t, "orders-nightly"),
		strings.NewReader(dumpStream), domain.RestoreInPlace); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	spec, ok := exec.specForArg("mysql", " orders")
	if !ok {
		t.Fatal("原地恢复必须打到策略声明的库")
	}
	joined := strings.Join(spec.Argv, " ")
	if strings.Contains(joined, "s3cr3t-pw") {
		t.Fatalf("密码不得出现在 argv 里：%s", joined)
	}
	if exec.runCount("mysql") != 1 {
		t.Fatal("原地恢复不需要建临时库")
	}
}

func TestRestoreRejectsUnknownMode(t *testing.T) {
	stubTools(t, "mysqldump", "mysql")
	adapter, _ := newAdapter()

	err := adapter.Restore(context.Background(), testPolicy(t, "orders-nightly"),
		strings.NewReader(dumpStream), domain.RestoreMode("whatever"))
	if domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
}

// ==== 清理 ====

func TestCleanupIsIdempotent(t *testing.T) {
	stubTools(t, "mysqldump", "mysql")
	adapter, _ := newAdapter()
	policy := testPolicy(t, "orders-nightly")

	for i := 0; i < 2; i++ {
		if err := adapter.Cleanup(context.Background(), policy, "op_1"); err != nil {
			t.Fatalf("Cleanup 必须幂等: %v", err)
		}
	}
	if err := adapter.Cleanup(context.Background(), nil, "op_1"); err != nil {
		t.Fatalf("nil 策略必须安全返回: %v", err)
	}
}

func TestCleanupUnresolvableSecretReportsLeftovers(t *testing.T) {
	stubTools(t, "mysqldump", "mysql")
	adapter, exec := newAdapter()
	policy := testPolicy(t, "orders-nightly")
	// 造一个中断留下的临时库：恢复本身失败、连删库也没成，名字就会留在适配器里。
	exec.streamErr["mysql"] = domain.NewError(v1.CodeExecExitNonZero, "boom")
	exec.failRunAt["mysql"] = map[int]bool{1: true}
	if err := adapter.Restore(context.Background(), policy, strings.NewReader(dumpStream), domain.RestoreIsolated); err == nil {
		t.Fatal("恢复应当失败")
	}
	// 让 Cleanup 也拿不到凭据：它必须把「去删哪个库」讲清楚，而不是默默什么都不做。
	adapter.secrets = fakeSecrets{err: errors.New("凭据文件被删了")}

	err := adapter.Cleanup(context.Background(), policy, "op_1")
	if domain.CodeOf(err) != v1.CodeBackupInUse {
		t.Fatalf("want BACKUP_IN_USE, got %v", err)
	}
	if !strings.Contains(err.Error(), "手工删除") || !strings.Contains(err.Error(), "frz_restore_") {
		t.Fatalf("必须告诉运维去删哪个库，got %v", err)
	}
}

// ==== 连接串解析 ====

func TestParseDSN(t *testing.T) {
	cases := []struct {
		name     string
		dsn      string
		wantErr  bool
		database string
		host     string
		port     string
		password string
	}{
		{name: "完整 URI", dsn: "mysql://u:p%40ss@h:3306/orders", database: "orders", host: "h", port: "3306", password: "p@ss"},
		{name: "没有端口", dsn: "mysql://u:p@h/orders", database: "orders", host: "h", password: "p"},
		{name: "没有密码", dsn: "mysql://u@h/orders", database: "orders", host: "h"},
		{name: "查询参数被拒", dsn: "mysql://u@h/orders?tls=true", wantErr: true},
		{name: "没有库名", dsn: "mysql://u@h", wantErr: true},
		{name: "没有主机名", dsn: "mysql:///orders", wantErr: true},
		{name: "库名里带斜杠", dsn: "mysql://u@h/orders/extra", wantErr: true},
		{name: "空串", dsn: "", wantErr: true},
		{name: "别的协议", dsn: "postgres://u@h/orders", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := parseDSN(tc.dsn)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应当被拒: %s", tc.dsn)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDSN(%q): %v", tc.dsn, err)
			}
			if info.database != tc.database || info.host != tc.host || info.port != tc.port || info.password != tc.password {
				t.Fatalf("解析结果不对: %+v", info)
			}
			for _, arg := range info.connectionArgs() {
				if strings.Contains(arg, tc.password) && tc.password != "" {
					t.Fatalf("连接参数里不得残留密码: %v", arg)
				}
			}
		})
	}
}

func TestQueryParamsAreRejectedRatherThanIgnored(t *testing.T) {
	// 静默丢掉一个连接选项，比直接报错危险得多——那会让人以为它生效了。
	_, err := parseDSN("mysql://u:p@h:3306/orders?ssl-mode=REQUIRED")
	if domain.CodeOf(err) != v1.CodeBackupPreflightFailed {
		t.Fatalf("want BACKUP_PREFLIGHT_FAILED, got %v", err)
	}
	if !strings.Contains(err.Error(), "ssl-mode") {
		t.Fatalf("报错要指出被丢掉的是哪个参数，got %v", err)
	}
}

func TestMajorVersionAndFlavor(t *testing.T) {
	if got := majorVersion("mysqldump  Ver 8.0.46 for Linux on aarch64"); got != 8 {
		t.Fatalf("want 8, got %d", got)
	}
	if got := majorVersion("mysqldump  Ver 10.19 Distrib 10.11.6-MariaDB"); got != 10 {
		t.Fatalf("want 10, got %d", got)
	}
	if got := majorVersion("没有版本号"); got != 0 {
		t.Fatalf("want 0, got %d", got)
	}
	if flavor("mysqldump  Ver 10.19 Distrib 10.11.6-MariaDB") != "MariaDB" {
		t.Fatal("MariaDB 必须被认出来：两者的参数集不完全相同")
	}
	if flavor("mysqldump  Ver 8.0.46 for Linux") != "MySQL" {
		t.Fatal("MySQL 必须被认出来")
	}
}

func TestStreamWindowKeepsHeadAndTail(t *testing.T) {
	window := newStreamWindow(16)
	if _, err := window.Write(bytes.Repeat([]byte("a"), 40)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := window.Write([]byte("TAILMARKER")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if window.total != 50 {
		t.Fatalf("total want 50, got %d", window.total)
	}
	if string(window.head) != strings.Repeat("a", 16) {
		t.Fatalf("head 必须保留最前面的 16 字节，got %q", window.head)
	}
	if !strings.HasSuffix(string(window.tail), "TAILMARKER") {
		t.Fatalf("tail 必须保留最后写入的内容，got %q", window.tail)
	}
	if len(window.tail) > 16 {
		t.Fatalf("tail 不得无限增长，got %d 字节", len(window.tail))
	}
}

// ==== 小工具 ====

func tempNameFromCreate(t *testing.T, argv string) string {
	t.Helper()
	start := strings.Index(argv, "`frz_restore_")
	if start < 0 {
		t.Fatalf("argv 里找不到临时库名：%s", argv)
	}
	rest := argv[start+1:]
	end := strings.Index(rest, "`")
	if end < 0 {
		t.Fatalf("临时库名没有闭合反引号：%s", argv)
	}
	return rest[:end]
}

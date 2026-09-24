package postgres

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
	"github.com/freezeChen/frz-tools/internal/adapters/backup/dbtools"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// ==== 测试替身 ====

// stubTools 造出一组同名可执行文件，并让 PATH 能解析到它们。
//
// 适配器用 exec.LookPath 把工具名解析成**绝对路径**（执行器的 allowedPaths 按绝对路径
// 比较），所以单测必须让这些名字真的能被解析到。用「本机没装 pg_dump 就跳过」的办法
// 会让最该跑的断言常年不跑，而它们恰恰是唯一能钉住 argv 形状的东西。
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

// scriptedExec 记录每一次调用的 argv 与环境，并按命令名给出预设结果。
type scriptedExec struct {
	mu    sync.Mutex
	calls []domain.CommandSpec

	// stdout 是 Run（只读查询）按命令名给出的输出。
	stdout map[string]string
	// runErr 是 Run 按命令名给出的错误。
	runErr map[string]error
	// streamErr 是 RunStream 按命令名给出的错误（nil 表示成功）。
	streamErr map[string]error
	// streamData 是 RunStream 写进 out 的内容（模拟工具的输出）。
	streamData []byte
	// streamRead 记录 RunStream 从 in 读到的全部内容（模拟恢复吃进的流）。
	streamRead []byte
	// failRunAt 让指定的**第几次**同名 Run 调用失败（下标从 0 起），
	// 用来构造「建临时库成功、删临时库失败」这类中途失败。
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

// argvFor 返回某次调用的 argv 拼接。
func (e *scriptedExec) argvFor(name string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, spec := range e.calls {
		if toolName(spec) == name {
			return strings.Join(spec.Argv, " ")
		}
	}
	return ""
}

func (e *scriptedExec) specFor(name string) (domain.CommandSpec, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, spec := range e.calls {
		if toolName(spec) == name {
			return spec, true
		}
	}
	return domain.CommandSpec{}, false
}

// specForArg 找出 argv 里含 marker 的那次调用。
//
// 按工具名取第一次是不够的：同一个工具在一次流程里会被调用多次（`pg_dump --version`
// 与真正的转储都是 pg_dump），断言必须指名道姓。
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

func (e *scriptedExec) runCount(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	count := 0
	for _, spec := range e.calls {
		if toolName(spec) == name {
			count++
		}
	}
	return count
}

// ==== 夹具 ====

const testDSN = "postgres://orders-user:s3cr3t-pw@db.internal:5433/orders?sslmode=require"

func testPolicy(t *testing.T, name string) *domain.BackupPolicy {
	t.Helper()
	policy := &domain.BackupPolicy{
		APIVersion: domain.BackupPolicyAPIVersion,
		Kind:       domain.BackupPolicyKind,
		Name:       name,
		Resource: domain.BackupResource{
			Kind:      domain.BackupResourcePostgres,
			DSNSecret: domain.SecretRef{Kind: domain.SecretKindFile, Name: "orders-dsn"},
			Database:  "orders",
		},
		Encoding: domain.BackupEncoding{
			// postgres 归档自带压缩，策略必须声明 none（规格 D11）。
			Compression: domain.CompressionNone,
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
	exec.stdout["pg_dump"] = "pg_dump (PostgreSQL) 16.4"
	exec.stdout["psql"] = "16.4 (Debian 16.4-1.pgdg120+1)"
	return New(exec, fakeSecrets{values: map[string]string{"orders-dsn": testDSN}}), exec
}

// ==== 校验 ====

func TestValidateRejectsCompressionOtherThanNone(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter := New(newScriptedExec(), fakeSecrets{})

	policy := testPolicy(t, "orders-nightly")
	policy.Encoding.Compression = domain.CompressionGzip

	err := adapter.Validate(context.Background(), policy)
	if domain.CodeOf(err) != v1.CodeManifestInvalid {
		t.Fatalf("want MANIFEST_INVALID, got %v", err)
	}
	if !strings.Contains(err.Error(), "encoding.compression") {
		t.Fatalf("错误信息必须说清该改哪个字段，got %v", err)
	}
}

func TestValidateRejectsNilAndWrongKind(t *testing.T) {
	adapter := New(newScriptedExec(), fakeSecrets{})
	if err := adapter.Validate(context.Background(), nil); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("nil 策略必须被拒，got %v", err)
	}
	policy := testPolicy(t, "orders-nightly")
	policy.Resource.Kind = domain.BackupResourceFiles
	if err := adapter.Validate(context.Background(), policy); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("kind 不匹配必须被拒，got %v", err)
	}
}

// ==== 备份 ====

func TestBackupKeepsPasswordOutOfArgv(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter, exec := newAdapter()
	exec.streamData = []byte("PGDMP-stub")

	var out bytes.Buffer
	metadata, err := adapter.Backup(context.Background(), testPolicy(t, "orders-nightly"), &out)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// 按 marker 取真正的那次转储：pg_dump 至少被调用两次（--version 与转储本体）。
	spec, ok := exec.specForArg("pg_dump", "--format=custom")
	if !ok {
		t.Fatal("必须调用 pg_dump 做转储")
	}
	joined := strings.Join(spec.Argv, " ")
	// 密码只能出现在环境变量里：argv 会出现在 ps 的输出里，任何本机用户都看得到。
	if strings.Contains(joined, "s3cr3t-pw") {
		t.Fatalf("密码不得出现在 argv 里：%s", joined)
	}
	if got := spec.Environment["PGPASSWORD"]; got != "s3cr3t-pw" {
		t.Fatalf("密码必须经 PGPASSWORD 传递，got %q", got)
	}
	if len(spec.SensitiveEnvKeys) == 0 || spec.SensitiveEnvKeys[0] != "PGPASSWORD" {
		t.Fatalf("PGPASSWORD 必须在脱敏名单里，got %v", spec.SensitiveEnvKeys)
	}
	// 连接参数要原样保留（sslmode 之类丢了就是静默换了连接语义），但数据库要剥掉。
	if !strings.Contains(joined, "--dbname=postgres://orders-user@db.internal:5433/orders?sslmode=require") {
		t.Fatalf("--dbname 必须是不含密码、其余参数原样的 URI，got %s", joined)
	}
	for _, want := range []string{"--format=custom", "--no-owner", "--no-acl"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv 缺少 %s：%s", want, joined)
		}
	}
	if metadata.ResourceKind != domain.BackupResourcePostgres || metadata.Tool != "pg_dump" {
		t.Fatalf("元数据不对: %+v", metadata)
	}
	// 版本进备份记录：恢复时的兼容性判断只能靠它。
	if metadata.ClientVersion != "pg_dump (PostgreSQL) 16.4" || metadata.ServerVersion != "16.4 (Debian 16.4-1.pgdg120+1)" {
		t.Fatalf("版本元数据不对: client=%q server=%q", metadata.ClientVersion, metadata.ServerVersion)
	}
	if out.String() != "PGDMP-stub" {
		t.Fatalf("适配器必须把工具输出原样交给应用层，got %q", out.String())
	}
}

func TestBackupRefusesMismatchedDatabase(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	// DSN 指着生产库，策略里却写着 orders——这正是「备错对象」的入口。
	adapter := New(newScriptedExec(), fakeSecrets{
		values: map[string]string{"orders-dsn": "postgres://u:p@h:5432/production"},
	})

	_, err := adapter.Backup(context.Background(), testPolicy(t, "orders-nightly"), &bytes.Buffer{})
	if domain.CodeOf(err) != v1.CodeBackupPreflightFailed {
		t.Fatalf("want BACKUP_PREFLIGHT_FAILED, got %v", err)
	}
	if !strings.Contains(err.Error(), "production") {
		t.Fatalf("错误里必须同时出现两个库名，got %v", err)
	}
}

func TestBackupUnresolvableSecretFails(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter := New(newScriptedExec(), fakeSecrets{err: errors.New("凭据文件不存在")})

	_, err := adapter.Backup(context.Background(), testPolicy(t, "orders-nightly"), &bytes.Buffer{})
	if domain.CodeOf(err) != v1.CodeSecretUnresolved {
		t.Fatalf("want SECRET_UNRESOLVED, got %v", err)
	}
}

func TestBackupKeepsExecutorErrorCode(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter, exec := newAdapter()
	// EXEC_EXIT_NONZERO 与 EXEC_TIMEOUT 在 1d 的重试白名单里，翻译掉就等于关掉重试。
	exec.streamErr["pg_dump"] = domain.NewError(v1.CodeExecExitNonZero, "command exited with code 1")

	_, err := adapter.Backup(context.Background(), testPolicy(t, "orders-nightly"), &bytes.Buffer{})
	if domain.CodeOf(err) != v1.CodeExecExitNonZero {
		t.Fatalf("备份失败必须保留执行器的错误码，got %v", err)
	}
	if !strings.Contains(err.Error(), "pg_dump") {
		t.Fatalf("错误信息要指明是哪个工具，got %v", err)
	}
}

// ==== 预检 ====

func TestPreflightChecksToolsDSNVersionAndDatabase(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter, _ := newAdapter()

	report, err := adapter.Preflight(context.Background(), testPolicy(t, "orders-nightly"))
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	names := map[string]string{}
	for _, check := range report.Checks {
		names[check.Name] = check.Detail
		if !check.OK {
			t.Fatalf("预检项 %s 不应失败：%s", check.Name, check.Detail)
		}
	}
	for _, want := range []string{"clientTools", "dsn", "databaseMatches", "serverReachable", "versionCompatibility", "databaseSize"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("预检缺少检查项 %s（现有 %v）", want, names)
		}
	}
	// 预检的详情是给运维看的，不该把密码带进去。
	for name, detail := range names {
		if strings.Contains(detail, "s3cr3t-pw") {
			t.Fatalf("预检项 %s 的详情里出现了密码：%s", name, detail)
		}
	}
}

func TestPreflightRejectsClientOlderThanServer(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter, exec := newAdapter()
	// pg_dump 拒绝备比自己新的服务端，这件事要在预检就说清楚。
	exec.stdout["pg_dump"] = "pg_dump (PostgreSQL) 15.6"
	exec.stdout["psql"] = "16.4"

	report, err := adapter.Preflight(context.Background(), testPolicy(t, "orders-nightly"))
	if domain.CodeOf(err) != v1.CodeBackupPreflightFailed {
		t.Fatalf("want BACKUP_PREFLIGHT_FAILED, got %v", err)
	}
	check, ok := report.Failure()
	if !ok || check.Name != "versionCompatibility" {
		t.Fatalf("失败项应当是 versionCompatibility，got %+v", check)
	}
}

func TestPreflightRejectsUnreachableServer(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter, exec := newAdapter()
	exec.runErr["psql"] = domain.NewError(v1.CodeExecExitNonZero, "command exited with code 2")

	report, err := adapter.Preflight(context.Background(), testPolicy(t, "orders-nightly"))
	if err == nil {
		t.Fatal("连不上服务端时预检必须失败")
	}
	if check, ok := report.Failure(); !ok || check.Name != "serverReachable" {
		t.Fatalf("失败项应当是 serverReachable，got %+v", check)
	}
}

func TestPreflightRejectsKeywordValueDSN(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter := New(newScriptedExec(), fakeSecrets{
		values: map[string]string{"orders-dsn": "host=db.internal user=orders password=x dbname=orders"},
	})

	_, err := adapter.Preflight(context.Background(), testPolicy(t, "orders-nightly"))
	if domain.CodeOf(err) != v1.CodeBackupPreflightFailed {
		t.Fatalf("keyword=value 形式的 DSN 必须被明确拒绝，got %v", err)
	}
	if !strings.Contains(err.Error(), "postgres://") {
		t.Fatalf("报错要给出可执行的改法，got %v", err)
	}
}

// ==== 校验 ====

func TestVerifyRejectsStreamWithoutMagic(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter, exec := newAdapter()

	err := adapter.Verify(context.Background(), testPolicy(t, "orders-nightly"), strings.NewReader("这不是归档"))
	if domain.CodeOf(err) != v1.CodeBackupVerifyFailed {
		t.Fatalf("want BACKUP_VERIFY_FAILED, got %v", err)
	}
	if exec.runCount("pg_restore") != 0 {
		t.Fatal("魔数就不对时不该再去调 pg_restore")
	}
}

func TestVerifyRejectsEmptyStream(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter, _ := newAdapter()

	err := adapter.Verify(context.Background(), testPolicy(t, "orders-nightly"), strings.NewReader(""))
	if domain.CodeOf(err) != v1.CodeBackupVerifyFailed {
		t.Fatalf("空流必须被拒（空 = 静默的不可恢复），got %v", err)
	}
}

func TestVerifyStreamsArchiveIntoPgRestore(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter, exec := newAdapter()
	archive := "PGDMP" + strings.Repeat("x", 100)

	if err := adapter.Verify(context.Background(), testPolicy(t, "orders-nightly"),
		strings.NewReader(archive)); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	spec, ok := exec.specForArg("pg_restore", "--file=/dev/null")
	if !ok {
		t.Fatal("必须调用 pg_restore 做校验")
	}
	joined := strings.Join(spec.Argv, " ")
	if !strings.Contains(joined, "--file=/dev/null") || !strings.Contains(joined, "--exit-on-error") {
		t.Fatalf("校验要把全部数据块解出来丢弃，argv=%s", joined)
	}
	// 魔数被 peek 过，但必须一个字节不少地喂给 pg_restore，否则我们改的就不是原件了。
	if string(exec.streamRead) != archive {
		t.Fatalf("归档必须完整喂给 pg_restore：want %d 字节, got %d", len(archive), len(exec.streamRead))
	}
	// 校验不接触目标资源，因此不该出现任何连接参数。
	if strings.Contains(joined, "--dbname") || len(spec.Environment) != 0 {
		t.Fatalf("校验不得连接数据库：argv=%s env=%v", joined, spec.Environment)
	}
}

func TestVerifyTranslatesPgRestoreFailure(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter, exec := newAdapter()
	exec.streamErr["pg_restore"] = domain.NewError(v1.CodeExecExitNonZero, "command exited with code 1")

	err := adapter.Verify(context.Background(), testPolicy(t, "orders-nightly"), strings.NewReader("PGDMPxxxx"))
	if domain.CodeOf(err) != v1.CodeBackupVerifyFailed {
		t.Fatalf("校验不过要报 BACKUP_VERIFY_FAILED，got %v", err)
	}
}

// ==== 恢复 ====

func TestRestoreIsolatedCreatesAndDropsTempDatabase(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter, exec := newAdapter()
	policy := testPolicy(t, "orders-nightly")

	if err := adapter.Restore(context.Background(), policy, strings.NewReader("PGDMPxxxx"), domain.RestoreIsolated); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	spec, ok := exec.specForArg("psql", "CREATE DATABASE")
	if !ok {
		t.Fatal("隔离恢复必须建临时库")
	}
	joined := strings.Join(spec.Argv, " ")
	// 临时库名要能被认出来（前缀 + 清洗过的策略名 + 随机后缀），
	// 且不能含连字符——未加引号的标识符在 PostgreSQL 里会把连字符当成减号。
	if !strings.Contains(joined, `"frz_restore_orders_nightly_`) {
		t.Fatalf("必须建一个可识别的临时库，argv=%s", joined)
	}
	temp := tempNameFromCreate(t, joined)

	// 恢复要落到临时库上，绝不能落到声明的真实库上。
	restoreJoined := exec.argvFor("pg_restore")
	if !strings.Contains(restoreJoined, "--dbname=postgres://orders-user@db.internal:5433/"+temp+"?sslmode=require") {
		t.Fatalf("pg_restore 必须打到临时库 %s，argv=%s", temp, restoreJoined)
	}
	if strings.Contains(restoreJoined, "--dbname=postgres://orders-user@db.internal:5433/orders?") {
		t.Fatalf("隔离恢复绝不能打到真实库，argv=%s", restoreJoined)
	}

	// 收尾：临时库必须被删掉，且是同一个名字。
	drops := exec.callsNamed("psql")
	if len(drops) != 2 {
		t.Fatalf("隔离恢复应当只有「建临时库 + 删临时库」两次 psql 调用，got %d", len(drops))
	}
	dropJoined := strings.Join(drops[1].Argv, " ")
	if !strings.Contains(dropJoined, "DROP DATABASE IF EXISTS") || !strings.Contains(dropJoined, temp) {
		t.Fatalf("临时库 %s 必须被删掉，argv=%s", temp, dropJoined)
	}
}

func TestRestoreIsolatedDropsTempDatabaseEvenWhenRestoreFails(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter, exec := newAdapter()
	exec.streamErr["pg_restore"] = domain.NewError(v1.CodeExecExitNonZero, "command exited with code 1")

	err := adapter.Restore(context.Background(), testPolicy(t, "orders-nightly"),
		strings.NewReader("PGDMPxxxx"), domain.RestoreIsolated)
	if domain.CodeOf(err) != v1.CodeBackupRestoreFailed {
		t.Fatalf("want BACKUP_RESTORE_FAILED, got %v", err)
	}
	drops := exec.callsNamed("psql")
	if len(drops) != 2 || !strings.Contains(strings.Join(drops[1].Argv, " "), "DROP DATABASE") {
		t.Fatal("恢复失败也必须删掉临时库，否则它会一直占着对端的磁盘")
	}
}

func TestRestoreIsolatedReportsLeftoverTempDatabase(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter, exec := newAdapter()
	// 第二次 psql 调用（删临时库）失败：内容已经恢复成功，但库还在。
	exec.failRunAt["psql"] = map[int]bool{1: true}

	err := adapter.Restore(context.Background(), testPolicy(t, "orders-nightly"),
		strings.NewReader("PGDMPxxxx"), domain.RestoreIsolated)
	if domain.CodeOf(err) != v1.CodeBackupRestoreFailed {
		t.Fatalf("want BACKUP_RESTORE_FAILED, got %v", err)
	}
	if !strings.Contains(err.Error(), "临时库") {
		t.Fatalf("必须说清是临时库没删掉，got %v", err)
	}
	// 没删掉的临时库必须还给 Cleanup，否则它会永远留在那儿。
	if err := adapter.Cleanup(context.Background(), testPolicy(t, "orders-nightly"), "op_1"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if exec.runCount("psql") != 3 {
		t.Fatalf("Cleanup 应当再发一次 DROP，psql 调用次数=%d", exec.runCount("psql"))
	}
}

func TestRestoreInPlaceCleansBeforeRestoring(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter, exec := newAdapter()

	if err := adapter.Restore(context.Background(), testPolicy(t, "orders-nightly"),
		strings.NewReader("PGDMPxxxx"), domain.RestoreInPlace); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	joined := exec.argvFor("pg_restore")
	if !strings.Contains(joined, "--clean") || !strings.Contains(joined, "--if-exists") {
		t.Fatalf("原地恢复要先清掉归档里会重建的对象，argv=%s", joined)
	}
	if !strings.Contains(joined, "--dbname=postgres://orders-user@db.internal:5433/orders?sslmode=require") {
		t.Fatalf("原地恢复必须打到策略声明的库，argv=%s", joined)
	}
	if strings.Contains(joined, "s3cr3t-pw") {
		t.Fatalf("密码不得出现在 argv 里：%s", joined)
	}
	if exec.runCount("psql") != 0 {
		t.Fatal("原地恢复不需要建临时库")
	}
}

func TestRestoreRejectsUnknownMode(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
	adapter, _ := newAdapter()

	err := adapter.Restore(context.Background(), testPolicy(t, "orders-nightly"),
		strings.NewReader("PGDMPxxxx"), domain.RestoreMode("whatever"))
	if domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
}

// ==== 清理 ====

func TestCleanupIsIdempotent(t *testing.T) {
	stubTools(t, "pg_dump", "pg_restore", "psql")
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

// ==== 连接串解析（直接测函数，不走适配器） ====

func TestParseDSN(t *testing.T) {
	cases := []struct {
		name     string
		dsn      string
		wantErr  bool
		database string
		argvURI  string
		password string
	}{
		{
			name:     "完整 URI",
			dsn:      "postgres://u:p%40ss@h:5432/orders?sslmode=require",
			database: "orders",
			argvURI:  "postgres://u@h:5432/orders?sslmode=require",
			password: "p@ss",
		},
		{
			name:     "没有密码",
			dsn:      "postgresql://u@h/orders",
			database: "orders",
			argvURI:  "postgresql://u@h/orders",
		},
		{
			name:     "没有用户名",
			dsn:      "postgres://h:5432/orders",
			database: "orders",
			argvURI:  "postgres://h:5432/orders",
		},
		{name: "keyword=value 被拒", dsn: "host=h dbname=orders", wantErr: true},
		{name: "没有库名", dsn: "postgres://u@h", wantErr: true},
		{name: "库名里带斜杠", dsn: "postgres://u@h/orders/extra", wantErr: true},
		{name: "空串", dsn: "", wantErr: true},
		{name: "别的协议", dsn: "mysql://u@h/orders", wantErr: true},
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
			if info.database != tc.database {
				t.Fatalf("database want %q, got %q", tc.database, info.database)
			}
			if got := info.argvURI(); got != tc.argvURI {
				t.Fatalf("argvURI want %q, got %q", tc.argvURI, got)
			}
			if info.password != tc.password {
				t.Fatalf("password want %q, got %q", tc.password, info.password)
			}
			if strings.Contains(info.argvURI(), tc.password) && tc.password != "" {
				t.Fatal("argv 里不得残留密码")
			}
		})
	}
}

func TestMajorVersion(t *testing.T) {
	cases := map[string]int{
		"pg_dump (PostgreSQL) 16.4":                       16,
		"pg_dump (PostgreSQL) 9.6.24":                     9,
		"16.4 (Debian 16.4-1.pgdg120+1)":                  16,
		"PostgreSQL 15.6 on x86_64":                       15,
		"显然没有版本号":                                         0,
		"psql (PostgreSQL) 12.18 (Ubuntu 12.18-0ubuntu1)": 12,
	}
	for value, want := range cases {
		if got := majorVersion(value); got != want {
			t.Fatalf("majorVersion(%q) want %d, got %d", value, want, got)
		}
	}
}

func TestTempDatabaseNameIsIdentifierSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		name := dbtools.TempDatabaseName("orders.nightly-1")
		if seen[name] {
			t.Fatalf("临时库名必须唯一，%q 重复了", name)
		}
		seen[name] = true
		if len(name) > 63 {
			t.Fatalf("临时库名必须落在标识符上限内，got %d 字节: %s", len(name), name)
		}
		if strings.ToLower(name) != name {
			t.Fatalf("临时库名必须全小写（未加引号的标识符会被折叠），got %s", name)
		}
		for _, r := range name {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
			if !ok {
				t.Fatalf("临时库名里出现了不安全的字符 %q: %s", r, name)
			}
		}
	}
	// 很长的策略名也不能撑破标识符上限，且随机后缀要留住（唯一性在它身上）。
	long := dbtools.TempDatabaseName(strings.Repeat("a", 200))
	if len(long) > 63 {
		t.Fatalf("超长策略名必须被截断，got %d 字节", len(long))
	}
}

// ==== 小工具 ====

// tempNameFromCreate 从 `CREATE DATABASE "x"` 里取出临时库名。
func tempNameFromCreate(t *testing.T, argv string) string {
	t.Helper()
	start := strings.Index(argv, `"frz_restore_`)
	if start < 0 {
		t.Fatalf("argv 里找不到临时库名：%s", argv)
	}
	rest := argv[start+1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("临时库名没有闭合引号：%s", argv)
	}
	return rest[:end]
}

// Package postgres 是 PostgreSQL 备份适配器：用 pg_dump 产出逻辑备份流。
//
// 它只产出**逻辑流**。压缩、加密、摘要与原子提交到 StorageBackend 全部由应用层负责
// （迭代 2 规格 D1）——把加密下放给每个适配器，等于让每种数据库各实现一套，
// 任何一处写错都是静默的数据泄露或静默的不可恢复。
//
// 归档格式固定用 `pg_dump --format=custom`，理由不是偏好：只有这个格式自带目录（TOC），
// 使得「不连数据库就能校验这份归档工具读不读得动」成为可能（`pg_restore --list` /
// `--file=/dev/null`）。相应地，**策略必须声明 `encoding.compression: none`**：
// 自定义归档自带压缩，应用层再压一遍只是白烧 CPU（规格 D11）。
package postgres

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/backup/dbtools"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// Tool 是写进备份元数据的工具名。
const Tool = "pg_dump"

// customFormatMagic 是 pg_dump 自定义格式归档的魔数。先看它一眼，能给出比
// 「pg_restore 说读不动」清楚得多的报错：空流、截断在开头、拿错了文件，
// 这三种情况的原因完全不同。
const customFormatMagic = "PGDMP"

// Adapter 实现 application.BackupAdapter，负责 kind=postgres。
type Adapter struct {
	exec    application.Executor
	secrets application.SecretResolver
	// temps 记录本策略下尚未收尾的临时库：Restore 正常收尾时会自己删掉，
	// 这里记的是**被中断**的那一次留下的东西，Cleanup 是它们唯一的出口。
	temps dbtools.Temps
}

func New(exec application.Executor, secrets application.SecretResolver) *Adapter {
	return &Adapter{exec: exec, secrets: secrets}
}

func (a *Adapter) Kind() domain.BackupResourceKind { return domain.BackupResourcePostgres }

func (a *Adapter) Validate(_ context.Context, policy *domain.BackupPolicy) error {
	if policy == nil {
		return domain.NewError(v1.CodeInvalidRequest, "postgres 适配器需要非空策略")
	}
	if policy.Resource.Kind != domain.BackupResourcePostgres {
		return domain.NewError(v1.CodeInvalidRequest,
			"postgres 适配器只处理 kind=postgres，got %q", policy.Resource.Kind)
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	// D11：归档自带压缩，应用层不再叠加。写 gzip 不是「也行」，是白烧一遍 CPU 与时间，
	// 而且在提交期就该被拦住——让它在备份跑了半小时之后才被人发现没有任何好处。
	if policy.Encoding.Compression != domain.CompressionNone {
		return domain.NewError(v1.CodeManifestInvalid,
			"postgres 用 pg_dump --format=custom，归档已自带压缩；"+
				"请把 encoding.compression 写成 none（当前是 %q，会在已压缩的归档上再压一遍）",
			policy.Encoding.Compression)
	}
	return nil
}

// Preflight 检查凭据、工具、版本与库名。它**不产生任何备份产物**。
func (a *Adapter) Preflight(ctx context.Context, policy *domain.BackupPolicy) (domain.PreflightReport, error) {
	if err := a.Validate(ctx, policy); err != nil {
		return domain.PreflightReport{}, err
	}

	report := domain.PreflightReport{ToolVersion: Tool}
	fail := func(name, detail string) domain.PreflightReport {
		report.Checks = append(report.Checks, domain.PreflightCheck{Name: name, OK: false, Detail: detail})
		return report
	}
	ok := func(name, detail string) {
		report.Checks = append(report.Checks, domain.PreflightCheck{Name: name, OK: true, Detail: detail})
	}

	// 1) 三个工具缺一不可：缺 psql 会在隔离恢复时才炸，缺 pg_restore 会在校验时才炸。
	//    locateTools 返回成功即三者齐备，因此这里只关心「有没有」。
	dump, _, _, toolErr := locateTools()
	if toolErr != nil {
		return fail("clientTools", toolErr.Error()), toolErr
	}
	clientVersion, clientErr := version(ctx, a.exec, dump, "--version")
	if clientErr != nil {
		return fail("clientVersion", "读取客户端版本失败: "+clientErr.Error()), clientErr
	}
	report.ToolVersion = Tool + " " + clientVersion
	ok("clientTools", "pg_dump / pg_restore / psql 均已找到")

	// 2) 凭据与 DSN 形状。
	conn, connErr := a.resolveConnection(ctx, policy)
	if connErr != nil {
		return fail("dsn", connErr.Error()), connErr
	}
	ok("dsn", "凭据可解析，目标 "+conn.endpoint()+"（不含密码，只报告主机与库名）")

	// 3) 库名一致（D13）：策略声明的库必须就是 DSN 指向的库。
	if matchErr := ensureDatabaseMatches(policy, conn); matchErr != nil {
		return fail("databaseMatches", matchErr.Error()), matchErr
	}
	ok("databaseMatches", "策略声明的库与 DSN 一致："+conn.database)

	// 4) 连得上吗、服务端是什么版本。这两件事由同一次查询回答。
	serverVersion, serverErr := a.serverVersion(ctx, conn)
	if serverErr != nil {
		return fail("serverReachable", "连不上服务端或读不到版本: "+serverErr.Error()), serverErr
	}
	ok("serverReachable", "服务端版本 "+serverVersion)

	// 5) 客户端版本必须不低于服务端主版本：pg_dump 拒绝倒着备，早说比中途失败好。
	clientMajor, serverMajor := majorVersion(clientVersion), majorVersion(serverVersion)
	switch {
	case clientMajor == 0 || serverMajor == 0:
		ok("versionCompatibility", "版本号无法判定（client="+clientVersion+" server="+serverVersion+"）")
	case clientMajor < serverMajor:
		detail := "pg_dump " + clientVersion + " 低于服务端 " + serverVersion +
			"：pg_dump 不能备份比自己新的服务端，请安装同版本或更新的客户端工具包"
		return fail("versionCompatibility", detail), domain.NewError(v1.CodeBackupPreflightFailed, "%s", detail)
	default:
		ok("versionCompatibility", "客户端 "+clientVersion+" ≥ 服务端 "+serverVersion)
	}

	// 6) 库大小：只作为信息报告。真正的空间约束是备份存储的 quotaBytes（应用层），
	//    以及对端磁盘——那两处管得住，这里再拦一道只会造出第二份会漂移的真相。
	if size, sizeErr := a.databaseSize(ctx, conn); sizeErr == nil {
		ok("databaseSize", size)
	} else {
		ok("databaseSize", "无法读取库大小（不影响备份）: "+sizeErr.Error())
	}

	ok("compression", "归档自带压缩（--format=custom），应用层不再叠加")

	return report, nil
}

// Backup 用 pg_dump 把逻辑备份流写进 w。
func (a *Adapter) Backup(ctx context.Context, policy *domain.BackupPolicy, w io.Writer) (domain.BackupMetadata, error) {
	if err := a.Validate(ctx, policy); err != nil {
		return domain.BackupMetadata{}, err
	}
	conn, err := a.resolveConnection(ctx, policy)
	if err != nil {
		return domain.BackupMetadata{}, err
	}
	if err := ensureDatabaseMatches(policy, conn); err != nil {
		return domain.BackupMetadata{}, err
	}
	dump, _, _, err := locateTools()
	if err != nil {
		return domain.BackupMetadata{}, err
	}
	clientVersion, err := version(ctx, a.exec, dump, "--version")
	if err != nil {
		return domain.BackupMetadata{}, err
	}
	// 服务端版本先取：让「12 的 pg_dump 备的 16 的库」这类事实进备份记录，
	// 恢复时的兼容性判断只能靠它。失败即失败，不静默留空。
	serverVersion, err := a.serverVersion(ctx, conn)
	if err != nil {
		return domain.BackupMetadata{}, err
	}

	spec := domain.CommandSpec{
		Argv: []string{
			dump,
			"--format=custom",
			// 属主与 ACL 不进归档：恢复到别的环境时它们几乎总是错的，
			// 而「恢复失败在权限上」是最难查的一类失败。
			"--no-owner",
			"--no-acl",
			"--dbname=" + conn.argvURI(),
		},
		Environment:      conn.env(),
		SensitiveEnvKeys: conn.sensitiveEnvKeys(),
		Timeout:          dbtools.TimeoutOrDefault(policy.Timeout.Backup, domain.DefaultBackupTimeout),
	}

	result, runErr := a.exec.RunStream(ctx, spec, nil, w)
	if runErr != nil {
		// 保留执行器的错误码：EXEC_EXIT_NONZERO 与 EXEC_TIMEOUT 都在 1d 的重试
		// 白名单里，翻译成 BACKUP_* 等于把「这次失败可以再试」这个判断抹掉。
		return domain.BackupMetadata{}, dbtools.ToolFailure(Tool, result, runErr, "")
	}

	return domain.BackupMetadata{
		ResourceKind:  domain.BackupResourcePostgres,
		Tool:          Tool,
		ClientVersion: clientVersion,
		ServerVersion: serverVersion,
	}, nil
}

// Verify 校验归档的自洽性，**不接触目标资源、也不需要凭据**。
//
// 两道判据合一：`pg_restore --file=/dev/null` 会把目录与**每一个数据块**都解出来
// （写到 /dev/null，不需要数据库连接），读不动就是损坏。
//
// 它证明的是「这个工具读不读得动这份归档」，**不是**内容完整。实测（PostgreSQL 16.15）：
// 把一份 94 KB 的归档截断到一半，只读目录的 `pg_restore --list` 照样返回 0，
// 而 `--file=/dev/null` 会拒绝；但砍掉末尾 500 字节两者都不报错。
// 整份流的完整性由应用层负责：存储摘要校验与传输编码（gzip 的 CRC、加密流的终结块）。
func (a *Adapter) Verify(ctx context.Context, policy *domain.BackupPolicy, r io.Reader) error {
	if err := a.Validate(ctx, policy); err != nil {
		return err
	}
	_, restore, _, err := locateTools()
	if err != nil {
		return err
	}

	head, err := peekMagic(r)
	if err != nil {
		return err
	}

	spec := domain.CommandSpec{
		Argv: []string{
			restore,
			"--file=/dev/null",
			// 校验的目的是「能不能用」，所以第一个错误就是结论，不必跑到最后。
			"--exit-on-error",
		},
		Timeout: dbtools.TimeoutOrDefault(policy.Timeout.Restore, domain.DefaultRestoreTimeout),
	}
	result, runErr := a.exec.RunStream(ctx, spec, io.MultiReader(bytes.NewReader(head), r), nil)
	if runErr != nil {
		return dbtools.ToolFailure("pg_restore --file=/dev/null", result, runErr, v1.CodeBackupVerifyFailed)
	}
	return nil
}

// Restore 恢复一份备份。
//
//   - isolated：建一个**临时库**（同一实例上），恢复到它，完成后删掉。它证明
//     「这份归档真的能恢复」，且不碰真实数据库。
//   - inPlace：恢复到策略声明的库，调用方必须先做显式确认（确认在应用层，规格 D7）。
//
// 「临时库」不是「临时实例」：起一个完整的 PostgreSQL 进程需要数据目录、端口、
// root 权限，成本与风险都远超收益。代价必须说清楚——它需要 CREATEDB 权限与对端磁盘，
// 且**覆盖不到实例级对象**（角色、表空间）。
func (a *Adapter) Restore(ctx context.Context, policy *domain.BackupPolicy, r io.Reader, mode domain.RestoreMode) error {
	if err := a.Validate(ctx, policy); err != nil {
		return err
	}
	if !mode.Valid() {
		return domain.NewError(v1.CodeInvalidRequest, "恢复模式取值非法: %q", mode)
	}
	conn, err := a.resolveConnection(ctx, policy)
	if err != nil {
		return err
	}
	if err := ensureDatabaseMatches(policy, conn); err != nil {
		return err
	}

	switch mode {
	case domain.RestoreInPlace:
		return a.restoreInto(ctx, policy, conn, conn.argvURI(), r, true)
	default:
		return a.restoreIsolated(ctx, policy, conn, r)
	}
}

// restoreInto 把归档恢复到给定连接串指向的库。
func (a *Adapter) restoreInto(ctx context.Context, policy *domain.BackupPolicy, conn conninfo, target string, r io.Reader, clean bool) error {
	_, restore, _, err := locateTools()
	if err != nil {
		return err
	}

	argv := []string{restore, "--no-owner", "--no-acl", "--exit-on-error"}
	if clean {
		// 原地恢复的语义是「让目标等于归档」，而不是「往现存的库里再灌一遍」：
		// 不先清掉归档里会重建的对象，恢复出来的东西取决于目标此刻的状态。
		argv = append(argv, "--clean", "--if-exists")
	}
	argv = append(argv, "--dbname="+target)

	spec := domain.CommandSpec{
		Argv:             argv,
		Environment:      conn.env(),
		SensitiveEnvKeys: conn.sensitiveEnvKeys(),
		Timeout:          dbtools.TimeoutOrDefault(policy.Timeout.Restore, domain.DefaultRestoreTimeout),
	}
	result, runErr := a.exec.RunStream(ctx, spec, r, nil)
	if runErr != nil {
		// 换成 BACKUP_RESTORE_FAILED：「这次恢复没成功」是运维要的答案，
		// 「一条命令退出了 1」不是。取消与超时仍原样上抛。
		return dbtools.ToolFailure("pg_restore", result, runErr, v1.CodeBackupRestoreFailed)
	}
	return nil
}

// restoreIsolated 在临时库里恢复，成功后立刻删掉临时库。
//
// 无论成败都删：成功时它已经没用了，失败时留下的是一份不一致的半成品。
// 中断路径留下的由 Cleanup 收尾。
func (a *Adapter) restoreIsolated(ctx context.Context, policy *domain.BackupPolicy, conn conninfo, r io.Reader) error {
	temp := dbtools.TempDatabaseName(policy.Name)
	// 先登记再创建：登记在创建之后的话，「建完了、还没登记」这一瞬间崩溃
	// 就会留下一个谁也不知道的孤儿库。
	a.temps.Track(policy.Name, temp)

	if err := a.execSQL(ctx, conn, conn.argvURI(), "CREATE DATABASE \""+temp+"\"",
		dbtools.TimeoutOrDefault(policy.Timeout.Restore, domain.DefaultRestoreTimeout)); err != nil {
		return err
	}

	restoreErr := a.restoreInto(ctx, policy, conn, conn.withDatabase(temp), r, false)

	dropErr := a.execSQL(ctx, conn, conn.argvURI(), "DROP DATABASE IF EXISTS \""+temp+"\"",
		dbtools.TimeoutOrDefault(policy.Timeout.Restore, domain.DefaultRestoreTimeout))
	if dropErr == nil {
		a.temps.Untrack(policy.Name, temp)
	}
	if restoreErr != nil {
		return restoreErr
	}
	if dropErr != nil {
		// 内容已经恢复成功了，只是临时库没删掉。这不该让整次恢复判为失败
		// （它已经证明了「这份归档能恢复」），但必须说出来——它会一直占着对端的磁盘。
		return domain.NewError(v1.CodeBackupRestoreFailed,
			"隔离恢复成功，但临时库 %s 没能删除，请手工清理: %v", temp, dropErr)
	}
	return nil
}

// Cleanup 删掉本策略下**还没收尾**的临时库。幂等：没有记录、或已经删过，都返回成功——
// 它会在中断路径上被反复调用。
func (a *Adapter) Cleanup(ctx context.Context, policy *domain.BackupPolicy, _ string) error {
	if policy == nil {
		return nil
	}
	names := a.temps.Drain(policy.Name)
	if len(names) == 0 {
		return nil
	}

	conn, err := a.resolveConnection(ctx, policy)
	if err != nil {
		// 库还在，但凭据已经解析不了了：把名字还回去并说明白，
		// 运维至少知道去删哪个。
		for _, name := range names {
			a.temps.Track(policy.Name, name)
		}
		return domain.NewError(v1.CodeBackupInUse,
			"有 %d 个临时库需要清理（%s），但凭据无法解析，请手工删除: %v",
			len(names), strings.Join(names, ", "), err)
	}

	var firstErr error
	for _, name := range names {
		dropErr := a.execSQL(ctx, conn, conn.argvURI(), "DROP DATABASE IF EXISTS \""+name+"\"", cleanupTimeout)
		if dropErr != nil {
			if firstErr == nil {
				firstErr = dropErr
			}
			a.temps.Track(policy.Name, name)
			continue
		}
	}
	if firstErr != nil {
		return domain.NewError(v1.CodeBackupInUse,
			"部分临时库未能删除，请手工清理（前缀 %s）: %v", dbtools.TempPrefix(), firstErr)
	}
	return nil
}

// cleanupTimeout 是 Cleanup 使用的固定超时：它不在任何 Operation 的超时预算之内
// （收尾发生在操作结束之后），又不该无限期挂着。
const cleanupTimeout = 60 * time.Second

// queryTimeout 只读查询（版本、库大小）的固定超时：它们都应该是毫秒级的，
// 给它 30 秒只是为了让「网络半死不活」这种情况以一个明确的超时收场。
const queryTimeout = 30 * time.Second

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// resolveConnection 解析凭据并得到连接信息。
func (a *Adapter) resolveConnection(ctx context.Context, policy *domain.BackupPolicy) (conninfo, error) {
	raw, err := dbtools.ResolveSecret(ctx, a.secrets, policy.Resource.DSNSecret)
	if err != nil {
		return conninfo{}, err
	}
	return parseDSN(raw)
}

// locateTools 把三个工具解析成绝对路径。执行器按绝对路径做 allowedPaths 校验，
// 所以这一步是必需的（适配器决定用哪个工具，配置决定它允许待在哪儿）。
func locateTools() (dump, restore, psql string, err error) {
	if dump, err = dbtools.Locate("pg_dump"); err != nil {
		return "", "", "", err
	}
	if restore, err = dbtools.Locate("pg_restore"); err != nil {
		return "", "", "", err
	}
	if psql, err = dbtools.Locate("psql"); err != nil {
		return "", "", "", err
	}
	return dump, restore, psql, nil
}

// version 跑一次 `<tool> --version` 并取出输出。
func version(ctx context.Context, exec application.Executor, tool string, args ...string) (string, error) {
	result, err := exec.Run(ctx, domain.CommandSpec{Argv: append([]string{tool}, args...)})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

// serverVersion 读服务端版本，与可达性检查是同一次往返。
func (a *Adapter) serverVersion(ctx context.Context, conn conninfo) (string, error) {
	out, err := a.query(ctx, conn, conn.argvURI(), "SHOW server_version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// databaseSize 读库大小，只作为预检的信息项。
func (a *Adapter) databaseSize(ctx context.Context, conn conninfo) (string, error) {
	out, err := a.query(ctx, conn, conn.argvURI(), "SELECT pg_size_pretty(pg_database_size(current_database()))")
	if err != nil {
		return "", err
	}
	return "库大小 " + strings.TrimSpace(out), nil
}

// query 跑一条只读 SQL 并返回结果。
func (a *Adapter) query(ctx context.Context, conn conninfo, target, sql string) (string, error) {
	psql, err := dbtools.Locate("psql")
	if err != nil {
		return "", err
	}
	result, runErr := a.exec.Run(ctx, domain.CommandSpec{
		Argv: []string{
			psql,
			"--no-psqlrc",
			"--no-align",
			"--tuples-only",
			// 没有它，SQL 失败时 psql 也退出 0——「查不到版本」会伪装成「版本是空串」。
			"--set=ON_ERROR_STOP=1",
			"--dbname=" + target,
			"--command", sql,
		},
		Environment:      conn.env(),
		SensitiveEnvKeys: conn.sensitiveEnvKeys(),
		Timeout:          queryTimeout,
	})
	if runErr != nil {
		return "", dbtools.ToolFailure("psql", result, runErr, v1.CodeBackupPreflightFailed)
	}
	return result.Stdout, nil
}

// execSQL 跑一条会改状态的 SQL（建/删临时库）。
func (a *Adapter) execSQL(ctx context.Context, conn conninfo, target, sql string, timeout time.Duration) error {
	psql, err := dbtools.Locate("psql")
	if err != nil {
		return err
	}
	result, runErr := a.exec.Run(ctx, domain.CommandSpec{
		Argv: []string{
			psql,
			"--no-psqlrc",
			"--quiet",
			"--set=ON_ERROR_STOP=1",
			"--dbname=" + target,
			"--command", sql,
		},
		Environment:      conn.env(),
		SensitiveEnvKeys: conn.sensitiveEnvKeys(),
		Timeout:          timeout,
	})
	if runErr != nil {
		return dbtools.ToolFailure("psql", result, runErr, v1.CodeBackupRestoreFailed)
	}
	return nil
}

// peekMagic 先看一眼前几个字节。
//
// 空流、截断在开头、拿错了文件——这三种情况的原因完全不同，而 pg_restore 给出的
// 都是同一句「could not read from input file」。这里把它们区分开。
func peekMagic(r io.Reader) ([]byte, error) {
	head := make([]byte, len(customFormatMagic))
	if _, err := io.ReadFull(r, head); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, domain.NewError(v1.CodeBackupVerifyFailed,
				"归档不足 %d 字节，不可能是 pg_dump 的自定义格式（--format=custom）",
				len(customFormatMagic))
		}
		return nil, domain.NewError(v1.CodeBackupVerifyFailed, "读取归档失败: %v", err)
	}
	if string(head) != customFormatMagic {
		return nil, domain.NewError(v1.CodeBackupVerifyFailed,
			"归档的魔数不是 %q，这不是 pg_dump --format=custom 的归档", customFormatMagic)
	}
	return head, nil
}

// majorVersion 从版本串里取出主版本号。
//
// pg_dump --version 是 `pg_dump (PostgreSQL) 16.15`，SHOW server_version 是
// `16.15 (Debian 16.15-1.pgdg120+1)`——两边的第一个数字段都是主版本。
// 取不出来时返回 0，调用方据此跳过判断而不是拿 0 去比较。
func majorVersion(value string) int {
	start := -1
	for i, r := range value {
		if r >= '0' && r <= '9' {
			start = i
			break
		}
	}
	if start < 0 {
		return 0
	}
	major := 0
	for _, r := range value[start:] {
		if r < '0' || r > '9' {
			break
		}
		major = major*10 + int(r-'0')
	}
	return major
}

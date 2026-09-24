// Package mysql 是 MySQL / MariaDB 备份适配器：用 mysqldump 产出逻辑备份流。
//
// 它只产出**逻辑流**。压缩、加密、摘要与原子提交到 StorageBackend 全部由应用层负责
// （迭代 2 规格 D1）。与 postgres 不同的两点，都是被工具的行为逼出来的：
//
//   - mysqldump 的输出是**纯 SQL 文本**，没有自带压缩，所以默认的
//     `encoding.compression: gzip` 是对的——应用层压它是净收益；
//   - 纯 SQL 文本没有可读的目录，因此校验只能靠**工具自己写的结束行**
//     `-- Dump completed on ...`：它只在 mysqldump 真的跑完时才会出现，
//     截断在任何位置都必然丢掉它。这条判据要求**绝不能传 `--skip-comments`**
//     （实测：MySQL 8.0.46 上该选项会把结束行一起去掉，校验判据会静默失效）。
package mysql

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
const Tool = "mysqldump"

const (
	// headerMarker 是 mysqldump 输出的开头标志（MariaDB 的 mysqldump 同样是这一行）。
	headerMarker = "-- MySQL dump"
	// completionMarker 是 mysqldump 跑完时写的最后一行。它**不是**我们编的格式，
	// 是工具自己的语义：没跑完就没有它。
	completionMarker = "-- Dump completed"
	// windowSize 是校验时保留的头部/尾部窗口大小。结束行是最后一行，
	// 尾巴留 8 KiB 足够容纳它以及它前面的一小段。
	windowSize = 8 << 10
)

// Adapter 实现 application.BackupAdapter，负责 kind=mysql。
type Adapter struct {
	exec    application.Executor
	secrets application.SecretResolver
	temps   dbtools.Temps
}

func New(exec application.Executor, secrets application.SecretResolver) *Adapter {
	return &Adapter{exec: exec, secrets: secrets}
}

func (a *Adapter) Kind() domain.BackupResourceKind { return domain.BackupResourceMySQL }

func (a *Adapter) Validate(_ context.Context, policy *domain.BackupPolicy) error {
	if policy == nil {
		return domain.NewError(v1.CodeInvalidRequest, "mysql 适配器需要非空策略")
	}
	if policy.Resource.Kind != domain.BackupResourceMySQL {
		return domain.NewError(v1.CodeInvalidRequest,
			"mysql 适配器只处理 kind=mysql，got %q", policy.Resource.Kind)
	}
	// 压缩不做限制：mysqldump 输出纯 SQL 文本，应用层压它是净收益（与 postgres 相反）。
	return policy.Validate()
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

	dump, _, toolErr := locateTools()
	if toolErr != nil {
		return fail("clientTools", toolErr.Error()), toolErr
	}
	clientVersion, versionErr := version(ctx, a.exec, dump, "--version")
	if versionErr != nil {
		return fail("clientVersion", "读取客户端版本失败: "+versionErr.Error()), versionErr
	}
	report.ToolVersion = clientVersion
	// 客户端是 MySQL 还是 MariaDB 会写进预检：两者的 mysqldump 参数集并不完全相同，
	// 而「备得上、恢复不了」的排查第一步就是这个。
	ok("clientTools", "mysqldump 与 mysql 均已找到（"+flavor(clientVersion)+"）")

	conn, connErr := a.resolveConnection(ctx, policy)
	if connErr != nil {
		return fail("dsn", connErr.Error()), connErr
	}
	ok("dsn", "凭据可解析，目标 "+conn.endpoint()+"（不含密码，只报告主机与库名）")

	if matchErr := ensureDatabaseMatches(policy, conn); matchErr != nil {
		return fail("databaseMatches", matchErr.Error()), matchErr
	}
	ok("databaseMatches", "策略声明的库与 DSN 一致："+conn.database)

	serverVersion, serverErr := a.serverVersion(ctx, conn)
	if serverErr != nil {
		return fail("serverReachable", "连不上服务端或读不到版本: "+serverErr.Error()), serverErr
	}
	ok("serverReachable", "服务端版本 "+serverVersion)

	clientMajor, serverMajor := majorVersion(clientVersion), majorVersion(serverVersion)
	switch {
	case clientMajor == 0 || serverMajor == 0:
		ok("versionCompatibility", "版本号无法判定（client="+clientVersion+" server="+serverVersion+"）")
	case clientMajor < serverMajor:
		detail := "mysqldump " + clientVersion + " 低于服务端 " + serverVersion +
			"：老客户端备新服务端会在读数据字典时失败，请安装同版本或更新的客户端工具包"
		return fail("versionCompatibility", detail), domain.NewError(v1.CodeBackupPreflightFailed, "%s", detail)
	default:
		ok("versionCompatibility", "客户端 "+clientVersion+" ≥ 服务端 "+serverVersion)
	}

	if size, sizeErr := a.databaseSize(ctx, conn); sizeErr == nil {
		ok("databaseSize", size)
	} else {
		ok("databaseSize", "无法读取库大小（不影响备份）: "+sizeErr.Error())
	}

	return report, nil
}

// Backup 用 mysqldump 把逻辑备份流写进 w。
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
	dump, _, err := locateTools()
	if err != nil {
		return domain.BackupMetadata{}, err
	}
	clientVersion, err := version(ctx, a.exec, dump, "--version")
	if err != nil {
		return domain.BackupMetadata{}, err
	}
	serverVersion, err := a.serverVersion(ctx, conn)
	if err != nil {
		return domain.BackupMetadata{}, err
	}

	argv := []string{dump}
	argv = append(argv, conn.connectionArgs()...)
	argv = append(argv,
		// 一致性快照：不加锁（--single-transaction 会关掉 --lock-tables），
		// 对 InnoDB 才是真的「在线备份」。
		"--single-transaction",
		// 存储过程、事件、触发器都要进备份。少了它们，恢复出来的库看着是好的，
		// 但少了一半逻辑——这类缺失只有在业务跑起来之后才暴露。
		"--routines",
		"--events",
		"--triggers",
		// 不加它，MySQL 8 在没有 PROCESS 权限时会因为读表空间信息而失败。
		"--no-tablespaces",
		// 刻意**不**传 --skip-comments：它会把结束行 `-- Dump completed on ...` 一起去掉，
		// 而 Verify 的判据正是那一行（规格 D15 的实测）。
		conn.database,
	)

	spec := domain.CommandSpec{
		Argv:             argv,
		Environment:      conn.env(),
		SensitiveEnvKeys: conn.sensitiveEnvKeys(),
		Timeout:          dbtools.TimeoutOrDefault(policy.Timeout.Backup, domain.DefaultBackupTimeout),
	}

	result, runErr := a.exec.RunStream(ctx, spec, nil, w)
	if runErr != nil {
		// 保留执行器的错误码，理由与 postgres 相同：重试白名单认的是那些码。
		return domain.BackupMetadata{}, dbtools.ToolFailure(Tool, result, runErr, "")
	}

	return domain.BackupMetadata{
		ResourceKind:  domain.BackupResourceMySQL,
		Tool:          Tool,
		ClientVersion: clientVersion,
		ServerVersion: serverVersion,
	}, nil
}

// Verify 校验备份流，**不接触目标资源、也不需要任何外部工具、连凭据都不用**。
//
// 判据是 mysqldump 自己写的两行：开头的 `-- MySQL dump` 与结尾的 `-- Dump completed`。
// 后者只在 mysqldump 真的跑完时才会出现，因此**任何位置的截断都能被发现**——
// 这一点比 postgres 那边的 `pg_restore --file=/dev/null` 还强（那边砍掉末尾几百字节不报错）。
//
// 它证明的是「这份流是 mysqldump 完整写完的」，**不是**内容正确：
// SQL 的语义是否正确、数据是否齐全，只有隔离恢复能回答。
func (a *Adapter) Verify(ctx context.Context, policy *domain.BackupPolicy, r io.Reader) error {
	if err := a.Validate(ctx, policy); err != nil {
		return err
	}

	window := newStreamWindow(windowSize)
	if _, err := io.Copy(window, &ctxReader{ctx: ctx, src: r}); err != nil {
		if errors.Is(err, context.Canceled) {
			return domain.NewError(v1.CodeExecCancelled, "校验被取消")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return domain.NewError(v1.CodeExecTimeout, "校验超时")
		}
		return domain.NewError(v1.CodeBackupVerifyFailed, "读取备份流失败: %v", err)
	}

	if window.total == 0 {
		return domain.NewError(v1.CodeBackupVerifyFailed, "备份流是空的")
	}
	if !bytes.Contains(window.head, []byte(headerMarker)) {
		return domain.NewError(v1.CodeBackupVerifyFailed,
			"备份流的开头不是 %q，这不是 mysqldump 的输出", headerMarker)
	}
	if !bytes.Contains(window.tail, []byte(completionMarker)) {
		// 最常见的成因是流被截断，其次是有人在参数里加了 --skip-comments。
		return domain.NewError(v1.CodeBackupVerifyFailed,
			"备份流的结尾没有 mysqldump 的结束行（%q）：流是不完整的，"+
				"或备份时误用了 --skip-comments", completionMarker)
	}
	return nil
}

// Restore 恢复一份备份。
//
//   - isolated：建一个**临时库**（同一实例上），恢复到它，完成后删掉。它证明
//     「这份 SQL 真的能恢复」，且不碰真实数据库。
//   - inPlace：恢复到策略声明的库，调用方必须先做显式确认（确认在应用层，规格 D7）。
//
// 「临时库」不是「临时实例」：起一个完整的 MySQL 进程成本与风险都远超收益。
// 代价与 postgres 侧相同——需要建库权限与对端磁盘，且覆盖不到实例级对象
// （账号、授权、全局设置）。
//
// 注意：Backup **刻意没有**用 `--databases`，因为那个选项会在流里写入
// `CREATE DATABASE` 与 `USE <原库>`——隔离恢复把它喂给临时库时，里面的 `USE`
// 会**切回真实库**，于是「隔离」恢复直接写到生产上。不用它，恢复目标就完全由
// 我们传给客户端的库名决定。
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
		return a.restoreInto(ctx, policy, conn, conn.database, r)
	default:
		return a.restoreIsolated(ctx, policy, conn, r)
	}
}

// restoreInto 把 SQL 流喂给 `mysql <库>`。
func (a *Adapter) restoreInto(ctx context.Context, policy *domain.BackupPolicy, conn conninfo, database string, r io.Reader) error {
	client, err := dbtools.Locate("mysql")
	if err != nil {
		return err
	}

	argv := []string{client}
	argv = append(argv, conn.connectionArgs()...)
	argv = append(argv, database)

	spec := domain.CommandSpec{
		Argv:             argv,
		Environment:      conn.env(),
		SensitiveEnvKeys: conn.sensitiveEnvKeys(),
		Timeout:          dbtools.TimeoutOrDefault(policy.Timeout.Restore, domain.DefaultRestoreTimeout),
	}
	// 非交互模式下 mysql 遇到第一个错误就退出，这正是恢复要的行为：
	// 半份数据被当成「恢复成功」比直接失败坏得多。
	result, runErr := a.exec.RunStream(ctx, spec, r, nil)
	if runErr != nil {
		return dbtools.ToolFailure("mysql", result, runErr, v1.CodeBackupRestoreFailed)
	}
	return nil
}

// restoreIsolated 在临时库里恢复，成功后立刻删掉临时库。
func (a *Adapter) restoreIsolated(ctx context.Context, policy *domain.BackupPolicy, conn conninfo, r io.Reader) error {
	temp := dbtools.TempDatabaseName(policy.Name)
	// 先登记再创建：登记在创建之后的话，「建完了、还没登记」这一瞬间崩溃
	// 就会留下一个谁也不知道的孤儿库。
	a.temps.Track(policy.Name, temp)

	if err := a.execSQL(ctx, conn, "CREATE DATABASE `"+temp+"`",
		dbtools.TimeoutOrDefault(policy.Timeout.Restore, domain.DefaultRestoreTimeout)); err != nil {
		return err
	}

	restoreErr := a.restoreInto(ctx, policy, conn, temp, r)

	dropErr := a.execSQL(ctx, conn, "DROP DATABASE IF EXISTS `"+temp+"`",
		dbtools.TimeoutOrDefault(policy.Timeout.Restore, domain.DefaultRestoreTimeout))
	if dropErr == nil {
		a.temps.Untrack(policy.Name, temp)
	}
	if restoreErr != nil {
		return restoreErr
	}
	if dropErr != nil {
		// 内容已经恢复成功了，只是临时库没删掉：不该把整次恢复判为失败
		// （它已经证明了「这份备份能恢复」），但必须说出来——它会一直占着对端的磁盘。
		return domain.NewError(v1.CodeBackupRestoreFailed,
			"隔离恢复成功，但临时库 %s 没能删除，请手工清理: %v", temp, dropErr)
	}
	return nil
}

// Cleanup 删掉本策略下**还没收尾**的临时库。幂等。
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
		for _, name := range names {
			a.temps.Track(policy.Name, name)
		}
		return domain.NewError(v1.CodeBackupInUse,
			"有 %d 个临时库需要清理（%s），但凭据无法解析，请手工删除: %v",
			len(names), strings.Join(names, ", "), err)
	}

	var firstErr error
	for _, name := range names {
		if dropErr := a.execSQL(ctx, conn, "DROP DATABASE IF EXISTS `"+name+"`", cleanupTimeout); dropErr != nil {
			if firstErr == nil {
				firstErr = dropErr
			}
			a.temps.Track(policy.Name, name)
		}
	}
	if firstErr != nil {
		return domain.NewError(v1.CodeBackupInUse,
			"部分临时库未能删除，请手工清理（前缀 %s）: %v", dbtools.TempPrefix(), firstErr)
	}
	return nil
}

// cleanupTimeout 是 Cleanup 使用的固定超时：收尾发生在操作结束之后，
// 不在任何 Operation 的超时预算之内，又不该无限期挂着。
const cleanupTimeout = 60 * time.Second

// queryTimeout 只读查询（版本、库大小）的固定超时。
const queryTimeout = 30 * time.Second

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

func (a *Adapter) resolveConnection(ctx context.Context, policy *domain.BackupPolicy) (conninfo, error) {
	raw, err := dbtools.ResolveSecret(ctx, a.secrets, policy.Resource.DSNSecret)
	if err != nil {
		return conninfo{}, err
	}
	return parseDSN(raw)
}

// locateTools 把工具解析成绝对路径（执行器按绝对路径做 allowedPaths 校验）。
func locateTools() (dump, client string, err error) {
	if dump, err = dbtools.Locate("mysqldump"); err != nil {
		return "", "", err
	}
	if client, err = dbtools.Locate("mysql"); err != nil {
		return "", "", err
	}
	return dump, client, nil
}

func version(ctx context.Context, exec application.Executor, tool string, args ...string) (string, error) {
	result, err := exec.Run(ctx, domain.CommandSpec{Argv: append([]string{tool}, args...)})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

// flavor 报告客户端是 MySQL 还是 MariaDB。参数集在两者之间并不完全相同，
// 预检里说出来能省掉一轮排查。
func flavor(version string) string {
	if strings.Contains(version, "MariaDB") {
		return "MariaDB"
	}
	return "MySQL"
}

func (a *Adapter) serverVersion(ctx context.Context, conn conninfo) (string, error) {
	out, err := a.query(ctx, conn, "SELECT VERSION()")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (a *Adapter) databaseSize(ctx context.Context, conn conninfo) (string, error) {
	out, err := a.query(ctx, conn,
		"SELECT CONCAT(ROUND(SUM(data_length + index_length) / 1024 / 1024, 1), ' MiB') "+
			"FROM information_schema.tables WHERE table_schema = DATABASE()")
	if err != nil {
		return "", err
	}
	return "库大小 " + strings.TrimSpace(out), nil
}

// query 跑一条只读 SQL 并返回结果。
func (a *Adapter) query(ctx context.Context, conn conninfo, sql string) (string, error) {
	client, err := dbtools.Locate("mysql")
	if err != nil {
		return "", err
	}
	argv := []string{client}
	argv = append(argv, conn.connectionArgs()...)
	argv = append(argv,
		"--skip-column-names", // 只要值，不要在输出里掺列名
		"--batch",             // 制表符分隔、不画表格边框
		"--execute", sql,
		conn.database,
	)
	result, runErr := a.exec.Run(ctx, domain.CommandSpec{
		Argv:             argv,
		Environment:      conn.env(),
		SensitiveEnvKeys: conn.sensitiveEnvKeys(),
		Timeout:          queryTimeout,
	})
	if runErr != nil {
		return "", dbtools.ToolFailure("mysql", result, runErr, v1.CodeBackupPreflightFailed)
	}
	return result.Stdout, nil
}

// execSQL 跑一条会改状态的 SQL（建/删临时库）。
func (a *Adapter) execSQL(ctx context.Context, conn conninfo, sql string, timeout time.Duration) error {
	client, err := dbtools.Locate("mysql")
	if err != nil {
		return err
	}
	argv := []string{client}
	argv = append(argv, conn.connectionArgs()...)
	argv = append(argv, "--execute", sql)

	result, runErr := a.exec.Run(ctx, domain.CommandSpec{
		Argv:             argv,
		Environment:      conn.env(),
		SensitiveEnvKeys: conn.sensitiveEnvKeys(),
		Timeout:          timeout,
	})
	if runErr != nil {
		return dbtools.ToolFailure("mysql", result, runErr, v1.CodeBackupRestoreFailed)
	}
	return nil
}

// majorVersion 从版本串里取出主版本号。
//
// `mysqldump  Ver 8.0.46 for Linux on aarch64 (MySQL Community Server - GPL)` 与
// MariaDB 的 `mysqldump  Ver 10.19 Distrib 10.11.6-MariaDB, for debian-linux-gnu`
// 里，**第一个数字段都是主版本**；`SELECT VERSION()` 的 `8.0.46` 也是。
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

// streamWindow 是 io.Writer，记住流的**开头**与**结尾**各一小段。
//
// 只留尾巴不够：开头是 mysqldump 的自我标识，结尾是它的结束标记，两者说的是
// 不同的失败——「拿错了文件」与「没写完」。
type streamWindow struct {
	head     []byte
	tail     []byte
	headSize int
	tailSize int
	total    int64
}

func newStreamWindow(size int) *streamWindow {
	return &streamWindow{headSize: size, tailSize: size}
}

func (w *streamWindow) Write(p []byte) (int, error) {
	if len(w.head) < w.headSize {
		need := w.headSize - len(w.head)
		if need > len(p) {
			need = len(p)
		}
		w.head = append(w.head, p[:need]...)
	}

	if len(p) >= w.tailSize {
		w.tail = append(w.tail[:0], p[len(p)-w.tailSize:]...)
	} else {
		w.tail = append(w.tail, p...)
		if len(w.tail) > w.tailSize {
			w.tail = append(w.tail[:0], w.tail[len(w.tail)-w.tailSize:]...)
		}
	}
	w.total += int64(len(p))
	return len(p), nil
}

// ctxReader 让 io.Copy 有机会响应取消：读一整份归档可能要几分钟。
type ctxReader struct {
	ctx context.Context
	src io.Reader
}

func (r *ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.src.Read(p)
}

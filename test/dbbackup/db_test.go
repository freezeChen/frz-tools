// Package dbbackup 在**真实数据库实例**上跑数据库备份适配器的共享合约测试。
//
// 它受环境变量控制：没有实例时整包跳过，因此 `go test ./...` 在开发者机器上仍然全绿。
// 起实例的活儿交给 test/linux/verify-db.sh（证据类型是 **Linux 容器**承载的真实数据库，
// 与「Linux 主机」是两类不同证据）。
//
// 为什么这里必须用真实例而不是假实现：适配器的全部风险都在「我们拼的 argv 与工具的
// 真实行为是否一致」上——`--single-transaction` 到底有没有给出快照、`--clean --if-exists`
// 到底能不能让目标等于归档、`pg_restore --file=/dev/null` 到底读不读得动一份真实的
// 自定义归档、`mysqldump` 到底写不写那行结束标记。这些没有一个能靠假实现证明。
//
// 夹具（Populate / Wipe / Fingerprint）刻意用 psql / mysql 的 argv 调用实现，
// 而不是引入数据库驱动：多一个依赖换来的只是把同一件事做第二遍，而这里要的
// 恰恰是「用真实的客户端工具」。
package dbbackup

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/freezeChen/frz-tools/internal/adapters/backup/mysql"
	"github.com/freezeChen/frz-tools/internal/adapters/backup/postgres"
	"github.com/freezeChen/frz-tools/internal/adapters/executor"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/application/backupcontract"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// 实例地址由环境变量给出。空值 = 没装实例 = 跳过。
const (
	postgresDSNEnv = "FRZ_TEST_POSTGRES_DSN"
	mysqlDSNEnv    = "FRZ_TEST_MYSQL_DSN"
	mariadbDSNEnv  = "FRZ_TEST_MARIADB_DSN"
	// probeTable 是夹具用的表名；固定一个，好让指纹查询写得简单。
	probeTable = "frz_probe"
)

func TestPostgresAdapterContract(t *testing.T) {
	dsn := requireDSN(t, postgresDSNEnv)
	runContract(t, dsn, domain.BackupResourcePostgres)
}

func TestMySQLAdapterContract(t *testing.T) {
	dsn := requireDSN(t, mysqlDSNEnv)
	runContract(t, dsn, domain.BackupResourceMySQL)
}

func TestMariaDBAdapterContract(t *testing.T) {
	dsn := requireDSN(t, mariadbDSNEnv)
	runContract(t, dsn, domain.BackupResourceMySQL)
}

func requireDSN(t *testing.T, env string) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(env))
	if dsn == "" {
		t.Skipf("未设置 %s：需要真实数据库实例（见 test/linux/verify-db.sh）", env)
	}
	return dsn
}

// runContract 把共享合约套在真实实例上。
func runContract(t *testing.T, dsn string, kind domain.BackupResourceKind) {
	t.Helper()

	// 用**真实的执行器**：适配器在生产里就是通过它跑外部工具的，换成假的
	// 就等于把「argv 能不能被 allowedPaths 放行、流式管道通不通」一起测没了。
	// allow 在这里全放行：被放行的是哪个目录属于部署配置，不是适配器的语义。
	runner := executor.New(func(string) bool { return true }, 5*time.Minute, 1<<20, nil)
	secrets := staticSecrets{value: dsn}

	backupcontract.Run(t, func(t *testing.T) backupcontract.Harness {
		client := newFixture(t, kind, dsn)

		var adapter application.BackupAdapter
		switch kind {
		case domain.BackupResourcePostgres:
			adapter = postgres.New(runner, secrets)
		case domain.BackupResourceMySQL:
			adapter = mysql.New(runner, secrets)
		default:
			t.Fatalf("未知资源种类 %q", kind)
		}

		return backupcontract.Harness{
			Adapter: adapter,
			NewPolicy: func(t *testing.T, name string) *domain.BackupPolicy {
				t.Helper()
				policy := &domain.BackupPolicy{
					APIVersion: domain.BackupPolicyAPIVersion,
					Kind:       domain.BackupPolicyKind,
					Name:       name,
					Resource: domain.BackupResource{
						Kind:      kind,
						DSNSecret: domain.SecretRef{Kind: domain.SecretKindFile, Name: "contract-dsn"},
						Database:  client.database,
					},
					Encoding: domain.BackupEncoding{
						// postgres 的自定义归档自带压缩、必须声明 none；
						// mysql 是纯 SQL 文本，压它是净收益。
						Compression: client.compression,
						Encryption:  domain.BackupEncryption{Enabled: false},
					},
					Retention: domain.BackupRetention{KeepLast: 1},
				}
				if err := policy.Validate(); err != nil {
					t.Fatalf("夹具策略本身不合法: %v", err)
				}
				return policy
			},
			Populate:    client.populate,
			Fingerprint: client.fingerprint,
			Wipe:        client.wipe,
		}
	})
}

// staticSecrets 把所有 SecretRef 都解析成同一份 DSN。
type staticSecrets struct{ value string }

func (s staticSecrets) Resolve(context.Context, domain.SecretRef) (string, error) {
	return s.value, nil
}

// ---------------------------------------------------------------------------
// 夹具：用真实的客户端工具读写探针表
// ---------------------------------------------------------------------------

// fixture 用 psql / mysql 在真实实例上造内容、读指纹、清空内容。
type fixture struct {
	t        *testing.T
	kind     domain.BackupResourceKind
	runner   application.Executor
	host     string
	port     string
	user     string
	password string
	database string
	// sqlTool 是夹具自己用的客户端名。
	//
	// MariaDB 的客户端包里可能**没有** mysql / mysqldump 这两个名字（官方
	// mariadb:11 镜像就只有 mariadb / mariadb-dump），所以这里也按优先级找一次：
	// 夹具要模拟的是「这台主机上的运维手边有什么」，而不是假定全世界都叫 mysql。
	sqlTool     string
	compression domain.Compression
}

// pickTool 按优先级挑一个能解析到的工具名。
func pickTool(t *testing.T, names ...string) string {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	t.Skipf("找不到 %s 中的任何一个", strings.Join(names, " / "))
	return ""
}

// newFixture 解析 DSN 并确认实例真的连得上。
//
// 这里刻意**不**复用适配器内部那份 DSN 解析：那是被测代码。夹具要是也用被测代码
// 解析地址，「解析错了」这件事就会同时骗过两边。二十行的 url.Parse 换一份独立的
// 判断，值。
func newFixture(t *testing.T, kind domain.BackupResourceKind, dsn string) *fixture {
	t.Helper()

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DSN 无法解析: %v", err)
	}
	client := &fixture{
		t:           t,
		kind:        kind,
		runner:      executor.New(func(string) bool { return true }, 2*time.Minute, 1<<20, nil),
		host:        parsed.Hostname(),
		port:        parsed.Port(),
		user:        parsed.User.Username(),
		database:    strings.TrimPrefix(parsed.Path, "/"),
		compression: domain.CompressionGzip,
	}
	if kind == domain.BackupResourcePostgres {
		client.sqlTool = pickTool(t, "psql")
	} else {
		client.sqlTool = pickTool(t, "mysql", "mariadb")
	}
	client.password, _ = parsed.User.Password()
	if kind == domain.BackupResourcePostgres {
		// pg_dump --format=custom 自带压缩，策略必须声明 none（规格 D11）。
		client.compression = domain.CompressionNone
	}
	if client.database == "" {
		t.Fatalf("DSN 里没有库名: %s", dsn)
	}
	// 连不上就别把失败伪装成一堆看不懂的用例失败。
	if out := client.query("SELECT 1"); !strings.Contains(out, "1") {
		t.Fatalf("连不上实例（%s）: %s", dsn, out)
	}
	return client
}

// clientArgv 拼出 psql 或 mysql 的 argv。
//
// 两边刻意不同：postgres 侧连库名都在 --dbname 里（与适配器一致），mysql 侧库名是
// 位置参数、连接参数分散。夹具要模拟的是「运维手边的客户端」，不是适配器，
// 所以这里按各家自己的习惯写，而不是从适配器里抽一份出来共用。
func (f *fixture) clientArgv(sql string) []string {
	if f.kind == domain.BackupResourcePostgres {
		argv := []string{f.sqlTool, "--no-psqlrc", "--no-align", "--tuples-only", "--set=ON_ERROR_STOP=1"}
		if f.host != "" {
			argv = append(argv, "--host="+f.host)
		}
		if f.port != "" {
			argv = append(argv, "--port="+f.port)
		}
		if f.user != "" {
			argv = append(argv, "--username="+f.user)
		}
		argv = append(argv, "--dbname="+f.database, "--command", sql)
		return argv
	}

	argv := []string{f.sqlTool}
	if f.host != "" {
		argv = append(argv, "--host="+f.host)
	}
	if f.port != "" {
		argv = append(argv, "--port="+f.port)
	}
	if f.user != "" {
		argv = append(argv, "--user="+f.user)
	}
	argv = append(argv, "--batch", "--skip-column-names", "--execute", sql)
	return append(argv, f.database)
}

func (f *fixture) env() map[string]string {
	if f.password == "" {
		return nil
	}
	if f.kind == domain.BackupResourcePostgres {
		return map[string]string{"PGPASSWORD": f.password}
	}
	return map[string]string{"MYSQL_PWD": f.password}
}

// run 跑一条 SQL；失败即测试失败——夹具出错不该被悄悄当成「资源为空」。
func (f *fixture) run(sql string) string {
	f.t.Helper()
	result, err := f.runner.Run(context.Background(), domain.CommandSpec{
		Argv:        f.clientArgv(sql),
		Environment: f.env(),
		Timeout:     2 * time.Minute,
	})
	if err != nil {
		f.t.Fatalf("执行 SQL 失败: %v\nSQL: %s\nstderr: %s", err, sql, result.Stderr)
	}
	return result.Stdout
}

// query 跑一条只读查询，出错时把 stderr 作为结果返回（用于「连得上吗」这类判断）。
func (f *fixture) query(sql string) string {
	result, err := f.runner.Run(context.Background(), domain.CommandSpec{
		Argv:        f.clientArgv(sql),
		Environment: f.env(),
		Timeout:     time.Minute,
	})
	if err != nil {
		return result.Stderr
	}
	return result.Stdout
}

// populate 造出一份可辨认的内容，并返回它的指纹。
func (f *fixture) populate(t *testing.T, _ *domain.BackupPolicy) string {
	t.Helper()

	if f.kind == domain.BackupResourcePostgres {
		f.run(fmt.Sprintf(
			`DROP TABLE IF EXISTS %[1]s; CREATE TABLE %[1]s (id int primary key, v text);`+
				` INSERT INTO %[1]s VALUES (1,'alpha'),(2,'beta'),(3,'gamma');`, probeTable))
	} else {
		f.run(fmt.Sprintf(
			`DROP TABLE IF EXISTS %[1]s; CREATE TABLE %[1]s (id int primary key, v varchar(32));`+
				` INSERT INTO %[1]s VALUES (1,'alpha'),(2,'beta'),(3,'gamma');`, probeTable))
	}
	return f.fingerprint(t, nil)
}

// wipe 把探针表整张删掉：只有这样「恢复真的写回来了」才说得通——
// 从空表恢复到有数据证明不了什么。
func (f *fixture) wipe(t *testing.T, _ *domain.BackupPolicy) {
	t.Helper()
	f.run(fmt.Sprintf("DROP TABLE IF EXISTS %s", probeTable))
}

// fingerprint 返回探针表当前内容的指纹。
//
// 「表不存在」与「表是空的」必须给出不同的指纹：前者是没恢复，后者是恢复了个空表，
// 两者都不等于「恢复成功」。
func (f *fixture) fingerprint(t *testing.T, _ *domain.BackupPolicy) string {
	t.Helper()

	if f.kind == domain.BackupResourcePostgres {
		exists := strings.TrimSpace(f.query(fmt.Sprintf("SELECT to_regclass('public.%s') IS NOT NULL", probeTable)))
		if !strings.HasPrefix(exists, "t") {
			return "absent"
		}
		return strings.TrimSpace(f.query(fmt.Sprintf(
			`SELECT count(*) || '|' || coalesce(sum(id),0) || '|' ||`+
				` coalesce(md5(string_agg(v, ',' ORDER BY id)),'-') FROM %s`, probeTable)))
	}
	exists := strings.TrimSpace(f.query(fmt.Sprintf(
		`SELECT count(*) FROM information_schema.tables`+
			` WHERE table_schema = DATABASE() AND table_name = '%s'`, probeTable)))
	if exists != "1" {
		return "absent"
	}
	return strings.TrimSpace(f.query(fmt.Sprintf(
		`SELECT count(*) || '|' || coalesce(sum(id),0) || '|' ||`+
			` coalesce(md5(group_concat(v ORDER BY id SEPARATOR ',')),'-') FROM %s`, probeTable)))
}

func TestMain(m *testing.M) {
	// 让「没装实例」这件事在输出里明确出现，而不是让人以为跑过了。
	if os.Getenv(postgresDSNEnv) == "" && os.Getenv(mysqlDSNEnv) == "" && os.Getenv(mariadbDSNEnv) == "" {
		fmt.Fprintln(os.Stderr, "dbbackup: 未设置任何 FRZ_TEST_*_DSN，整包跳过（见 test/linux/verify-db.sh）")
	}
	os.Exit(m.Run())
}

package mysql

import (
	"fmt"
	"net/url"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// conninfo 是一次连接的解析结果。
//
// **密码只以结构体字段的形式留在这里**，用途只有一处：设进 MYSQL_PWD。它刻意不进 argv——
// argv 会出现在 `ps` 的输出里，任何本机用户都看得到；环境变量只对同用户与 root 可见
// （迭代 2 规格 D13）。
//
// 与 postgres 不同，MySQL 的客户端**不认连接串**：它只接受 --host/--port/--user 这些
// 分散的参数，所以这里必须把 URI 拆开。这也意味着 DSN 里多出来的参数没有去处——
// 因此**明确拒绝**而不是忽略：静默丢掉一个 `?tls=...` 比直接报错危险得多。
type conninfo struct {
	host     string
	port     string
	username string
	password string
	database string
}

// parseDSN 解析并校验一份 DSN。
//
// 只接受 `mysql://user:password@host:port/dbname` 这一种形状（规格 D13）。
func parseDSN(raw string) (conninfo, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return conninfo{}, domain.NewError(v1.CodeSecretUnresolved, "DSN 是空的")
	}
	if !strings.HasPrefix(trimmed, "mysql://") {
		return conninfo{}, domain.NewError(v1.CodeBackupPreflightFailed,
			"DSN 必须写成 mysql://[user[:password]@]host[:port]/dbname")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return conninfo{}, domain.NewError(v1.CodeBackupPreflightFailed, "DSN 无法解析: %v", err)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return conninfo{}, domain.NewError(v1.CodeBackupPreflightFailed,
			"DSN 不接受查询参数（got %q）：MySQL 客户端不认连接串，多出来的参数没有去处，"+
				"忽略它就等于静默丢掉一个连接选项", parsed.RawQuery)
	}

	database := strings.TrimPrefix(parsed.Path, "/")
	if database == "" {
		return conninfo{}, domain.NewError(v1.CodeBackupPreflightFailed,
			"DSN 里没有库名（形如 mysql://user:password@host:3306/dbname）")
	}
	if strings.Contains(database, "/") {
		return conninfo{}, domain.NewError(v1.CodeBackupPreflightFailed,
			"DSN 的库名里出现了 /（%q）：URI 形式只表达「一台主机上的一个库」", database)
	}

	info := conninfo{
		host:     parsed.Hostname(),
		port:     parsed.Port(),
		database: database,
	}
	if info.host == "" {
		return conninfo{}, domain.NewError(v1.CodeBackupPreflightFailed,
			"DSN 里没有主机名（形如 mysql://user:password@host:3306/dbname）")
	}
	if parsed.User != nil {
		info.username = parsed.User.Username()
		info.password, _ = parsed.User.Password()
	}
	return info, nil
}

// env 返回要交给子进程的环境变量。
//
// 密码为空时不设 MYSQL_PWD：设成空串与「没设」在客户端里是两回事，
// 后者还能让 `~/.my.cnf` 之类的既有配置起作用。
func (c conninfo) env() map[string]string {
	if c.password == "" {
		return nil
	}
	return map[string]string{"MYSQL_PWD": c.password}
}

// sensitiveEnvKeys 是与 env 配套的脱敏名单。
func (c conninfo) sensitiveEnvKeys() []string { return []string{"MYSQL_PWD"} }

// connectionArgs 是**两个工具都认**的连接参数，不含密码。
//
// 刻意只放最小的公共子集：`mysqldump` 并不接受 mysql 客户端的全部选项——
// 例如 `--connect-timeout` 只有 `mysql` 认，给了 mysqldump 就是
// `unknown variable 'connect-timeout=15'` 并直接退出 7（这是真实实例跑出来的，
// 不是文档里读来的）。所以客户端特有的选项放在 clientArgs 里。
func (c conninfo) connectionArgs() []string {
	args := []string{"--host=" + c.host}
	if c.port != "" {
		args = append(args, "--port="+c.port)
	}
	if c.username != "" {
		args = append(args, "--user="+c.username)
	}
	return args
}

// clientArgs 是 `mysql` 客户端的连接参数：公共子集再加上它独有的建连超时。
//
// 不设它的话，网络不可达时客户端会一直挂着直到操作超时（默认 1 小时），
// 期间一个 worker 被白占。
func (c conninfo) clientArgs() []string {
	return append(c.connectionArgs(), "--connect-timeout=15")
}

// endpoint 是给日志与断言看的一行摘要，**不含**密码。
func (c conninfo) endpoint() string {
	return fmt.Sprintf("%s/%s", c.host, c.database)
}

// ensureDatabaseMatches 校验策略声明的库与 DSN 指向的库是同一个。
//
// 这条挡的是「策略里写 orders、DSN 却指着生产库」这类误配置：备份会跑得好好的、
// 备的却是另一个库，直到真正要恢复时才发现（规格 D13）。
func ensureDatabaseMatches(policy *domain.BackupPolicy, conn conninfo) error {
	if policy.Resource.Database != conn.database {
		return domain.NewError(v1.CodeBackupPreflightFailed,
			"策略声明的库是 %q，而 DSN 指向 %q——两者必须是同一个库，"+
				"否则备份会静默地备错对象",
			policy.Resource.Database, conn.database)
	}
	return nil
}

package postgres

import (
	"fmt"
	"net/url"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// conninfo 是一次连接的解析结果。
//
// **密码只以结构体字段的形式留在这里**，用途只有一处：设进 PGPASSWORD。它刻意不进 argv——
// argv 会出现在 `ps` 的输出里，任何本机用户都看得到；环境变量只对同用户与 root 可见
// （迭代 2 规格 D13）。
type conninfo struct {
	// uri 是**已剥掉密码**的连接串，用于 `--dbname`：主机、端口、用户、库名、
	// sslmode 等参数原样保留，免得我们自己重新拼一遍把某个参数弄丢。
	uri      *url.URL
	username string
	password string
	database string
	host     string
}

// parseDSN 解析并校验一份 DSN。
//
// 只接受 URI 形式（`postgres://` 与 `postgresql://`）。libpq 的 `keyword=value` 连接串
// 有自己的转义规则（引号、反斜杠），自己实现一份的结果是**连接串被静默改变**——
// 连错库比连不上坏得多，所以这里明确拒绝并告诉用户该写成什么（规格 D13）。
func parseDSN(raw string) (conninfo, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return conninfo{}, domain.NewError(v1.CodeSecretUnresolved, "DSN 是空的")
	}
	if !strings.HasPrefix(trimmed, "postgres://") && !strings.HasPrefix(trimmed, "postgresql://") {
		return conninfo{}, domain.NewError(v1.CodeBackupPreflightFailed,
			"DSN 必须是 URI 形式（postgres://[user[:password]@]host[:port]/dbname[?参数]）；"+
				"本版本不接受 libpq 的 keyword=value 连接串，请改写成 URI")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return conninfo{}, domain.NewError(v1.CodeBackupPreflightFailed, "DSN 无法解析: %v", err)
	}

	database := strings.TrimPrefix(parsed.Path, "/")
	if database == "" {
		return conninfo{}, domain.NewError(v1.CodeBackupPreflightFailed,
			"DSN 里没有库名（形如 postgres://host:5432/dbname）")
	}
	if strings.Contains(database, "/") {
		return conninfo{}, domain.NewError(v1.CodeBackupPreflightFailed,
			"DSN 的库名里出现了 /（%q）：URI 形式只表达「一台主机上的一个库」", database)
	}

	info := conninfo{uri: parsed, database: database, host: parsed.Host}
	if parsed.User != nil {
		info.username = parsed.User.Username()
		info.password, _ = parsed.User.Password()
		// 剥掉密码：这一步之后 conninfo.uri 里就不存在密码了，
		// 即使有人把它拼进日志也带不出去。
		if info.username == "" {
			info.uri.User = nil
		} else {
			info.uri.User = url.User(info.username)
		}
	}
	return info, nil
}

// argvURI 返回给 `--dbname` 用的连接串（不含密码）。
func (c conninfo) argvURI() string { return c.uri.String() }

// withDatabase 返回一份指向另一个库的连接串，用于隔离恢复的临时库。
func (c conninfo) withDatabase(name string) string {
	clone := *c.uri
	clone.Path = "/" + name
	return clone.String()
}

// env 返回要交给子进程的环境变量。
//
// 密码为空时不设 PGPASSWORD：设成空串与「没设」在 libpq 里是两回事，前者会覆盖掉
// 用户可能已经配好的 .pgpass 之外的路径，语义上更差。
func (c conninfo) env() map[string]string {
	env := map[string]string{
		// 没有它，网络不可达时连接会一直挂着直到操作超时（默认 1 小时），
		// 期间一个 worker 被白占。它只限定**建连**这一段。
		"PGCONNECT_TIMEOUT": "15",
	}
	if c.password != "" {
		env["PGPASSWORD"] = c.password
	}
	return env
}

// sensitiveEnvKeys 是与 env 配套的脱敏名单。
func (c conninfo) sensitiveEnvKeys() []string { return []string{"PGPASSWORD"} }

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

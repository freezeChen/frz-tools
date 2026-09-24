// Package dbtools 是两个数据库备份适配器（postgres / mysql）共用的部分：
// 凭据解析、外部工具的定位，以及隔离恢复用的临时库管理。
//
// 这些都是**语义必须一致**的东西：临时库名要满足对端的标识符限制、要能被运维一眼
// 认出来、清理要幂等——各写一份的结果是两边在不同的地方破。工具特定的一切
// （argv 怎么拼、怎么校验、怎么恢复）留在各自的适配器里，不上提到这里。
package dbtools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"strings"
	"sync"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// tempPrefix 是隔离恢复临时库的统一前缀：运维据此找出残留（`frz_restore_%`）。
const tempPrefix = "frz_restore_"

// maxIdentifierBytes 是对端标识符的**最短**公共上限：PostgreSQL 是 63 字节，
// MySQL 是 64 字符。取小的那个，两边的临时库名规则就只有一套。
//
// 超限不是「报错」而是「被静默截断」——PostgreSQL 会截断超长标识符并只发一个
// NOTICE，于是 DROP DATABASE 拿着一个更长的名字去删，删不掉、还不报错。
const maxIdentifierBytes = 63

// Temps 按策略名跟踪「还没收尾的临时库」。
//
// 按策略名而不是 operationID 索引，是因为端口的 Restore 拿不到 operationID
// （与 files 适配器的临时目录同一套办法）。因此这里记的是**被中断**的那一次留下的
// 东西——Cleanup 是它们唯一的出口。
type Temps struct {
	mu       sync.Mutex
	byPolicy map[string][]string
}

// Track 记下本策略下新建的临时库。
func (t *Temps) Track(policy, name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byPolicy == nil {
		t.byPolicy = map[string][]string{}
	}
	t.byPolicy[policy] = append(t.byPolicy[policy], name)
}

// Untrack 忘掉一个已经自己收尾的临时库。
func (t *Temps) Untrack(policy, name string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// 刻意新建切片而不是复用底层数组：在 t.byPolicy[policy] 上切片再 append
	// 会与正在遍历的那个切片别名，边读边写同一个底层数组。
	var kept []string
	for _, existing := range t.byPolicy[policy] {
		if existing != name {
			kept = append(kept, existing)
		}
	}
	if len(kept) == 0 {
		delete(t.byPolicy, policy)
		return
	}
	t.byPolicy[policy] = kept
}

// Drain 取出并清空本策略下记录的临时库。
func (t *Temps) Drain(policy string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	names := t.byPolicy[policy]
	delete(t.byPolicy, policy)
	return names
}

// TempDatabaseName 生成隔离恢复用的临时库名。
//
// 名字里带策略名是为了可读（运维看得到它在伺候哪份策略），带随机后缀是为了
// 同一策略的两份备份**同时**恢复时不撞名——恢复操作的 resource 是具体的备份 ID，
// 不是策略，所以同一策略下的两次恢复并不互斥。
func TempDatabaseName(policyName string) string {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		panic("dbtools: entropy source failed: " + err.Error())
	}
	name := tempPrefix + identifierFragment(policyName) + "_" + hex.EncodeToString(suffix)
	return truncateIdentifier(name)
}

// TempPrefix 返回临时库的公共前缀。
func TempPrefix() string { return tempPrefix }

// identifierFragment 把策略名压成标识符安全的一段：
// 只保留字母、数字与下划线，其余（点、连字符）一律换成下划线。
//
// 不做引号转义而是直接替换，是因为未加引号的标识符在 PostgreSQL 里会被折叠成小写，
// 一旦折叠，DROP 时用的名字与 CREATE 时的就不是同一个字符串了——这类不一致
// 只在清理时暴露，而且表现为「删不掉、也不报错」。
func identifierFragment(value string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			out.WriteRune(r)
		default:
			out.WriteByte('_')
		}
	}
	fragment := out.String()
	if fragment == "" {
		fragment = "policy"
	}
	return fragment
}

// truncateIdentifier 把标识符截到两端的公共上限之内，保留随机后缀。
func truncateIdentifier(name string) string {
	if len(name) <= maxIdentifierBytes {
		return name
	}
	// 后缀（"_" + 8 位十六进制）必须留住：它才是唯一性所在。
	const suffixLen = 9
	keep := maxIdentifierBytes - suffixLen
	return name[:keep] + name[len(name)-suffixLen:]
}

// Locate 把外部工具名解析成**绝对路径**，并确认它可执行。
//
// 解析成绝对路径是必需的而不是讲究：执行器的 execution.allowedPaths 比较的是绝对路径，
// 直接传裸名字会被判为「不在允许目录内」——而那是个让人一头雾水的报错。
// 分工是清楚的：**适配器决定用哪个工具，配置决定它允许待在哪儿**。
func Locate(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", domain.NewError(v1.CodeBackupPreflightFailed,
			"找不到 %s（需要安装对应的客户端工具包）: %v", name, err)
	}
	return path, nil
}

// LocateAny 按优先级解析工具名，返回解析到的路径与**实际用到的名字**。
//
// 需要它是因为同一件工具在不同发行版上叫不同的名字：MariaDB 从 10.5 起把
// mysqldump / mysql 改名为 mariadb-dump / mariadb，而且**有些客户端包不再提供旧名字**
// ——官方 `mariadb:11` 镜像里只有 `mariadb-dump`，没有 `mysqldump`。
// 只认 MySQL 的名字，会让「支持 MariaDB」这句话在真实的 MariaDB 主机上落空，
// 而且表现成「工具没装」——那是个会把排查带偏的报错。
func LocateAny(names ...string) (path, used string, err error) {
	for _, name := range names {
		if resolved, lookupErr := exec.LookPath(name); lookupErr == nil {
			return resolved, name, nil
		}
	}
	return "", "", domain.NewError(v1.CodeBackupPreflightFailed,
		"找不到 %s 中的任何一个（需要安装 MySQL 或 MariaDB 的客户端工具包）",
		strings.Join(names, " / "))
}

// ResolveSecret 解析一次凭据引用。
//
// 明文只在**使用时刻**出现，不落库、不落日志——与 1a 的凭据约定一致
// （迭代 2 规格 D13）。
func ResolveSecret(ctx context.Context, resolver application.SecretResolver, ref domain.SecretRef) (string, error) {
	if resolver == nil {
		return "", domain.NewError(v1.CodeSecretUnresolved,
			"本部署没有配置凭据解析器，无法解析 %s", ref.String())
	}
	value, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return "", domain.NewError(v1.CodeSecretUnresolved,
			"凭据 %s 无法解析: %v", ref.String(), err)
	}
	if strings.TrimSpace(value) == "" {
		return "", domain.NewError(v1.CodeSecretUnresolved, "凭据 %s 是空的", ref.String())
	}
	return value, nil
}

// ResultDetail 把一次命令执行的 stderr 收成一句可读的补充说明（有长度上限）。
//
// 非零退出时真正有用的信息几乎总在 stderr 里；原样拼进错误信息会让一条 Operation
// 记录长到没法看，所以截断。
func ResultDetail(result domain.Result) string {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		return ""
	}
	const limit = 500
	if len(detail) > limit {
		return detail[:limit] + "…（已截断）"
	}
	return detail
}

// ToolFailure 把外部工具的执行失败整理成一条带上下文、带码的错误。
//
// force 为空表示**保留**执行器给出的错误码。备份路径要它：非零退出的
// EXEC_EXIT_NONZERO 与 EXEC_TIMEOUT 都在 1d 的重试白名单里，翻译成别的码
// 就等于把「这次失败可以再试」这个判断抹掉。恢复与校验路径则相反——它们传 force，
// 因为「这次恢复没成功」「这份归档校验不过」才是运维要的答案。
//
// 取消、超时与权限错误在任何路径上都原样上抛：这三个码各自对应上层一套明确决策
// （取消要收尾、超时可能重试、权限要看 allowedPaths），换掉就是把决策依据抹了。
func ToolFailure(action string, result domain.Result, err error, force v1.ErrorCode) error {
	if err == nil {
		return nil
	}

	code := domain.CodeOf(err)
	switch code {
	case v1.CodeExecCancelled, v1.CodeExecTimeout, v1.CodePermissionDenied:
		return err
	}

	message := action + " 失败: " + domain.MessageOf(err)
	if detail := ResultDetail(result); detail != "" {
		message += "；stderr: " + detail
	}
	if force == "" {
		force = code
	}
	return domain.NewError(force, "%s", message)
}

// TimeoutOrDefault 取策略声明的超时；未声明时回落到仓库的默认值。
//
// 适配器不假设调用方已经跑过 `policy.Validate()`（合约测试就直接调 Backup），
// 因此零值必须在这里兜住，而不是让命令拿着 0 去跑。
func TimeoutOrDefault(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

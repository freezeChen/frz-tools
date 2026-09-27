package v1

import "time"

// IdentityResponse 回答「你是谁、我以什么身份连上来的、这次走的是哪条路」（迭代 5a）。
//
// 它与 HealthResponse 刻意分开：health 回答「你活着吗」（并且真的去 ping 数据库），
// identity 回答「我到底打到了哪台机、以什么身份」。混成一个端点会让 `opsctl health`
// 与 `opsctl host check` 关心的事互相污染。
//
// backend / clientCn / scope 是给远程排障用的三个字段：
// 「我明明给了 --host，怎么打到了本机」必须一眼看得出来（backend=unix 就说明打错了）；
// 而「我以什么身份连上来的」是排授权问题的第一句话。
type IdentityResponse struct {
	APIVersion string `json:"apiVersion"`
	// Hostname 是这台机器的 os.Hostname()。它可能与 host 记录里的名字不同——
	// 记录里的名字是运维起的标签，这里是机器自己报的。
	Hostname string `json:"hostname"`
	// DaemonVersion 是构建信息（模块版本 + VCS 修订）。未注入时为**空串**，
	// 不是 "unknown"：空串是诚实的事实，而 "unknown" 会被读成「查过了、查不到」。
	DaemonVersion string    `json:"daemonVersion"`
	StartedAt     time.Time `json:"startedAt"`
	// Applications 是这台机器上登记的应用数。它**可能是 null**：库读不出来时，
	// 「查不到」与「一台空机器」是两件必须分辨的事——前者说明这台机还有别的问题。
	// 不为这个而让整个端点失败：host check 最需要的那几个字段（backend / clientCn）
	// 恰恰是库挂了也答得出来的。
	Applications *int `json:"applications"`

	// Backend 是这次请求实际走的路径：unix 或 tls。
	Backend string `json:"backend"`
	// ClientCn 是认证结果里的客户端名字；Unix socket 形态下为空串（那条路没有
	// 证书，身份由 socket 文件模式表达）。
	ClientCn string `json:"clientCn"`
	// Scope 是这次请求被授予的档位：read 或 write。
	Scope string `json:"scope"`
	// ApplicationsAllowed 是这个身份的**应用白名单**；空数组表示这台机器上的全部应用
	// （刻意不回 null：null 会被读成「没有信息」，而「全部」是确定的语义）。
	ApplicationsAllowed []string `json:"applicationsAllowed"`
}

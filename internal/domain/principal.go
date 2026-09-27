package domain

import (
	"context"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// Principal 是**已经认证过的**调用方身份（迭代 5a）。
//
// 它只回答两件事：「谁做的」（审计的 actor）与「远程能不能做」（授权）。
// 刻意不让它参与业务决策——一旦它开始影响「怎么做」，context 就成了隐藏参数，
// 而隐藏参数是会漏的。
//
// 本机（Unix socket）与远程（mTLS）的差别只有一处：本机的身份由 socket 文件模式
// 表达（能连上就等于本地运维），因此 Name 取自请求体里的 createdBy；远程的身份
// 只能由证书证明，因此 Name 取证书的 CN。
type Principal struct {
	Kind PrincipalKind
	// Name 是可信的调用方名字。远程是证书 CN，本机是请求体里自报的 createdBy。
	Name string
	// Scope 是允许的访问档位。本机恒为 ScopeWrite。
	Scope AccessScope
	// Applications 限制这个身份能碰哪些应用（按**应用名**）。为空表示这台机器上的
	// 全部应用。
	Applications []string
}

// PrincipalKind 区分两条进入 opsd 的路径。它同时也是 /identity 里 backend 字段的取值。
type PrincipalKind string

const (
	// PrincipalKindLocal 是 Unix socket 那条路。
	PrincipalKindLocal PrincipalKind = "unix"
	// PrincipalKindRemote 是 mTLS 那条路。
	PrincipalKindRemote PrincipalKind = "tls"
)

// AccessScope 是一条路由要求的访问档位，也是远程身份被授予的档位。
// 只有两个取值：5a 刻意只做「能表达只读与可写之差」这一档，更细的角色是 5c 的事。
type AccessScope string

const (
	// ScopeRead 只能打只读端点。
	ScopeRead AccessScope = "read"
	// ScopeWrite 能打全部端点（含只读端点）。
	ScopeWrite AccessScope = "write"
)

// Allows 判定这个档位能不能打要求 want 的端点。写包含读，读不包含写。
func (s AccessScope) Allows(want AccessScope) bool {
	if s == ScopeWrite {
		return true
	}
	return s == ScopeRead && want == ScopeRead
}

// Valid 判定是不是已知的档位取值。配置文件校验用。
func (s AccessScope) Valid() bool {
	return s == ScopeRead || s == ScopeWrite
}

// LocalPrincipal 是本机请求的身份：能连上 Unix socket 就等于本地运维，
// 因此档位恒为写。名字留空表示「由请求体里的 createdBy 决定」。
func LocalPrincipal() Principal {
	return Principal{Kind: PrincipalKindLocal, Scope: ScopeWrite}
}

// IsRemote 判定这次请求是不是从网络上来的。
func (p Principal) IsRemote() bool { return p.Kind == PrincipalKindRemote }

// AllowsApplication 判定这个身份能不能碰某个应用（按应用名）。
// 空白名单表示「这台机器上的全部应用」。
//
// 名字比较对大小写敏感：应用名是标识符，不做模糊匹配——「生产库里有两个只差大小写
// 的应用」这种状态不该被一个授权判断默默合并掉。
func (p Principal) AllowsApplication(name string) bool {
	if len(p.Applications) == 0 {
		return true
	}
	for _, allowed := range p.Applications {
		if allowed == name {
			return true
		}
	}
	return false
}

// RemoteIdentity 是一条「被允许连进来的远程身份」的规则（迭代 5a）。
//
// 它是配置里 clients 名单的一项，但类型放在 domain 而不是 config：判断
// 「这个 CN 被允许做什么」是授权逻辑，而配置只是它的来源。
type RemoteIdentity struct {
	// CN 是客户端证书的 Common Name。它就是身份本身。
	CN string
	// Scope 是允许的访问档位。
	Scope AccessScope
	// Applications 限制这个身份能碰哪些应用（按应用名）。为空表示全部。
	Applications []string
}

// Principal 把这个身份转成一次请求的调用方身份。
func (r RemoteIdentity) Principal() Principal {
	return Principal{
		Kind:         PrincipalKindRemote,
		Name:         r.CN,
		Scope:        r.Scope,
		Applications: append([]string(nil), r.Applications...),
	}
}

type principalContextKey struct{}

// WithPrincipal 把认证结果放进 context，交给应用层决定「谁做的」。
//
// 之所以走 context 而不是让每个 HTTP 处理器各自改写请求体里的 createdBy：
// 写入口有八九个，漏一个就是一个能被冒充的审计条目；而应用层的创建路径只有一条。
// 放在这里，**新加的写端点自动获得正确行为**。
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

// PrincipalFrom 取出认证结果。没有时返回 (零值, false)——「没认证过」与
// 「认证成空身份」是两件事，调用方必须能区分。
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}

// ClaimedActor 决定这次操作记到谁头上。
//
// 返回两个值：audit 是**可信的**那个（远程一律用证书 CN），claimed 是请求体里
// 自报的那个（只在它与可信值不同、且非空时非空）。自报值不丢，但它进的是审计的
// details，不许替代事实。
func ClaimedActor(ctx context.Context, createdBy string) (actor, claimed string) {
	claimed = strings.TrimSpace(createdBy)
	principal, ok := PrincipalFrom(ctx)
	if !ok || !principal.IsRemote() {
		// 本机：能连上 socket 就等于本地运维，createdBy 与今天一样直接可信。
		return claimed, ""
	}
	// 远程：证书是唯一能自证的东西。名字前缀刻意标出来源，让审计里一眼能看出
	// 这条操作是从网络上来的，而不是某个人在机器上敲的。
	actor = "remote:" + principal.Name
	if claimed != "" && claimed != actor {
		return actor, claimed
	}
	return actor, ""
}

// RemoteForbidden 说清楚「认出来了，但这件事不归你做」。
//
// 与 CodeInvalidRequest 刻意分开：那个码说「你的请求有问题」，这个码说「你的请求没错，
// 是你的身份不允许」。运维看到它要做的事是去改配置里的 clients 名单，而不是改命令。
func RemoteForbidden(format string, args ...any) error {
	return NewError(v1.CodeRemoteForbidden, format, args...)
}

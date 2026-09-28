package httpapi

import (
	"net/http"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/pki"
)

// route 是一条路由。access 是**必填**的：路由表是唯一的注册处，因此「新加一个端点
// 却忘了声明它的访问档位」在启动时就会炸，而不是被默认成某个档位静默放行。
type route struct {
	pattern string
	access  domain.AccessScope
	handler http.HandlerFunc
}

const (
	accessRead  = domain.ScopeRead
	accessWrite = domain.ScopeWrite
)

// RemoteIdentityLookup 是远程身份白名单的查询口。为 nil 表示本进程不听远程——
// 那时任何带客户端证书的请求都不该出现，出现就说明装配与配置对不上。
//
// 定义成接口而不是直接收 config.RemoteConfig：httpapi 不该依赖具体的配置适配器，
// 而这条查询的形状只有一行。
type RemoteIdentityLookup interface {
	FindRemoteIdentity(cn string) (domain.RemoteIdentity, bool)
}

// authenticate 做两件事，顺序不能反：
//
//  1. **认证**：把请求翻译成一个领域身份（Unix socket → 本机，mTLS → 证书 CN）。
//  2. **授权**：先按路由要求的档位判 scope，再按身份的应用白名单判应用。
//
// 它必须在 mux 之后：路由的档位是从**匹配到的 pattern** 查的，而在
// `mux.Handler(r)` 之前根本不知道这次请求落到了哪条路由上。用 mux 自己解析出来的
// pattern（而不是自己再写一遍路径匹配）是刻意的——两套匹配迟早会漂移，而漂移的那
// 一天，报出来的是「明明配了权限却放行了」。
//
// 拒绝一律发生在**动手之前**：任何写操作都还没被创建。
func (s *Server) authenticate(mux *http.ServeMux, access map[string]domain.AccessScope) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := s.principalFor(r)
		if err != nil {
			s.logger().Warn("remote identity rejected",
				"path", r.URL.Path, "clientCn", clientCN(r), "error", domain.MessageOf(err))
			writeError(w, s.logger(), err)
			return
		}

		_, pattern := mux.Handler(r)
		required, ok := access[pattern]
		if !ok {
			if pattern == "" {
				// 没有任何注册的路由匹配上（方法不对时 ServeMux 也会走到这里）。
				// 这不是「没声明档位」，而是「没有这条路由」——放行给 mux 自己去回
				// 404 / 405，别把一次普通的打错路径报成装配错误。
				required = accessRead
			} else {
				// 到不了：Handler() 已经把每条路由的档位登记进表里。真到了这里说明
				// 路由表与 access 表不是一个来源，那是**装配错误**——拒绝，不放行。
				writeError(w, s.logger(), domain.NewError(v1.CodeInternal,
					"路由 %s %s 没有声明访问档位（装配错误）", r.Method, pattern))
				return
			}
		}
		if !principal.Scope.Allows(required) {
			writeError(w, s.logger(), domain.RemoteForbidden(
				"身份 %q 的档位是 %s，不足以访问 %s %s（需要 %s）",
				principalLabel(principal), principal.Scope, r.Method, r.URL.Path, required))
			return
		}
		if err := s.checkApplicationScope(r, pattern, principal); err != nil {
			writeError(w, s.logger(), err)
			return
		}

		next := r.WithContext(domain.WithPrincipal(r.Context(), principal))
		mux.ServeHTTP(w, next)
	})
}

// principalFor 把请求翻译成领域身份。
//
// 判据是 `r.TLS` 而不是「哪个 listener 收的」：Unix socket 上永远没有 TLS，
// mTLS 端口上永远有，因此这个判据与「走的是哪条路」等价，而且它是**请求自己带进来
// 的事实**，不依赖我们记得给每条监听器套对包装。
func (s *Server) principalFor(r *http.Request) (domain.Principal, error) {
	if r.TLS == nil {
		return domain.LocalPrincipal(), nil
	}

	cn := clientCN(r)
	if cn == "" {
		return domain.Principal{}, domain.NewError(v1.CodeHostTLSFailed,
			"客户端证书没有 Common Name：本工具用 CN 认身份，请在签发时填上")
	}
	if s.deps.RemoteIdentities == nil {
		// 装配与配置对不上：能走到 TLS 分支的请求只可能来自远程监听，
		// 而远程监听只有在配了 remote 段时才会开。
		return domain.Principal{}, domain.NewError(v1.CodeInternal,
			"本进程没有装配远程身份白名单，却收到了一个 mTLS 请求（装配错误）")
	}

	identity, ok := s.deps.RemoteIdentities.FindRemoteIdentity(cn)
	if !ok {
		// 默认拒绝。名单里没有就是不放行，不做任何「同前缀」「大小写不敏感」的
		// 近似匹配——授权判断上的近似等于没有判断。
		return domain.Principal{}, domain.RemoteForbidden(
			"客户端证书的 CN %q 不在 remote.clients 名单里", cn)
	}
	return identity.Principal(), nil
}

const (
	// applicationRoutePrefix 下挂着所有「针对某个应用」的端点，它们的 {id} 是应用引用。
	applicationRoutePrefix = "/api/v1/applications/{id}"
	// applicationRouteRoot 是同一批端点在真实路径上的前缀。
	applicationRouteRoot = "/api/v1/applications/"
	// targetsRoutePrefix 下的 {application} **就是应用名**（迭代 5b），不需要再解析。
	// 注册表的键是跨主机一致的名字，而不是本机 applications 表里的那一行。
	targetsRoutePrefix = "/api/v1/targets/{application}"
	targetsRouteRoot   = "/api/v1/targets/"
)

// applicationRef 从请求里取出应用引用，并说明它是不是已经是一个**名字**。
//
// **不能**用 r.PathValue("id")：路径参数是 ServeMux 在派发时填进去的，而中间件跑在
// 它之前。这里曾经就是这么写的，而结果是白名单**一次都没生效过**（取到空串就放行了），
// 只有把「名单外的应用应当被拒」写成断言才看得出来。
//
// 也不能直接拿 pattern 去比前缀：mux.Handler 返回的 pattern **带方法**（例如
// `GET /api/v1/applications/{id}`）。所以先剥掉方法，再按已知形状取真实路径里的那一段。
// pattern 仍然是 mux 自己解析出来的（不是我们猜的），「哪条路由」这一半是准的。
func applicationRef(r *http.Request, pattern string) (ref string, isName bool) {
	path := patternPath(pattern)

	// /api/v1/targets/{application}：路径上那一段就是应用名，不需要查库。
	if strings.HasPrefix(path, targetsRoutePrefix) {
		return pathSegment(r.URL.Path, targetsRouteRoot), true
	}
	if !strings.HasPrefix(path, applicationRoutePrefix) {
		return "", false
	}
	// /api/v1/applications/{id}：这里可能是名字、也可能是不透明 ID，要查一次才知道。
	return pathSegment(r.URL.Path, applicationRouteRoot), false
}

// pathSegment 取 root 之后的第一段路径。取不到时返回空串。
func pathSegment(path, root string) string {
	rest := strings.TrimPrefix(path, root)
	if rest == path {
		return ""
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// patternPath 去掉 pattern 里的方法前缀（`GET /x` → `/x`）。
func patternPath(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		return pattern[i+1:]
	}
	return pattern
}

// checkApplicationScope 判定这个身份能不能碰这次请求指向的应用。
//
// 只有在身份带应用白名单时才查——绝大多数身份（以及本机的所有请求）没有白名单，
// 这里一次数据库读都不做（`/targets/{application}` 那条更是连读都不用）。
//
// 它覆盖不了 `POST /api/v1/operations`：那条路的应用来自请求体里的 resource，
// 在 HTTP 层看不出来。那一半在 application.Service.Create 里判——那是创建操作的
// **唯一**入口，因此没有第二条旁路。`GET /api/v1/targets`（全部应用）同理覆盖不了，
// 它是跨应用的读，与其它跨应用的读一样只受档位约束。
func (s *Server) checkApplicationScope(r *http.Request, pattern string, principal domain.Principal) error {
	if len(principal.Applications) == 0 {
		return nil
	}
	ref, isName := applicationRef(r, pattern)
	if ref == "" {
		return nil
	}
	if isName {
		// 已经是应用名，直接比——不必为了一次授权判断去查一次库。
		if !principal.AllowsApplication(ref) {
			return domain.RemoteForbidden(
				"身份 %q 的白名单里没有应用 %q", principalLabel(principal), ref)
		}
		return nil
	}

	if s.deps.Catalogs == nil {
		return domain.NewError(v1.CodeInternal, "应用目录服务未配置")
	}
	application, err := s.deps.Catalogs.GetApplication(r.Context(), ref)
	if err != nil {
		// 应用不存在时把 404 原样放行给处理器去报，不在这里改写成 403：
		// 「没有这个应用」与「这个应用不归你」是两件事。
		if domain.CodeOf(err) == v1.CodeApplicationNotFound {
			return nil
		}
		return err
	}
	if !principal.AllowsApplication(application.Name) {
		return domain.RemoteForbidden(
			"身份 %q 的白名单里没有应用 %q", principalLabel(principal), application.Name)
	}
	return nil
}

// clientCN 取这次请求里已验证的客户端证书的 CN。没有证书时返回空串。
func clientCN(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	return pki.CommonNameOf(r.TLS.PeerCertificates[0])
}

// principalLabel 是审计与报错里给身份用的短名字。
func principalLabel(principal domain.Principal) string {
	if principal.Name != "" {
		return principal.Name
	}
	return string(principal.Kind)
}

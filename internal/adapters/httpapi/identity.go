package httpapi

import (
	"net/http"
	"os"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// handleIdentity 回答「你是谁、我以什么身份连上来的、这次走的是哪条路」。
//
// 它与 health 刻意分开：health 会去 ping 数据库（「你活着吗」），identity 只描述
// 自己（「我是不是我要找的那台机」）。把两件事混在一起，会让一个在库挂了时仍然
// 该答得出来的问题跟着一起失败。
//
// 它**不回**任何密钥材料：客户端身份只以 CN 的形式出现，服务端身份只以主机名与
// 构建版本的形式出现。证书与私钥绝不进这条响应（迭代 5 规格 D7）。
func (s *Server) handleIdentity(w http.ResponseWriter, r *http.Request) {
	// 中间件一定已经放进来了；取不到时按本机处理——这与「没认证过就是本机」一致。
	principal, _ := domain.PrincipalFrom(r.Context())

	hostname, err := os.Hostname()
	if err != nil {
		// 主机名取不到只影响一个展示字段，不让整条响应失败：host check 的主诉是
		// 「连不连得上、以什么身份」。
		s.logger().Warn("cannot read hostname for /identity", "error", err)
	}

	response := v1.IdentityResponse{
		APIVersion:    v1.APIVersion,
		Hostname:      hostname,
		DaemonVersion: s.deps.Version,
		StartedAt:     s.startedAt,
		Backend:       string(principal.Kind),
		ClientCn:      principal.Name,
		Scope:         string(principal.Scope),
		// 空数组而不是 null：「这台机上的全部应用」是确定的语义，
		// 用 null 表达会让调用方以为「没有信息」。
		ApplicationsAllowed: append([]string{}, principal.Applications...),
	}

	if s.deps.Catalogs != nil {
		applications, err := s.deps.Catalogs.ListApplications(r.Context())
		if err != nil {
			// 只记一条日志并把 applications 留成 null：身份本身的答案不受影响，
			// 而「库读不出来」这件事不该被伪装成「0 个应用」。
			s.logger().Warn("cannot list applications for /identity", "error", err)
		} else {
			count := len(applications)
			response.Applications = &count
		}
	}

	writeJSON(w, http.StatusOK, response)
}

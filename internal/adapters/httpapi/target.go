package httpapi

import (
	"net/http"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

func (s *Server) handleSetTargets(w http.ResponseWriter, r *http.Request) {
	if s.deps.Targets == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "部署目标服务未配置"))
		return
	}
	var req v1.SetTargetsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, s.logger(), err)
		return
	}

	application := r.PathValue("application")
	if _, err := s.deps.Targets.SetTargets(r.Context(), application, req.Hosts); err != nil {
		writeError(w, s.logger(), err)
		return
	}
	targets, err := s.targetsDTO(r, application)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.TargetsResponse{APIVersion: v1.APIVersion, Targets: targets})
}

func (s *Server) handleGetTargets(w http.ResponseWriter, r *http.Request) {
	if s.deps.Targets == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "部署目标服务未配置"))
		return
	}
	targets, err := s.targetsDTO(r, r.PathValue("application"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.TargetsResponse{APIVersion: v1.APIVersion, Targets: targets})
}

func (s *Server) handleListTargets(w http.ResponseWriter, r *http.Request) {
	if s.deps.Targets == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "部署目标服务未配置"))
		return
	}
	all, err := s.deps.Targets.AllTargets(r.Context())
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}

	items := make([]v1.ApplicationTargets, 0, len(all))
	for i := range all {
		hosts, err := s.resolveHosts(r, all[i].Hosts)
		if err != nil {
			writeError(w, s.logger(), err)
			return
		}
		items = append(items, v1.ApplicationTargets{Application: all[i].Application, Hosts: hosts})
	}
	writeJSON(w, http.StatusOK, v1.TargetsListResponse{APIVersion: v1.APIVersion, Items: items})
}

// targetsDTO 组装一个应用的部署目标，并把主机名解析成地址。
//
// 地址在**读的时候**解析，不在写的时候存下来：主机记录的地址改了（换了机器、
// 换了端口），注册表不该还拿着旧的。这与 5a 的 `--host` 从库里现查地址是同一条。
func (s *Server) targetsDTO(r *http.Request, application string) (v1.ApplicationTargets, error) {
	names, err := s.deps.Targets.Targets(r.Context(), application)
	if err != nil {
		return v1.ApplicationTargets{}, err
	}
	hosts, err := s.resolveHosts(r, names)
	if err != nil {
		return v1.ApplicationTargets{}, err
	}
	return v1.ApplicationTargets{Application: application, Hosts: hosts}, nil
}

func (s *Server) resolveHosts(r *http.Request, names []string) ([]v1.TargetHost, error) {
	hosts := make([]v1.TargetHost, 0, len(names))
	for _, name := range names {
		resolved, err := s.deps.Hosts.GetHost(r.Context(), name)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, v1.TargetHost{Name: resolved.Name, Address: resolved.Address})
	}
	return hosts, nil
}

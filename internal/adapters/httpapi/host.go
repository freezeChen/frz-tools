package httpapi

import (
	"net/http"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

func (s *Server) handleListHosts(w http.ResponseWriter, r *http.Request) {
	if s.deps.Hosts == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "host service is not configured"))
		return
	}

	hosts, err := s.deps.Hosts.ListHosts(r.Context())
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}

	items := make([]v1.Host, 0, len(hosts))
	for i := range hosts {
		items = append(items, hostDTO(&hosts[i]))
	}
	writeJSON(w, http.StatusOK, v1.HostListResponse{APIVersion: v1.APIVersion, Items: items})
}

func (s *Server) handleCreateHost(w http.ResponseWriter, r *http.Request) {
	if s.deps.Hosts == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "host service is not configured"))
		return
	}

	var req v1.CreateHostRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, s.logger(), err)
		return
	}

	host, err := s.deps.Hosts.CreateHost(r.Context(), req.Name, req.Address, req.Labels)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusCreated, v1.HostResponse{APIVersion: v1.APIVersion, Host: hostDTO(host)})
}

func (s *Server) handleGetHost(w http.ResponseWriter, r *http.Request) {
	if s.deps.Hosts == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "host service is not configured"))
		return
	}

	// 引用同时接受不透明 ID 与名称，便于 CLI 直接用名称查。
	host, err := s.deps.Hosts.GetHost(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.HostResponse{APIVersion: v1.APIVersion, Host: hostDTO(host)})
}

func (s *Server) handleListEnvironments(w http.ResponseWriter, r *http.Request) {
	if s.deps.Hosts == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "host service is not configured"))
		return
	}

	environments, err := s.deps.Hosts.ListEnvironments(r.Context())
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}

	items := make([]v1.Environment, 0, len(environments))
	for i := range environments {
		items = append(items, environmentDTO(&environments[i]))
	}
	writeJSON(w, http.StatusOK, v1.EnvironmentListResponse{APIVersion: v1.APIVersion, Items: items})
}

func (s *Server) handleCreateEnvironment(w http.ResponseWriter, r *http.Request) {
	if s.deps.Hosts == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "host service is not configured"))
		return
	}

	var req v1.CreateEnvironmentRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, s.logger(), err)
		return
	}

	environment, err := s.deps.Hosts.CreateEnvironment(r.Context(), req.Name, req.Labels)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusCreated, v1.EnvironmentResponse{
		APIVersion:  v1.APIVersion,
		Environment: environmentDTO(environment),
	})
}

func (s *Server) handleGetEnvironment(w http.ResponseWriter, r *http.Request) {
	if s.deps.Hosts == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "host service is not configured"))
		return
	}

	environment, err := s.deps.Hosts.GetEnvironment(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.EnvironmentResponse{
		APIVersion:  v1.APIVersion,
		Environment: environmentDTO(environment),
	})
}

func hostDTO(host *domain.Host) v1.Host {
	return v1.Host{
		ID:        host.ID,
		Name:      host.Name,
		Address:   host.Address,
		Labels:    host.Labels,
		CreatedAt: host.CreatedAt,
		UpdatedAt: host.UpdatedAt,
	}
}

func environmentDTO(environment *domain.Environment) v1.Environment {
	return v1.Environment{
		ID:        environment.ID,
		Name:      environment.Name,
		Labels:    environment.Labels,
		CreatedAt: environment.CreatedAt,
		UpdatedAt: environment.UpdatedAt,
	}
}

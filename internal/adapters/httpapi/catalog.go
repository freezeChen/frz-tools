package httpapi

import (
	"net/http"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

func (s *Server) handleCreateApplication(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalogs == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "catalog service is not configured"))
		return
	}

	var req v1.CreateApplicationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, s.logger(), err)
		return
	}
	app, err := s.deps.Catalogs.CreateApplication(r.Context(), req.Name, req.Labels)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusCreated, v1.ApplicationResponse{
		APIVersion:  v1.APIVersion,
		Application: applicationDTO(app),
	})
}

func (s *Server) handleListApplications(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalogs == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "catalog service is not configured"))
		return
	}

	apps, err := s.deps.Catalogs.ListApplications(r.Context())
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	items := make([]v1.Application, 0, len(apps))
	for i := range apps {
		items = append(items, applicationDTO(&apps[i]))
	}
	writeJSON(w, http.StatusOK, v1.ApplicationListResponse{APIVersion: v1.APIVersion, Items: items})
}

func (s *Server) handleGetApplication(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalogs == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "catalog service is not configured"))
		return
	}

	app, err := s.deps.Catalogs.GetApplication(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.ApplicationResponse{
		APIVersion:  v1.APIVersion,
		Application: applicationDTO(app),
	})
}

func (s *Server) handleListApplicationReleases(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalogs == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "catalog service is not configured"))
		return
	}

	limit, err := intQuery(r, "limit", 0)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	releases, err := s.deps.Catalogs.ListReleases(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.ReleaseListResponse{
		APIVersion: v1.APIVersion,
		Items:      releaseItems(releases),
	})
}

func (s *Server) handleCreateRelease(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalogs == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "catalog service is not configured"))
		return
	}

	var req v1.CreateReleaseRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, s.logger(), err)
		return
	}
	release, err := s.deps.Catalogs.CreateRelease(r.Context(),
		req.Application, req.Artifact, req.Version, req.Labels, req.CreatedBy)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusCreated, v1.ReleaseResponse{
		APIVersion: v1.APIVersion,
		Release:    releaseDTO(release),
	})
}

func (s *Server) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalogs == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "catalog service is not configured"))
		return
	}

	release, err := s.deps.Catalogs.GetRelease(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.ReleaseResponse{
		APIVersion: v1.APIVersion,
		Release:    releaseDTO(release),
	})
}

func applicationDTO(app *domain.Application) v1.Application {
	return v1.Application{
		ID:        app.ID,
		Name:      app.Name,
		Labels:    app.Labels,
		CreatedAt: app.CreatedAt,
		UpdatedAt: app.UpdatedAt,
	}
}

func releaseDTO(release *domain.Release) v1.Release {
	return v1.Release{
		ID:            release.ID,
		ApplicationID: release.ApplicationID,
		ArtifactID:    release.ArtifactID,
		Version:       release.Version,
		Labels:        release.Labels,
		CreatedAt:     release.CreatedAt,
		CreatedBy:     release.CreatedBy,
		Status:        string(release.Status),
		Directory:     release.Directory,
		ActivatedAt:   release.ActivatedAt,
		FinishedAt:    release.FinishedAt,
		ErrorCode:     release.ErrorCode,
		ErrorMessage:  release.ErrorMessage,
	}
}

func releaseItems(releases []domain.Release) []v1.Release {
	items := make([]v1.Release, 0, len(releases))
	for i := range releases {
		items = append(items, releaseDTO(&releases[i]))
	}
	return items
}

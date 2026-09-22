package httpapi

import (
	"net/http"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/application"
	"frz-tools/internal/domain"
)

const (
	headerArtifactName      = "X-Artifact-Name"
	headerArtifactMediaType = "X-Artifact-Media-Type"
	headerArtifactDigest    = "X-Artifact-SHA256"
)

func (s *Server) handleUploadArtifact(w http.ResponseWriter, r *http.Request) {
	if s.deps.Artifacts == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "artifact service is not configured"))
		return
	}

	declared := domain.Digest("")
	if raw := r.Header.Get(headerArtifactDigest); raw != "" {
		parsed, err := domain.ParseDigest(raw)
		if err != nil {
			writeError(w, s.logger(), err)
			return
		}
		declared = parsed
	}

	artifact, created, err := s.deps.Artifacts.Put(r.Context(), application.PutArtifactInput{
		Name:      r.Header.Get(headerArtifactName),
		MediaType: r.Header.Get(headerArtifactMediaType),
		Digest:    declared,
		Body:      r.Body,
		CreatedBy: r.Header.Get("X-Requested-By"),
	})
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, v1.ArtifactResponse{APIVersion: v1.APIVersion, Artifact: artifactDTO(artifact)})
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	if s.deps.Artifacts == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "artifact service is not configured"))
		return
	}

	limit, err := intQuery(r, "limit", 0)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	artifacts, err := s.deps.Artifacts.List(r.Context(), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}

	items := make([]v1.Artifact, 0, len(artifacts))
	for i := range artifacts {
		items = append(items, artifactDTO(&artifacts[i]))
	}
	response := v1.ArtifactListResponse{APIVersion: v1.APIVersion, Items: items}
	if len(items) > 0 {
		response.NextCursor = items[len(items)-1].ID
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	if s.deps.Artifacts == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "artifact service is not configured"))
		return
	}

	artifact, err := s.deps.Artifacts.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.ArtifactResponse{APIVersion: v1.APIVersion, Artifact: artifactDTO(artifact)})
}

func (s *Server) handleVerifyArtifact(w http.ResponseWriter, r *http.Request) {
	if s.deps.Artifacts == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "artifact service is not configured"))
		return
	}

	artifact, err := s.deps.Artifacts.Verify(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.VerifyArtifactResponse{
		APIVersion: v1.APIVersion,
		ArtifactID: artifact.ID,
		Digest:     artifact.Digest.String(),
		Verified:   true,
	})
}

func (s *Server) handleDeleteArtifact(w http.ResponseWriter, r *http.Request) {
	if s.deps.Artifacts == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "artifact service is not configured"))
		return
	}

	if err := s.deps.Artifacts.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, s.logger(), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type collectRequest struct {
	KeepLast    int   `json:"keepLast"`
	OlderThanMs int64 `json:"olderThanMs"`
	DryRun      bool  `json:"dryRun"`
}

func (s *Server) handleCollectArtifacts(w http.ResponseWriter, r *http.Request) {
	if s.deps.Artifacts == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "artifact service is not configured"))
		return
	}

	var req collectRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, s.logger(), err)
			return
		}
	}
	if req.KeepLast < 0 || req.OlderThanMs < 0 {
		writeError(w, s.logger(), domain.NewError(v1.CodeInvalidRequest, "keepLast and olderThanMs must not be negative"))
		return
	}

	result, err := s.deps.Artifacts.Collect(r.Context(), req.KeepLast, time.Duration(req.OlderThanMs)*time.Millisecond, req.DryRun)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.CollectArtifactsResponse{
		APIVersion: v1.APIVersion,
		DryRun:     result.DryRun,
		Removed:    emptyIfNil(result.Removed),
		Orphans:    emptyIfNil(result.Orphans),
		Kept:       result.Kept,
		FreedBytes: result.FreedBytes,
	})
}

func artifactDTO(artifact *domain.Artifact) v1.Artifact {
	return v1.Artifact{
		ID:        artifact.ID,
		Digest:    artifact.Digest.String(),
		Size:      artifact.Size,
		MediaType: artifact.MediaType,
		Name:      artifact.Name,
		CreatedAt: artifact.CreatedAt,
		CreatedBy: artifact.CreatedBy,
	}
}

func emptyIfNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

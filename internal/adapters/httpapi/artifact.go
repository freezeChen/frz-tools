package httpapi

import (
	"io"
	"net/http"
	"strconv"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
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

// handleDownloadArtifact 原样输出制品内容。字节一致性由
// ArtifactService.Download 保证：它先按记录重算摘要，确认一致之后才打开流，
// 因此摘要不匹配时返回的是 JSON 错误信封，而不是一份看起来成功的坏内容。
func (s *Server) handleDownloadArtifact(w http.ResponseWriter, r *http.Request) {
	if s.deps.Artifacts == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "artifact service is not configured"))
		return
	}

	artifact, content, err := s.deps.Artifacts.Download(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	defer content.Close()

	header := w.Header()
	header.Set("Content-Type", artifactContentType(artifact))
	header.Set("Content-Length", strconv.FormatInt(artifact.Size, 10))
	// ETag 用内容摘要：它与内容一一对应，客户端可据此核对下载结果的完整性。
	header.Set("ETag", `"`+artifact.Digest.String()+`"`)
	w.WriteHeader(http.StatusOK)

	written, err := io.Copy(w, content)
	if err != nil {
		// 响应头已经发出，无法再改状态码，只能记录：客户端会因为字节数不足而
		// 在自己的摘要校验里失败，不会被当成成功。
		s.logger().Error("artifact download interrupted", "artifactId", artifact.ID, "error", err)
		return
	}
	if written != artifact.Size {
		s.logger().Error("artifact download size mismatch",
			"artifactId", artifact.ID, "want", artifact.Size, "got", written)
	}
}

// artifactContentType 在制品没有记录 mediaType 时给出通用二进制类型，
// 避免让客户端拿到空的 Content-Type。
func artifactContentType(artifact *domain.Artifact) string {
	if artifact.MediaType != "" {
		return artifact.MediaType
	}
	return "application/octet-stream"
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

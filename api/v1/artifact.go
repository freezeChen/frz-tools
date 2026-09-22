package v1

import "time"

type Artifact struct {
	ID        string    `json:"id"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	MediaType string    `json:"mediaType,omitempty"`
	Name      string    `json:"name,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	CreatedBy string    `json:"createdBy,omitempty"`
}

type ArtifactResponse struct {
	APIVersion string   `json:"apiVersion"`
	Artifact   Artifact `json:"artifact"`
}

type ArtifactListResponse struct {
	APIVersion string     `json:"apiVersion"`
	Items      []Artifact `json:"items"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

type VerifyArtifactResponse struct {
	APIVersion string `json:"apiVersion"`
	ArtifactID string `json:"artifactId"`
	Digest     string `json:"digest"`
	Verified   bool   `json:"verified"`
}

type CollectArtifactsResponse struct {
	APIVersion string   `json:"apiVersion"`
	DryRun     bool     `json:"dryRun"`
	Removed    []string `json:"removed"`
	Orphans    []string `json:"orphans"`
	Kept       int      `json:"kept"`
	FreedBytes int64    `json:"freedBytes"`
}

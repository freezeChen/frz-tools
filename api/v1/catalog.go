package v1

import "time"

type CreateApplicationRequest struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

type Application struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

type ApplicationResponse struct {
	APIVersion  string      `json:"apiVersion"`
	Application Application `json:"application"`
}

type ApplicationListResponse struct {
	APIVersion string        `json:"apiVersion"`
	Items      []Application `json:"items"`
}

type CreateReleaseRequest struct {
	Application string            `json:"application"`
	Artifact    string            `json:"artifact"`
	Version     string            `json:"version"`
	Labels      map[string]string `json:"labels,omitempty"`
	CreatedBy   string            `json:"createdBy,omitempty"`
}

type Release struct {
	ID            string            `json:"id"`
	ApplicationID string            `json:"applicationId"`
	ArtifactID    string            `json:"artifactId"`
	Version       string            `json:"version"`
	Labels        map[string]string `json:"labels,omitempty"`
	CreatedAt     time.Time         `json:"createdAt"`
	CreatedBy     string            `json:"createdBy,omitempty"`

	// 迭代 3 起，release 是一次**部署**而不只是一条登记记录。
	Status       string     `json:"status"`
	Directory    string     `json:"directory,omitempty"`
	ActivatedAt  *time.Time `json:"activatedAt,omitempty"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
	ErrorCode    string     `json:"errorCode,omitempty"`
	ErrorMessage string     `json:"errorMessage,omitempty"`
}

type ReleaseResponse struct {
	APIVersion string  `json:"apiVersion"`
	Release    Release `json:"release"`
}

type ReleaseListResponse struct {
	APIVersion string    `json:"apiVersion"`
	Items      []Release `json:"items"`
}

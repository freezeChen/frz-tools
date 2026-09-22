package domain

import (
	"strings"
	"time"

	v1 "frz-tools/api/v1"
)

type Application struct {
	ID        string
	Name      string
	Labels    map[string]string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (a *Application) Validate() error {
	if strings.TrimSpace(a.Name) == "" {
		return NewError(v1.CodeInvalidRequest, "application name is required")
	}
	return nil
}

// Release 在 1a 中只是「某个应用采用了某个制品」的记录，没有 slot、没有 active
// 标记、没有部署状态；这些要等迭代 3/4 有真实语义时再加。
type Release struct {
	ID            string
	ApplicationID string
	ArtifactID    string
	Version       string
	Labels        map[string]string
	CreatedAt     time.Time
	CreatedBy     string
}

func (r *Release) Validate() error {
	if strings.TrimSpace(r.ApplicationID) == "" {
		return NewError(v1.CodeInvalidRequest, "release.applicationId is required")
	}
	if strings.TrimSpace(r.ArtifactID) == "" {
		return NewError(v1.CodeInvalidRequest, "release.artifactId is required")
	}
	if strings.TrimSpace(r.Version) == "" {
		return NewError(v1.CodeInvalidRequest, "release.version is required")
	}
	return nil
}

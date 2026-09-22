package application

import (
	"context"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

const defaultCatalogLimit = 100
const maxCatalogLimit = 1000

const appIDPrefix = "app"
const releaseIDPrefix = "rel"

// CatalogService 承载 Application 与 Release 的用例。1a 中 Release 只是「某个应用
// 采用了某个制品」的登记，没有部署、槽位或激活语义。
type CatalogService struct {
	repo  Repository
	newID func(prefix string) string
	now   func() time.Time
}

func newCatalogService(repo Repository, newID func(prefix string) string, now func() time.Time) *CatalogService {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &CatalogService{repo: repo, newID: newID, now: now}
}

func (s *CatalogService) CreateApplication(ctx context.Context, name string, labels map[string]string) (*domain.Application, error) {
	now := s.now()
	app := &domain.Application{
		ID:        s.newID(appIDPrefix),
		Name:      strings.TrimSpace(name),
		Labels:    labels,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.CreateApplication(ctx, app); err != nil {
		return nil, err
	}
	return app, nil
}

func (s *CatalogService) GetApplication(ctx context.Context, ref string) (*domain.Application, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, domain.NewError(v1.CodeInvalidRequest, "application reference is required")
	}
	return s.repo.GetApplication(ctx, ref)
}

func (s *CatalogService) ListApplications(ctx context.Context) ([]domain.Application, error) {
	return s.repo.ListApplications(ctx)
}

// CreateRelease 把制品绑定到应用并登记版本号。引用的对象必须真实存在，否则在
// 写入前就以明确的错误码失败，而不是让数据库外键报出难以理解的约束错误。
func (s *CatalogService) CreateRelease(ctx context.Context, appRef, artifactRef, version string, labels map[string]string, createdBy string) (*domain.Release, error) {
	app, err := s.GetApplication(ctx, appRef)
	if err != nil {
		return nil, err
	}
	artifact, err := s.repo.GetArtifact(ctx, artifactRef)
	if err != nil {
		return nil, err
	}

	release := &domain.Release{
		ID:            s.newID(releaseIDPrefix),
		ApplicationID: app.ID,
		ArtifactID:    artifact.ID,
		Version:       strings.TrimSpace(version),
		Labels:        labels,
		CreatedAt:     s.now(),
		CreatedBy:     createdBy,
	}
	return s.repo.CreateRelease(ctx, release)
}

func (s *CatalogService) GetRelease(ctx context.Context, id string) (*domain.Release, error) {
	if strings.TrimSpace(id) == "" {
		return nil, domain.NewError(v1.CodeInvalidRequest, "release id is required")
	}
	return s.repo.GetRelease(ctx, id)
}

func (s *CatalogService) ListReleases(ctx context.Context, appRef string, limit int) ([]domain.Release, error) {
	app, err := s.GetApplication(ctx, appRef)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxCatalogLimit {
		limit = defaultCatalogLimit
	}
	return s.repo.ListReleases(ctx, app.ID, limit)
}

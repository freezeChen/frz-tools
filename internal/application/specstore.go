package application

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// SpecService 承载「应用当前规格」的用例：一个应用一份 manifest，提交即覆盖。
//
// 严格解码（KnownFields）与字段校验属于 manifest 包与 domain.ApplicationSpec，
// 这一层不重复实现；它只负责把目标应用解析成 ID、序列化并落库。
type SpecService struct {
	repo Repository
	now  func() time.Time
}

func newSpecService(repo Repository, now func() time.Time) *SpecService {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &SpecService{repo: repo, now: now}
}

// PutSpec 提交应用的当前规格。ref 可以是应用 ID 或名称；解析不到时返回
// APPLICATION_NOT_FOUND，而不是让调用方拿着不存在的 ID 去撞数据库外键。
func (s *SpecService) PutSpec(ctx context.Context, ref string, spec *domain.ApplicationSpec, updatedBy string) (*domain.ApplicationSpec, error) {
	if spec == nil {
		return nil, domain.NewError(v1.CodeManifestInvalid, "manifest 不能为空")
	}
	app, err := s.resolveApplication(ctx, ref)
	if err != nil {
		return nil, err
	}

	// 这一层是落库前的最后一道：规格可能来自内部调用而不经过 manifest 的解码，
	// 这里漏掉校验就会让非法规格静默进库，等到准备/启动时才炸。
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, domain.NewError(v1.CodeInternal, "序列化 manifest 失败: %v", err)
	}
	if err := s.repo.PutApplicationSpec(ctx, app.ID, encoded, s.now(), updatedBy); err != nil {
		return nil, err
	}
	return spec, nil
}

// GetSpec 取回应用的当前规格；尚未提交 manifest 时返回 SPEC_NOT_FOUND。
func (s *SpecService) GetSpec(ctx context.Context, ref string) (*domain.ApplicationSpec, error) {
	app, err := s.resolveApplication(ctx, ref)
	if err != nil {
		return nil, err
	}
	return s.repo.GetApplicationSpec(ctx, app.ID)
}

func (s *SpecService) resolveApplication(ctx context.Context, ref string) (*domain.Application, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, domain.NewError(v1.CodeInvalidRequest, "application reference is required")
	}
	return s.repo.GetApplication(ctx, ref)
}

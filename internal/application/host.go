package application

import (
	"context"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

const hostIDPrefix = "host"
const environmentIDPrefix = "env"

// localHostName 是本机自举记录的固定名字。本机记录以「address 为空」为语义，
// 名字只是给人看的标签，因此固定成这个名字，让 `opsctl host list` 一眼能认出来。
const localHostName = "local"

// HostService 承载 Host 与 Environment 的用例。1c 里两者只建身份与标签，
// 不承载连接语义，也不与应用建立关联（迭代 3/5 再做）。
type HostService struct {
	repo  Repository
	newID func(prefix string) string
	now   func() time.Time
}

func newHostService(repo Repository, newID func(prefix string) string, now func() time.Time) *HostService {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &HostService{repo: repo, newID: newID, now: now}
}

// EnsureLocalHost 保证存在一条 address 为空的本机 Host 记录。opsd 每次启动都会
// 调用它，因此必须先查后建、并且不改动已有记录：名字与标签可能已经被运维调整过，
// 覆盖它们就等于每次重启都悄悄回滚用户的修改。
//
// 名字冲突不能让守护进程起不来：字段表里的 name 有唯一约束，若它已被一台远程主机
// 占用，就换一个由 ID 派生的名字落库——本机的身份由 address 表达，不依赖名字。
func (s *HostService) EnsureLocalHost(ctx context.Context) (*domain.Host, error) {
	existing, err := s.repo.FindLocalHost(ctx)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	now := s.now()
	host := &domain.Host{
		ID:        s.newID(hostIDPrefix),
		Name:      localHostName,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if taken, err := s.hostNameTaken(ctx, localHostName); err != nil {
		return nil, err
	} else if taken {
		// localHostName 被另一台已在册的主机占用（例如它是一台远程主机）。
		// 本机的身份由 address 表达，不依赖名字，因此换一个由 ID 派生的名字，
		// 而不是让 opsd 因为一个标签冲突起不来。
		host.Name = localHostName + "-" + host.ID
	}
	if err := s.repo.CreateHost(ctx, host); err != nil {
		return nil, err
	}
	return host, nil
}

func (s *HostService) hostNameTaken(ctx context.Context, name string) (bool, error) {
	_, err := s.repo.GetHost(ctx, name)
	switch {
	case err == nil:
		return true, nil
	case domain.CodeOf(err) == v1.CodeHostNotFound:
		return false, nil
	default:
		return false, err
	}
}

func (s *HostService) CreateHost(ctx context.Context, name, address string, labels map[string]string) (*domain.Host, error) {
	now := s.now()
	host := &domain.Host{
		ID:        s.newID(hostIDPrefix),
		Name:      strings.TrimSpace(name),
		Address:   strings.TrimSpace(address),
		Labels:    labels,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.CreateHost(ctx, host); err != nil {
		return nil, err
	}
	return host, nil
}

func (s *HostService) GetHost(ctx context.Context, ref string) (*domain.Host, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, domain.NewError(v1.CodeInvalidRequest, "host reference is required")
	}
	return s.repo.GetHost(ctx, ref)
}

func (s *HostService) ListHosts(ctx context.Context) ([]domain.Host, error) {
	return s.repo.ListHosts(ctx)
}

func (s *HostService) CreateEnvironment(ctx context.Context, name string, labels map[string]string) (*domain.Environment, error) {
	now := s.now()
	environment := &domain.Environment{
		ID:        s.newID(environmentIDPrefix),
		Name:      strings.TrimSpace(name),
		Labels:    labels,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.CreateEnvironment(ctx, environment); err != nil {
		return nil, err
	}
	return environment, nil
}

func (s *HostService) GetEnvironment(ctx context.Context, ref string) (*domain.Environment, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, domain.NewError(v1.CodeInvalidRequest, "environment reference is required")
	}
	return s.repo.GetEnvironment(ctx, ref)
}

func (s *HostService) ListEnvironments(ctx context.Context) ([]domain.Environment, error) {
	return s.repo.ListEnvironments(ctx)
}

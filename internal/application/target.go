package application

import (
	"context"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// TargetService 承载**部署目标**的用例：一个应用应该跑在哪些主机上（迭代 5b）。
//
// 它住在「与 opsctl 说话的那台机」上，而不是每台目标机上。一台机上的注册表说明了
// 从这个控制点看出去的部署范围；注册表本身不参与部署（部署仍然是每台机各做各的）。
type TargetService struct {
	repo Repository
	now  func() time.Time
}

func newTargetService(repo Repository, now func() time.Time) *TargetService {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &TargetService{repo: repo, now: now}
}

// SetTargets 替换某个应用的部署目标列表。
//
// 主机名必须在本机的主机表里：打错一个字母的后果不该等到部署那天才显形，而那时它
// 表现为「少发了一台」，看起来像是「这台本来就不该发」。
func (s *TargetService) SetTargets(ctx context.Context, application string, hosts []string) ([]string, error) {
	if err := domain.ValidateApplicationName(application); err != nil {
		return nil, err
	}
	normalized := domain.NormalizeTargets(hosts)
	if len(normalized) == 0 {
		// 空列表刻意拒绝，因此也没有「清空登记」这个动作（记在规格的「不做」里）：
		// 一个「应该跑在零台机上」的应用没有意义，而把「我忘了写 --host」读成
		// 「清空」是一类会静默改变发布范围的错。
		return nil, domain.NewError(v1.CodeInvalidRequest,
			"部署目标不能为空：至少要给一个 --host（清空整个登记在 5b 不做）")
	}
	if err := s.repo.ReplaceApplicationTargets(ctx, application, normalized, s.now()); err != nil {
		return nil, err
	}
	return normalized, nil
}

// Targets 读回某个应用的部署目标（按主机名）。
func (s *TargetService) Targets(ctx context.Context, application string) ([]string, error) {
	if err := domain.ValidateApplicationName(application); err != nil {
		return nil, err
	}
	return s.repo.ListApplicationTargets(ctx, application)
}

// AllTargets 列出所有应用的部署目标——跨主机的应用清单汇总。
func (s *TargetService) AllTargets(ctx context.Context) ([]domain.ApplicationTargets, error) {
	return s.repo.ListAllApplicationTargets(ctx)
}

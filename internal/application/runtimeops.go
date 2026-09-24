package application

import (
	"context"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// RuntimeService 承载「应用的当前规格 → 运行时动作」的用例：解析规格、确认适配器可用、
// 把动作交给 RuntimeAdapter，并提供 sync 端点与 Operation 创建共用的前置校验。
//
// 1c 只有 systemd 一个真实适配器，且装配层在非 Linux 上根本不注入它：因此
// 「本机没有可用的运行时适配器」是一种正常部署形态，必须给出 RUNTIME_UNSUPPORTED，
// 而不是 panic 或 500。
type RuntimeService struct {
	repo     Repository
	adapter  RuntimeAdapter
	reporter RuntimePrepareReporter
}

func newRuntimeService(repo Repository, adapter RuntimeAdapter, reporter RuntimePrepareReporter) *RuntimeService {
	return &RuntimeService{repo: repo, adapter: adapter, reporter: reporter}
}

// Resolve 取回应用与它的当前规格。错误码由仓储给出，这里不重新拼：
// 应用不存在 → APPLICATION_NOT_FOUND，未登记 manifest → SPEC_NOT_FOUND（§11）。
func (s *RuntimeService) Resolve(ctx context.Context, appRef string) (*domain.Application, *domain.ApplicationSpec, error) {
	app, err := s.repo.GetApplication(ctx, appRef)
	if err != nil {
		return nil, nil, err
	}
	spec, err := s.repo.GetApplicationSpec(ctx, app.ID)
	if err != nil {
		return nil, nil, err
	}
	return app, spec, nil
}

// Validate 只检查规格能否被本适配器执行，不产生任何副作用。
func (s *RuntimeService) Validate(ctx context.Context, appRef string) (*domain.Application, *domain.ApplicationSpec, error) {
	app, spec, err := s.Resolve(ctx, appRef)
	if err != nil {
		return nil, nil, err
	}
	adapter, err := s.requireAdapter()
	if err != nil {
		return nil, nil, err
	}
	if err := adapter.Validate(ctx, spec); err != nil {
		return nil, nil, err
	}
	return app, spec, nil
}

// Prepare 执行一次幂等的准备，并返回适配器做出的决策。决策未知时 ok=false：
// 调用方据此省略字段，而不是把零值当档位展示。
func (s *RuntimeService) Prepare(ctx context.Context, appRef string) (*domain.Application, *domain.ApplicationSpec, RuntimeDecision, bool, error) {
	app, spec, err := s.Resolve(ctx, appRef)
	if err != nil {
		return nil, nil, RuntimeDecision{}, false, err
	}
	adapter, err := s.requireAdapter()
	if err != nil {
		return nil, nil, RuntimeDecision{}, false, err
	}
	// 适配器契约要求 Prepare 内部先自校验，因此这里不重复 Validate。
	if err := adapter.Prepare(ctx, spec); err != nil {
		return nil, nil, RuntimeDecision{}, false, err
	}
	decision, ok := s.decision(ctx, spec)
	return app, spec, decision, ok, nil
}

// Health 返回就绪快照。未就绪本身不是错误，由 Ready=false 表达（调用方据此轮询）；
// 确定性失败（unit failed、启动超时）由适配器返回 RUNTIME_NOT_READY 硬错误。
func (s *RuntimeService) Health(ctx context.Context, appRef string) (*domain.Application, *domain.ApplicationSpec, domain.RuntimeHealth, error) {
	app, spec, err := s.Resolve(ctx, appRef)
	if err != nil {
		return nil, nil, domain.RuntimeHealth{}, err
	}
	adapter, err := s.requireAdapter()
	if err != nil {
		return nil, nil, domain.RuntimeHealth{}, err
	}
	health, err := adapter.Health(ctx, spec)
	if err != nil {
		return nil, nil, domain.RuntimeHealth{}, err
	}
	return app, spec, health, nil
}

// ResolveRuntimeOperation 是创建侧的前置校验：应用存在、manifest 已登记、适配器可用
// 且能执行这份规格，最后把资源规范成应用 ID。规范化是必要的——同一个应用无论用名称
// 还是 ID 提交 runtime.start，都必须命中同一把资源锁。
func (s *RuntimeService) ResolveRuntimeOperation(ctx context.Context, kind, appRef string) (string, error) {
	if kind != v1.KindRuntimeStart && kind != v1.KindRuntimeStop {
		return "", domain.NewError(v1.CodeInvalidRequest,
			"运行时操作只支持 %s 与 %s（got %q）", v1.KindRuntimeStart, v1.KindRuntimeStop, kind)
	}
	app, _, err := s.Validate(ctx, appRef)
	if err != nil {
		return "", err
	}
	return app.ID, nil
}

// adapter 供同包的 worker 使用：执行侧与创建侧必须用同一个适配器实例，
// 否则 systemd 的版本探测与就绪计数会在两个实例之间各记一份。
func (s *RuntimeService) requireAdapter() (RuntimeAdapter, error) {
	if s.adapter == nil {
		return nil, errRuntimeUnsupported()
	}
	return s.adapter, nil
}

func (s *RuntimeService) decision(ctx context.Context, spec *domain.ApplicationSpec) (RuntimeDecision, bool) {
	if s.reporter == nil {
		return RuntimeDecision{}, false
	}
	return s.reporter.ReportRuntimePrepare(ctx, spec)
}

// errRuntimeUnsupported 用同一句话描述「没有可用适配器」，让 HTTP 与 worker 两条
// 路径给出可对照的错误。
func errRuntimeUnsupported() error {
	return domain.NewError(v1.CodeRuntimeUnsupport,
		"本机没有可用的运行时适配器：runtime.* 需要 Linux 上的 systemd 适配器，当前部署未装配")
}

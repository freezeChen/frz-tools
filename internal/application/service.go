package application

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

type cancelRegistry struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func newCancelRegistry() *cancelRegistry {
	return &cancelRegistry{cancels: map[string]context.CancelFunc{}}
}

func (r *cancelRegistry) register(id string, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels[id] = cancel
}

func (r *cancelRegistry) unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancels, id)
}

func (r *cancelRegistry) cancel(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cancel, ok := r.cancels[id]
	if !ok {
		return false
	}
	cancel()
	return true
}

type Service struct {
	repo     Repository
	defaults Defaults
	allow    func(string) bool
	cancels  *cancelRegistry
	notify   func()
	runtime  runtimeOperationResolver
	newID    func() string
	now      func() time.Time
	logger   *slog.Logger
}

func newService(repo Repository, defaults Defaults, allow func(string) bool, cancels *cancelRegistry, runtime runtimeOperationResolver, newID func() string, logger *slog.Logger) *Service {
	return &Service{
		repo:     repo,
		defaults: defaults,
		allow:    allow,
		cancels:  cancels,
		runtime:  runtime,
		newID:    newID,
		now:      func() time.Time { return time.Now().UTC() },
		logger:   logger,
	}
}

func (s *Service) Create(ctx context.Context, req v1.CreateOperationRequest) (*domain.Operation, bool, error) {
	if strings.TrimSpace(req.Kind) == "" {
		return nil, false, domain.NewError(v1.CodeInvalidRequest, "kind is required")
	}
	if strings.TrimSpace(req.Resource) == "" {
		return nil, false, domain.NewError(v1.CodeInvalidRequest, "resource is required")
	}

	// runtime.* 与 executor.command 共用这一条创建路径：锁、幂等键、请求摘要与 worker
	// 唤醒全部走同一段代码，因此 runtime.start 在 operation get/logs/cancel/retry 上
	// 的行为与命令行执行完全一致，不存在第二条旁路。
	resource := req.Resource
	specJSON := req.Spec
	if isRuntimeKind(req.Kind) {
		if req.DryRun {
			// 适配器端口没有 dry-run 语义：接受它就会变成「以为只是预演、其实真的启停进程」。
			// 只做无副作用的检查请用 runtime/validate。
			return nil, false, domain.NewError(v1.CodeInvalidRequest,
				"%s 不支持 dryRun：它无法阻止适配器真的启停进程，只做校验请用 runtime validate", req.Kind)
		}
		if s.runtime == nil {
			return nil, false, errRuntimeUnsupported()
		}
		resolved, err := s.runtime.ResolveRuntimeOperation(ctx, req.Kind, req.Resource)
		if err != nil {
			return nil, false, err
		}
		// 资源规范成应用 ID：同一个应用无论用名称还是 ID 提交，都命中同一把锁；
		// op.Spec 留空，执行时以「应用的当前规格」为准（与 spec put 的语义一致），
		// 因此重试 runtime.start 用的是最新 manifest，而不是创建时的快照。
		resource = resolved
		specJSON = nil
	} else {
		spec, err := BuildCommandSpec(req.Kind, req.Spec, s.defaults)
		if err != nil {
			return nil, false, err
		}
		if s.allow != nil && !s.allow(spec.Argv[0]) {
			return nil, false, domain.NewError(v1.CodePermissionDenied,
				"executable %q is not permitted by execution.allowedPaths", spec.Argv[0])
		}
	}

	hash, err := requestHash(req.Kind, resource, req.DryRun, specJSON)
	if err != nil {
		return nil, false, err
	}

	op := &domain.Operation{
		ID:             s.newID(),
		Kind:           req.Kind,
		Resource:       resource,
		Status:         domain.StatusPending,
		DryRun:         req.DryRun,
		IdempotencyKey: req.IdempotencyKey,
		RequestHash:    hash,
		Spec:           specJSON,
		CreatedAt:      s.now(),
		CreatedBy:      req.CreatedBy,
	}

	result, err := s.repo.CreateOperation(ctx, op)
	if err != nil {
		return nil, false, err
	}
	if result.Created && s.notify != nil {
		s.notify()
	}
	return result.Operation, result.Created, nil
}

// isRuntimeKind 判定 kind 是否由运行时适配器执行（而不是本机执行器）。
func isRuntimeKind(kind string) bool {
	return kind == v1.KindRuntimeStart || kind == v1.KindRuntimeStop
}

func (s *Service) Get(ctx context.Context, id string) (*domain.Operation, error) {
	return s.repo.GetOperation(ctx, id)
}

func (s *Service) Cancel(ctx context.Context, id string) (*domain.Operation, error) {
	op, cancelledInline, err := s.repo.CancelPending(ctx, id, s.now())
	if err != nil {
		return nil, err
	}
	if cancelledInline {
		return op, nil
	}
	if op.Status == domain.StatusRunning {
		s.cancels.cancel(op.ID)
	}
	return op, nil
}

func (s *Service) Retry(ctx context.Context, id string) (*domain.Operation, error) {
	original, err := s.repo.GetOperation(ctx, id)
	if err != nil {
		return nil, err
	}
	if !domain.CanRetry(original.Status) {
		return nil, domain.NewError(v1.CodeInvalidRequest,
			"operation %s in status %q cannot be retried", id, original.Status)
	}

	hash, err := requestHash(original.Kind, original.Resource, original.DryRun, original.Spec)
	if err != nil {
		return nil, err
	}

	retry := &domain.Operation{
		ID:          s.newID(),
		Kind:        original.Kind,
		Resource:    original.Resource,
		Status:      domain.StatusPending,
		DryRun:      original.DryRun,
		RequestHash: hash,
		RetryOf:     original.ID,
		Spec:        original.Spec,
		CreatedAt:   s.now(),
		CreatedBy:   original.CreatedBy,
	}

	result, err := s.repo.CreateOperation(ctx, retry)
	if err != nil {
		return nil, err
	}
	if !result.Created {
		return result.Operation, nil
	}

	if err := s.repo.AppendAudit(ctx, domain.AuditEvent{
		EventType:   domain.EventOperationRetried,
		Actor:       original.CreatedBy,
		OperationID: retry.ID,
		Resource:    retry.Resource,
		Result:      string(retry.Status),
		Time:        s.now(),
		Details:     map[string]string{"retryOf": original.ID},
	}); err != nil {
		return nil, err
	}
	if s.notify != nil {
		s.notify()
	}
	return result.Operation, nil
}

func (s *Service) Logs(ctx context.Context, id string, cursor int64, limit int) ([]domain.LogEntry, error) {
	if _, err := s.repo.GetOperation(ctx, id); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxLogLimit {
		limit = defaultLogLimit
	}
	if cursor < 0 {
		cursor = 0
	}
	return s.repo.ListLogs(ctx, id, cursor, limit)
}

const (
	defaultLogLimit = 200
	maxLogLimit     = 1000
)

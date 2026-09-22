package application

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/domain"
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
	newID    func() string
	now      func() time.Time
	logger   *slog.Logger
}

func newService(repo Repository, defaults Defaults, allow func(string) bool, cancels *cancelRegistry, newID func() string, logger *slog.Logger) *Service {
	return &Service{
		repo:     repo,
		defaults: defaults,
		allow:    allow,
		cancels:  cancels,
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

	spec, err := BuildCommandSpec(req.Kind, req.Spec, s.defaults)
	if err != nil {
		return nil, false, err
	}
	if s.allow != nil && !s.allow(spec.Argv[0]) {
		return nil, false, domain.NewError(v1.CodePermissionDenied,
			"executable %q is not permitted by execution.allowedPaths", spec.Argv[0])
	}

	hash, err := requestHash(req.Kind, req.Resource, req.DryRun, req.Spec)
	if err != nil {
		return nil, false, err
	}

	op := &domain.Operation{
		ID:             s.newID(),
		Kind:           req.Kind,
		Resource:       req.Resource,
		Status:         domain.StatusPending,
		DryRun:         req.DryRun,
		IdempotencyKey: req.IdempotencyKey,
		RequestHash:    hash,
		Spec:           req.Spec,
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

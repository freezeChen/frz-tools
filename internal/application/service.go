package application

import (
	"context"
	"encoding/json"
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
	backups  backupOperationResolver
	deploys  deployOperationResolver
	newID    func() string
	now      func() time.Time
	logger   *slog.Logger
}

func newService(repo Repository, defaults Defaults, allow func(string) bool, cancels *cancelRegistry, runtime runtimeOperationResolver, backups backupOperationResolver, deploys deployOperationResolver, newID func() string, logger *slog.Logger) *Service {
	return &Service{
		repo:     repo,
		defaults: defaults,
		allow:    allow,
		cancels:  cancels,
		runtime:  runtime,
		backups:  backups,
		deploys:  deploys,
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

	// D7：dryRun 没有真实副作用，也就没有「值得重试的失败」，把两者一起提交是规格误用。
	if req.Retry != nil && req.DryRun {
		return nil, false, domain.NewError(v1.CodeInvalidRequest,
			"dryRun 与 retry 不能同时使用：dryRun 不产生真实副作用，重试它没有意义")
	}

	// 重试策略在提交时校验并序列化成**原文**存下来：事后能查到「当时到底按什么策略
	// 重试的」，且策略字段增减不需要再加数据库列。
	var (
		retryPolicy domain.RetryPolicy
		retryJSON   json.RawMessage
	)
	if req.Retry != nil {
		retryPolicy = domain.RetryPolicyFromSpec(*req.Retry)
		if err := retryPolicy.Validate(); err != nil {
			return nil, false, err
		}
		raw, err := json.Marshal(req.Retry)
		if err != nil {
			return nil, false, domain.NewError(v1.CodeInternal, "无法序列化重试策略: %v", err)
		}
		retryJSON = raw
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
	} else if isBackupKind(req.Kind) {
		if req.DryRun {
			// 与 runtime.* 同样的理由：适配器端口没有 dry-run 语义。备份的预演请用
			// `backup verify`；恢复的「预演」用隔离恢复（--mode isolated）。
			return nil, false, domain.NewError(v1.CodeInvalidRequest,
				"%s 不支持 dryRun：适配器端口没有 dry-run 语义，预演请用 backup verify 或 --mode isolated", req.Kind)
		}
		if s.backups == nil {
			return nil, false, errBackupUnsupported()
		}
		resolved, err := s.backups.ResolveBackupOperation(ctx, req.Kind, req.Resource)
		if err != nil {
			return nil, false, err
		}
		resource = resolved
		if req.Kind == v1.KindBackupRestore {
			// 恢复的模式是**创建时**决定的，必须随操作一起存下来（见 RestoreOptions）。
			options, err := DecodeRestoreOptions(req.Spec)
			if err != nil {
				return nil, false, err
			}
			// 原地恢复的显式确认在这里检查：让它在排队之后就失败，等于让运维
			// 以为提交成功了，而恢复会覆盖真实数据。
			if _, err := s.backups.PrepareRestore(ctx, resolved, options.Mode, options.Confirmed); err != nil {
				return nil, false, err
			}
		} else {
			// backup.run / backup.verify 执行时读「当前」的策略，与 runtime.* 读当前
			// 规格一致：改了策略之后重试，用的是最新那份。
			specJSON = nil
		}
	} else if isDeployKind(req.Kind) {
		if req.DryRun {
			// 与 runtime.* 同一条理由：换版本会重启正在服务的进程，而适配器端口没有
			// dry-run 语义——接受它就会变成「以为只是预演、其实真的换了版本」。
			return nil, false, domain.NewError(v1.CodeInvalidRequest,
				"%s 不支持 dryRun：它无法阻止适配器真的切换版本，只做校验请用 spec put", req.Kind)
		}
		if s.deploys == nil {
			return nil, false, domain.NewError(v1.CodeRuntimeUnsupport, "本部署未启用应用部署能力")
		}
		resolvedResource, resolvedSpec, err := s.deploys.ResolveDeployOperation(ctx, req.Kind, req.Resource, req.Spec)
		if err != nil {
			return nil, false, err
		}
		// 部署与回滚**不读"应用的当前规格"**：要跑的那个版本的配置是创建期就选好的
		// （随 Operation.spec 存下），执行时再去读当前规格会让「回滚」回成一个混合体。
		resource = resolvedResource
		specJSON = resolvedSpec
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

	hash, err := requestHash(req.Kind, resource, req.DryRun, retryJSON, specJSON)
	if err != nil {
		return nil, false, err
	}

	op := &domain.Operation{
		ID:              s.newID(),
		Kind:            req.Kind,
		Resource:        resource,
		Status:          domain.StatusPending,
		DryRun:          req.DryRun,
		IdempotencyKey:  req.IdempotencyKey,
		RequestHash:     hash,
		Spec:            specJSON,
		CreatedAt:       s.now(),
		CreatedBy:       req.CreatedBy,
		Attempt:         1,
		RetryPolicy:     retryPolicy,
		RetryPolicyJSON: retryJSON,
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

// errBackupUnsupported 是「本部署没配备份能力」时的统一说法，与 runtime 的对应函数同形。
func errBackupUnsupported() error {
	return domain.NewError(v1.CodeConfigInvalid,
		"本部署未配置备份存储根（backupStore.root），备份相关操作不可用")
}

// isDeployKind 判定 kind 是否是部署类操作。
func isDeployKind(kind string) bool {
	return kind == v1.KindAppDeploy || kind == v1.KindAppRollback
}

// isBackupKind 判定 kind 是否是备份类操作。
func isBackupKind(kind string) bool {
	switch kind {
	case v1.KindBackupRun, v1.KindBackupVerify, v1.KindBackupRestore:
		return true
	}
	return false
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

	hash, err := requestHash(original.Kind, original.Resource, original.DryRun, original.RetryPolicyJSON, original.Spec)
	if err != nil {
		return nil, err
	}

	// 手动重试沿用链上的策略（这样后续失败仍会自动重试），但**不受 maxAttempts 限制**：
	// 运维的判断优先于策略的自动上限。attempt 照常递增，让链上的计数保持连续，
	// 因此「第几次尝试」这件事永远只有一个说法。
	retry := &domain.Operation{
		ID:              s.newID(),
		Kind:            original.Kind,
		Resource:        original.Resource,
		Status:          domain.StatusPending,
		DryRun:          original.DryRun,
		RequestHash:     hash,
		RetryOf:         original.ID,
		Spec:            original.Spec,
		CreatedAt:       s.now(),
		CreatedBy:       original.CreatedBy,
		Attempt:         original.Attempt + 1,
		RetryPolicy:     original.RetryPolicy,
		RetryPolicyJSON: original.RetryPolicyJSON,
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

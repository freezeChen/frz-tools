package application

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

const (
	defaultScheduleRunLimit = 100
	maxScheduleLimit        = 1000

	scheduleIDPrefix    = "sch"
	scheduleRunIDPrefix = "run"
)

// CreateScheduleInput 是创建计划的入参。spec 与触发时创建的 Operation 完全一致，
// 因此计划在创建时就能被校验，不必等到触发时才发现它本身是坏的。
type CreateScheduleInput struct {
	Name     string
	Kind     domain.ScheduleKind
	Cron     string
	Interval time.Duration
	Timezone string
	Resource string
	// OperationKind 省略时是 executor.command。
	OperationKind   string
	Spec            json.RawMessage
	MissedRunPolicy domain.MissedRunPolicy
	CreatedBy       string
}

type ScheduleService struct {
	repo     Repository
	defaults Defaults
	newID    func(prefix string) string
	now      func() time.Time
	notify   func()
}

func newScheduleService(repo Repository, defaults Defaults, newID func(prefix string) string, now func() time.Time, notify func()) *ScheduleService {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &ScheduleService{repo: repo, defaults: defaults, newID: newID, now: now, notify: notify}
}

func (s *ScheduleService) Create(ctx context.Context, in CreateScheduleInput) (*domain.Schedule, error) {
	// 按**声明出来的**操作类型校验 spec：用与触发时同一套规则，让错误的 spec 在
	// 创建计划时就失败，而不是等到半夜触发时才发现。
	kind := in.OperationKind
	if kind == "" {
		kind = v1.KindExecutorCommand
	}
	switch {
	case kind == v1.KindExecutorCommand:
		if _, err := BuildCommandSpec(kind, in.Spec, s.defaults); err != nil {
			return nil, err
		}
	case kind == v1.KindBackupRestore:
		// 恢复模式写在计划里，因此这里就要能解出来——否则「mode 写错了」要等到
		// 半夜触发时才被发现。
		if _, err := DecodeRestoreOptions(in.Spec); err != nil {
			return nil, err
		}
	case isRuntimeKind(kind) || kind == v1.KindBackupRun || kind == v1.KindBackupVerify:
		// 这几类执行时读**当前**的规格或策略（与手工提交一致），因此计划本身不带 spec。
		if len(in.Spec) > 0 {
			return nil, domain.NewError(v1.CodeScheduleInvalid,
				"%s 执行时读当前的规格/策略，计划里不应带 spec", kind)
		}
	}

	now := s.now()
	schedule := &domain.Schedule{
		ID:              s.newID(scheduleIDPrefix),
		Name:            strings.TrimSpace(in.Name),
		Enabled:         true,
		Kind:            in.Kind,
		Cron:            in.Cron,
		Interval:        in.Interval,
		Timezone:        in.Timezone,
		Resource:        in.Resource,
		OperationKind:   in.OperationKind,
		Spec:            in.Spec,
		MissedRunPolicy: in.MissedRunPolicy,
		CreatedAt:       now,
		UpdatedAt:       now,
		CreatedBy:       in.CreatedBy,
	}
	if schedule.MissedRunPolicy == "" {
		schedule.MissedRunPolicy = domain.MissedRunSkip
	}
	if err := schedule.Validate(); err != nil {
		return nil, err
	}

	evaluator, err := domain.NewEvaluator(schedule)
	if err != nil {
		return nil, err
	}
	next := evaluator.Next(now)
	schedule.NextRunAt = &next

	if err := s.repo.CreateSchedule(ctx, schedule); err != nil {
		return nil, err
	}
	if err := s.repo.AppendAudit(ctx, domain.AuditEvent{
		EventType: domain.EventScheduleCreated,
		Actor:     in.CreatedBy,
		Resource:  schedule.Resource,
		Result:    "created",
		Time:      now,
		Details:   map[string]string{"scheduleId": schedule.ID, "name": schedule.Name, "nextRunAt": next.Format(time.RFC3339)},
	}); err != nil {
		return nil, err
	}

	s.signal()
	return schedule, nil
}

func (s *ScheduleService) Get(ctx context.Context, ref string) (*domain.Schedule, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, domain.NewError(v1.CodeInvalidRequest, "计划引用不能为空")
	}
	return s.repo.GetSchedule(ctx, ref)
}

func (s *ScheduleService) List(ctx context.Context, limit int) ([]domain.Schedule, error) {
	if limit <= 0 || limit > maxScheduleLimit {
		limit = defaultScheduleRunLimit
	}
	return s.repo.ListSchedules(ctx, limit)
}

// SetEnabled 启用时重新计算下一次触发时间：停用期间积累的时刻不做补偿，
// 否则一启用就会立刻补跑一堆过期任务。
func (s *ScheduleService) SetEnabled(ctx context.Context, ref string, enabled bool) (*domain.Schedule, error) {
	schedule, err := s.repo.GetSchedule(ctx, ref)
	if err != nil {
		return nil, err
	}

	now := s.now()
	if enabled {
		evaluator, err := domain.NewEvaluator(schedule)
		if err != nil {
			return nil, err
		}
		next := evaluator.Next(now)
		if err := s.repo.UpdateScheduleProgress(ctx, schedule.ID, domain.ScheduleProgress{
			NextRunAt:  &next,
			LastResult: schedule.LastResult,
		}, now); err != nil {
			return nil, err
		}
	}

	updated, err := s.repo.SetScheduleEnabled(ctx, schedule.ID, enabled, now)
	if err != nil {
		return nil, err
	}

	event := domain.EventScheduleEnabled
	result := "enabled"
	if !enabled {
		event = domain.EventScheduleDisabled
		result = "disabled"
	}
	if err := s.repo.AppendAudit(ctx, domain.AuditEvent{
		EventType: event,
		Resource:  updated.Resource,
		Result:    result,
		Time:      now,
		Details:   map[string]string{"scheduleId": updated.ID, "name": updated.Name},
	}); err != nil {
		return nil, err
	}

	s.signal()
	return updated, nil
}

// Delete 只允许删除已停用的计划，避免「删掉一个还在跑的计划」这种歧义操作。
func (s *ScheduleService) Delete(ctx context.Context, ref string) error {
	schedule, err := s.repo.GetSchedule(ctx, ref)
	if err != nil {
		return err
	}
	if schedule.Enabled {
		return domain.NewError(v1.CodeScheduleEnabled,
			"计划 %s 仍在启用中，请先停用再删除", schedule.Name)
	}

	if err := s.repo.DeleteSchedule(ctx, schedule.ID); err != nil {
		return err
	}
	if err := s.repo.AppendAudit(ctx, domain.AuditEvent{
		EventType: domain.EventScheduleDeleted,
		Resource:  schedule.Resource,
		Result:    "deleted",
		Time:      s.now(),
		Details:   map[string]string{"scheduleId": schedule.ID, "name": schedule.Name},
	}); err != nil {
		return err
	}

	s.signal()
	return nil
}

func (s *ScheduleService) Runs(ctx context.Context, ref string, limit int) ([]domain.ScheduleRun, error) {
	schedule, err := s.repo.GetSchedule(ctx, ref)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxScheduleLimit {
		limit = defaultScheduleRunLimit
	}
	return s.repo.ListScheduleRuns(ctx, schedule.ID, limit)
}

func (s *ScheduleService) signal() {
	if s.notify != nil {
		s.notify()
	}
}

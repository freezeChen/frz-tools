package application

import (
	"context"
	"errors"
	"log/slog"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// fallbackPoll 是没有任何启用计划时的兜底唤醒周期。正常情况下调度器按最近的
// next_run_at 精确等待，不依赖这个周期。
const fallbackPoll = 60 * time.Second

// minWait 防止已到期的计划导致紧密循环。
const minWait = 200 * time.Millisecond

// misfireThreshold 是「正常到点」与「停机积压」的分界线。调度器按精确时刻唤醒，
// 正常情况下的延迟在毫秒级；只有恰好一个时刻、且延迟没超过这个窗口，才算正常到点，
// 此时无论错过策略是什么都应当执行——否则 skip 策略会把自己的正常触发也吞掉。
const misfireThreshold = time.Minute

// Scheduler 是触发时刻的唯一权威：它只决定「什么时候该跑」，把真正的执行交给
// 既有的 Operation 流程（resource 锁、状态机、审计、日志、取消、重试都不变）。
type Scheduler struct {
	repo   Repository
	newID  func(prefix string) string
	logger *slog.Logger
	now    func() time.Time
	// notify 用于在派发成功后唤醒 worker 池。
	notify func()

	wake chan struct{}
}

func newScheduler(repo Repository, newID func(prefix string) string, logger *slog.Logger, now func() time.Time, notify func()) *Scheduler {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Scheduler{
		repo:   repo,
		newID:  newID,
		logger: logger,
		now:    now,
		notify: notify,
		wake:   make(chan struct{}, 1),
	}
}

// Wake 通知调度器计划发生了变化，需要立即重算等待时间。
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) Run(ctx context.Context) {
	s.logger.Info("调度器已启动")
	defer s.logger.Info("调度器已停止")

	for {
		wait := s.nextWait(ctx)

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
		case <-timer.C:
		}

		if err := s.Tick(ctx); err != nil {
			s.logger.Error("调度轮询失败", "error", err)
		}
	}
}

// nextWait 返回应当等待多久。没有任何启用计划时返回兜底周期。
func (s *Scheduler) nextWait(ctx context.Context) time.Duration {
	schedules, err := s.repo.ListEnabledSchedules(ctx)
	if err != nil {
		s.logger.Error("无法载入启用中的计划", "error", err)
		return fallbackPoll
	}

	now := s.now()
	earliest := time.Time{}
	for _, schedule := range schedules {
		// 没有 next_run_at 说明是新建但尚未初始化，立即处理一次。
		if schedule.NextRunAt == nil {
			return minWait
		}
		if earliest.IsZero() || schedule.NextRunAt.Before(earliest) {
			earliest = *schedule.NextRunAt
		}
	}
	if earliest.IsZero() {
		return fallbackPoll
	}
	if !earliest.After(now) {
		return minWait
	}
	if wait := earliest.Sub(now); wait < minWait {
		return minWait
	} else {
		return wait
	}
}

// Tick 处理所有已到期的计划，并做停机期间的补偿。它同时承担启动时的恢复职责：
// next_run_at 已在过去，就会按错过执行策略处理。
func (s *Scheduler) Tick(ctx context.Context) error {
	schedules, err := s.repo.ListEnabledSchedules(ctx)
	if err != nil {
		return err
	}

	now := s.now()
	var failures []error
	for _, schedule := range schedules {
		if err := s.processSchedule(ctx, schedule, now); err != nil {
			// 一条计划坏掉不能影响其它计划，因此记录后继续。
			s.logger.Error("处理计划失败", "schedule", schedule.Name, "error", err)
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Scheduler) processSchedule(ctx context.Context, schedule domain.Schedule, now time.Time) error {
	evaluator, err := domain.NewEvaluator(&schedule)
	if err != nil {
		return err
	}

	if schedule.NextRunAt == nil {
		next := evaluator.Next(now)
		return s.repo.UpdateScheduleProgress(ctx, schedule.ID, domain.ScheduleProgress{
			NextRunAt:  &next,
			LastResult: schedule.LastResult,
		}, now)
	}
	if schedule.NextRunAt.After(now) {
		return nil
	}

	moments, truncated := domain.DueMoments(evaluator, *schedule.NextRunAt, now, 0)
	if truncated {
		s.logger.Warn("错过执行过多，本次只补偿到上限",
			"schedule", schedule.Name, "limit", len(moments))
	}

	dispatch := chooseDispatchedMoments(moments, schedule.MissedRunPolicy, now)
	lastResult := schedule.LastResult

	for _, moment := range moments {
		if !dispatch[moment.UnixNano()] {
			handled, err := s.repo.RecordMissedRun(ctx, schedule.ID, s.newID(scheduleRunIDPrefix), moment, now)
			if err != nil {
				return err
			}
			if !handled {
				lastResult = string(domain.RunMissed)
			}
			continue
		}

		operation, err := s.buildOperation(&schedule, now)
		if err != nil {
			return err
		}
		run, handled, err := s.repo.DispatchScheduledRun(ctx, domain.ScheduledDispatch{
			ScheduleID:   schedule.ID,
			ScheduledFor: moment,
			RunID:        s.newID(scheduleRunIDPrefix),
			Operation:    operation,
			Now:          now,
		})
		if err != nil {
			return err
		}
		if handled {
			// 该时刻此前已处理过（重启后重算），不重复触发。
			continue
		}
		lastResult = string(run.Result)
		if run.Result == domain.RunDispatched {
			s.notifyWorkers()
		} else {
			s.logger.Warn("计划触发被拒绝",
				"schedule", schedule.Name, "scheduledFor", moment, "errorCode", run.ErrorCode)
		}
	}

	next := evaluator.Next(moments[len(moments)-1])
	if truncated {
		// 被截断时不要继续追赶积压，从当前时刻之后重新开始。
		next = evaluator.Next(now)
	}
	lastMoment := moments[len(moments)-1]
	return s.repo.UpdateScheduleProgress(ctx, schedule.ID, domain.ScheduleProgress{
		NextRunAt:  &next,
		LastRunAt:  &lastMoment,
		LastResult: lastResult,
	}, now)
}

// chooseDispatchedMoments 挑出真正要派发的时刻。
//
// 正常到点（恰好一个时刻且延迟在宽限窗口内）一律派发；否则按错过执行策略决定：
// skip 全不派发、runOnce 只派发最近一次。
func chooseDispatchedMoments(moments []time.Time, policy domain.MissedRunPolicy, now time.Time) map[int64]bool {
	dispatch := make(map[int64]bool, len(moments))

	if len(moments) == 1 && now.Sub(moments[0]) <= misfireThreshold {
		dispatch[moments[0].UnixNano()] = true
		return dispatch
	}

	if policy == domain.MissedRunOnce {
		dispatch[moments[len(moments)-1].UnixNano()] = true
	}
	return dispatch
}

// buildOperation 把计划翻译成一条 pending Operation。它走的是与手工提交完全相同的
// 路径，因此 resource 锁与幂等语义不会有第二套实现。
func (s *Scheduler) buildOperation(schedule *domain.Schedule, now time.Time) (*domain.Operation, error) {
	hash, err := requestHash(v1.KindExecutorCommand, schedule.Resource, false, schedule.Spec)
	if err != nil {
		return nil, err
	}
	return &domain.Operation{
		ID:          s.newID("op"),
		Kind:        v1.KindExecutorCommand,
		Resource:    schedule.Resource,
		Status:      domain.StatusPending,
		RequestHash: hash,
		Spec:        schedule.Spec,
		CreatedAt:   now,
		CreatedBy:   "schedule:" + schedule.Name,
	}, nil
}

func (s *Scheduler) notifyWorkers() {
	if s.notify != nil {
		s.notify()
	}
}

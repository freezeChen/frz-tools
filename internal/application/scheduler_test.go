package application

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/adapters/sqlite"
	"frz-tools/internal/domain"
)

// fakeClock 让调度测试不必真的等待：调度器与计划服务都通过它取当前时间。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(at time.Time) *fakeClock { return &fakeClock{now: at} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newSchedulerRuntime(t *testing.T, policy domain.MissedRunPolicy) (*Runtime, *fakeClock, *sqlite.Store) {
	t.Helper()

	clock := newFakeClock(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	rt, store := newTestRuntimeWith(t, Options{Now: clock.Now})
	return rt, clock, store
}

func createIntervalSchedule(t *testing.T, rt *Runtime, name string, every time.Duration, policy domain.MissedRunPolicy) *domain.Schedule {
	t.Helper()
	schedule, err := rt.Schedules.Create(context.Background(), CreateScheduleInput{
		Name:            name,
		Kind:            domain.ScheduleKindInterval,
		Interval:        every,
		Resource:        "sched-" + name,
		Spec:            json.RawMessage(`{"argv":["/usr/bin/true"]}`),
		MissedRunPolicy: policy,
		CreatedBy:       "tester",
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	return schedule
}

func TestCreateScheduleInitialisesNextRun(t *testing.T) {
	rt, clock, _ := newSchedulerRuntime(t, domain.MissedRunSkip)
	schedule := createIntervalSchedule(t, rt, "every-minute", time.Minute, domain.MissedRunSkip)

	if schedule.NextRunAt == nil {
		t.Fatal("创建计划时应当算出下一次触发时间")
	}
	want := clock.Now().Add(time.Minute)
	if !schedule.NextRunAt.Equal(want) {
		t.Fatalf("want %s, got %s", want, schedule.NextRunAt)
	}
	if !schedule.Enabled {
		t.Fatal("新建的计划默认启用")
	}
}

// 计划里带一个坏的 spec 必须在创建时就失败，而不是等到触发时才暴露。
func TestCreateScheduleValidatesSpec(t *testing.T) {
	rt, _, _ := newSchedulerRuntime(t, domain.MissedRunSkip)

	_, err := rt.Schedules.Create(context.Background(), CreateScheduleInput{
		Name:     "bad-spec",
		Kind:     domain.ScheduleKindInterval,
		Interval: time.Minute,
		Resource: "r",
		Spec:     json.RawMessage(`{"argv":["relative-binary"]}`),
	})
	if domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
}

func TestTickDoesNothingBeforeDue(t *testing.T) {
	rt, clock, store := newSchedulerRuntime(t, domain.MissedRunSkip)
	schedule := createIntervalSchedule(t, rt, "later", time.Minute, domain.MissedRunSkip)
	ctx := context.Background()

	clock.Advance(30 * time.Second)
	if err := rt.Scheduler.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	runs, err := rt.Schedules.Runs(ctx, schedule.ID, 10)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("未到期不应产生任何记录: %+v", runs)
	}
	if pending, err := store.CountByStatus(ctx, domain.StatusPending); err != nil || pending != 0 {
		t.Fatalf("未到期不应创建 Operation: %d %v", pending, err)
	}
}

func TestTickDispatchesDueScheduleOnce(t *testing.T) {
	rt, clock, store := newSchedulerRuntime(t, domain.MissedRunSkip)
	schedule := createIntervalSchedule(t, rt, "dispatch", time.Minute, domain.MissedRunSkip)
	ctx := context.Background()

	clock.Advance(time.Minute)
	if err := rt.Scheduler.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	runs, err := rt.Schedules.Runs(ctx, schedule.ID, 10)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 1 || runs[0].Result != domain.RunDispatched {
		t.Fatalf("want 一条 dispatched，got %+v", runs)
	}
	if runs[0].OperationID == "" {
		t.Fatal("dispatched 的记录必须指向创建的 Operation")
	}
	if _, err := store.GetOperation(ctx, runs[0].OperationID); err != nil {
		t.Fatalf("派发应当创建 Operation: %v", err)
	}

	// 进度必须推进到下一个时刻，否则调度器会一直认为它已到期。
	updated, err := rt.Schedules.Get(ctx, schedule.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	wantNext := clock.Now().Add(time.Minute)
	if updated.NextRunAt == nil || !updated.NextRunAt.Equal(wantNext) {
		t.Fatalf("next_run_at 应推进到 %s，got %v", wantNext, updated.NextRunAt)
	}

	// 同一时刻再来一次不得重复触发。
	if err := rt.Scheduler.Tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	runs, _ = rt.Schedules.Runs(ctx, schedule.ID, 10)
	if len(runs) != 1 {
		t.Fatalf("同一时刻不得重复触发，got %d 条记录", len(runs))
	}
}

func TestMissedRunPolicies(t *testing.T) {
	// 时钟从 T0 走到 T0+5min，间隔 1 分钟 => 到期时刻为 T0+1 .. T0+5 共 5 个。
	// 5 个时刻说明这是停机积压而不是正常到点，因此错过策略生效。
	cases := []struct {
		policy     domain.MissedRunPolicy
		dispatched int
		missed     int
	}{
		{domain.MissedRunSkip, 0, 5},
		{domain.MissedRunOnce, 1, 4},
	}

	for _, tc := range cases {
		t.Run(string(tc.policy), func(t *testing.T) {
			rt, clock, store := newSchedulerRuntime(t, tc.policy)
			schedule := createIntervalSchedule(t, rt, "policy", time.Minute, tc.policy)
			ctx := context.Background()

			clock.Advance(5 * time.Minute)
			if err := rt.Scheduler.Tick(ctx); err != nil {
				t.Fatalf("tick: %v", err)
			}

			runs, err := rt.Schedules.Runs(ctx, schedule.ID, 20)
			if err != nil {
				t.Fatalf("runs: %v", err)
			}
			counts := map[domain.ScheduleRunResult]int{}
			for _, run := range runs {
				counts[run.Result]++
			}
			if counts[domain.RunDispatched] != tc.dispatched {
				t.Fatalf("dispatched want %d, got %d (%+v)", tc.dispatched, counts[domain.RunDispatched], runs)
			}
			if counts[domain.RunMissed] != tc.missed {
				t.Fatalf("missed want %d, got %d (%+v)", tc.missed, counts[domain.RunMissed], runs)
			}

			pending, err := store.CountByStatus(ctx, domain.StatusPending)
			if err != nil {
				t.Fatalf("count pending: %v", err)
			}
			if pending != tc.dispatched {
				t.Fatalf("创建的 Operation 数应等于 dispatched 数：want %d, got %d", tc.dispatched, pending)
			}

			// runOnce 补跑的必须是最近那一次。
			if tc.policy == domain.MissedRunOnce {
				for _, run := range runs {
					if run.Result == domain.RunDispatched {
						want := clock.Now()
						if !run.ScheduledFor.Equal(want) {
							t.Fatalf("runOnce 应当补跑最近一次 %s，got %s", want, run.ScheduledFor)
						}
					}
				}
			}
		})
	}
}

// 「正常到点」与「停机积压」的分界必须有测试钉住：只有恰好一个到期时刻、
// 且延迟在宽限窗口内，才算正常到点；否则按错过策略处理。
func TestMisfireBoundary(t *testing.T) {
	// 用 1 小时间隔，才能造出「只有一次到期、且迟到超过 1 分钟」的情形。
	const interval = time.Hour

	t.Run("准点到点：即便 skip 也会执行", func(t *testing.T) {
		rt, clock, store := newSchedulerRuntime(t, domain.MissedRunSkip)
		schedule := createIntervalSchedule(t, rt, "on-time", interval, domain.MissedRunSkip)
		ctx := context.Background()

		clock.Advance(interval)
		if err := rt.Scheduler.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}

		runs, err := rt.Schedules.Runs(ctx, schedule.ID, 10)
		if err != nil {
			t.Fatalf("runs: %v", err)
		}
		if len(runs) != 1 || runs[0].Result != domain.RunDispatched {
			t.Fatalf("准点触发必须执行，skip 策略不应把它丢掉: %+v", runs)
		}
		if pending, _ := store.CountByStatus(ctx, domain.StatusPending); pending != 1 {
			t.Fatalf("应当创建 1 条 Operation，got %d", pending)
		}
	})

	t.Run("迟到超过宽限：skip 丢弃", func(t *testing.T) {
		rt, clock, store := newSchedulerRuntime(t, domain.MissedRunSkip)
		schedule := createIntervalSchedule(t, rt, "late-skip", interval, domain.MissedRunSkip)
		ctx := context.Background()

		clock.Advance(interval + 10*time.Minute)
		if err := rt.Scheduler.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}

		runs, err := rt.Schedules.Runs(ctx, schedule.ID, 10)
		if err != nil {
			t.Fatalf("runs: %v", err)
		}
		if len(runs) != 1 || runs[0].Result != domain.RunMissed {
			t.Fatalf("want 1 条 missed，got %+v", runs)
		}
		if pending, _ := store.CountByStatus(ctx, domain.StatusPending); pending != 0 {
			t.Fatalf("skip 不应创建 Operation，got %d", pending)
		}
	})

	t.Run("迟到超过宽限：runOnce 补跑那一次", func(t *testing.T) {
		rt, clock, _ := newSchedulerRuntime(t, domain.MissedRunOnce)
		schedule := createIntervalSchedule(t, rt, "late-once", interval, domain.MissedRunOnce)
		dueAt := *schedule.NextRunAt
		ctx := context.Background()

		clock.Advance(interval + 10*time.Minute)
		if err := rt.Scheduler.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}

		runs, err := rt.Schedules.Runs(ctx, schedule.ID, 10)
		if err != nil {
			t.Fatalf("runs: %v", err)
		}
		if len(runs) != 1 || runs[0].Result != domain.RunDispatched {
			t.Fatalf("want 1 条 dispatched，got %+v", runs)
		}
		if !runs[0].ScheduledFor.Equal(dueAt) {
			t.Fatalf("补跑的应当是计划时刻 %s，got %s", dueAt, runs[0].ScheduledFor)
		}
	})
}
func TestTickRecordsLockBusyWithoutQueueing(t *testing.T) {
	rt, clock, _ := newSchedulerRuntime(t, domain.MissedRunSkip)
	schedule := createIntervalSchedule(t, rt, "busy", time.Minute, domain.MissedRunSkip)
	ctx := context.Background()

	// 先在同一个 resource 上占一个未完成的操作（走与手工提交相同的路径）。
	if _, _, err := rt.Service.Create(ctx, v1.CreateOperationRequest{
		Kind:     v1.KindExecutorCommand,
		Resource: schedule.Resource,
		Spec:     json.RawMessage(`{"argv":["/usr/bin/true"]}`),
	}); err != nil {
		t.Fatalf("create occupier: %v", err)
	}

	clock.Advance(time.Minute)
	if err := rt.Scheduler.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	runs, err := rt.Schedules.Runs(ctx, schedule.ID, 10)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	if runs[0].Result != domain.RunFailed || runs[0].ErrorCode != string(v1.CodeLockBusy) {
		t.Fatalf("want failed/LOCK_BUSY, got %+v", runs[0])
	}
	if runs[0].OperationID != "" {
		t.Fatalf("被占用时不得创建 Operation，got %s", runs[0].OperationID)
	}
}

func TestDisabledScheduleIsNotProcessed(t *testing.T) {
	rt, clock, _ := newSchedulerRuntime(t, domain.MissedRunSkip)
	schedule := createIntervalSchedule(t, rt, "paused", time.Minute, domain.MissedRunSkip)
	ctx := context.Background()

	if _, err := rt.Schedules.SetEnabled(ctx, schedule.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	clock.Advance(10 * time.Minute)
	if err := rt.Scheduler.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	runs, err := rt.Schedules.Runs(ctx, schedule.ID, 10)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("停用的计划不应被处理: %+v", runs)
	}
}

// 重新启用时不做补偿：否则一启用就会补跑一堆过期任务。
func TestReEnableResetsNextRunInsteadOfBackfilling(t *testing.T) {
	rt, clock, _ := newSchedulerRuntime(t, domain.MissedRunSkip)
	schedule := createIntervalSchedule(t, rt, "re-enable", time.Minute, domain.MissedRunSkip)
	ctx := context.Background()

	if _, err := rt.Schedules.SetEnabled(ctx, schedule.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	clock.Advance(time.Hour)

	reEnabled, err := rt.Schedules.SetEnabled(ctx, schedule.ID, true)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}

	if reEnabled.NextRunAt == nil || !reEnabled.NextRunAt.After(clock.Now()) {
		t.Fatalf("重新启用后 next_run_at 应当落在未来，got %v", reEnabled.NextRunAt)
	}

	if err := rt.Scheduler.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	runs, _ := rt.Schedules.Runs(ctx, schedule.ID, 10)
	if len(runs) != 0 {
		t.Fatalf("重新启用不应补跑停用期间的任务: %+v", runs)
	}
}

func TestDeleteRequiresDisabledSchedule(t *testing.T) {
	rt, _, _ := newSchedulerRuntime(t, domain.MissedRunSkip)
	schedule := createIntervalSchedule(t, rt, "to-delete", time.Minute, domain.MissedRunSkip)
	ctx := context.Background()

	if err := rt.Schedules.Delete(ctx, schedule.ID); domain.CodeOf(err) != v1.CodeScheduleEnabled {
		t.Fatalf("启用中的计划必须拒绝删除，got %v", err)
	}
	if _, err := rt.Schedules.SetEnabled(ctx, schedule.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := rt.Schedules.Delete(ctx, schedule.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := rt.Schedules.Get(ctx, schedule.ID); domain.CodeOf(err) != v1.CodeScheduleNotFound {
		t.Fatalf("删除后不应再查得到，got %v", err)
	}
}

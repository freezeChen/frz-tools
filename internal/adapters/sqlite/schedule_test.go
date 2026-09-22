package sqlite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/domain"
	"frz-tools/internal/idgen"
)

func newSchedule(name string) *domain.Schedule {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &domain.Schedule{
		ID:              idgen.New("sch"),
		Name:            name,
		Enabled:         true,
		Kind:            domain.ScheduleKindCron,
		Cron:            "0 2 * * *",
		Timezone:        "UTC",
		Resource:        "sched-" + name,
		Spec:            json.RawMessage(`{"argv":["/usr/bin/true"]}`),
		MissedRunPolicy: domain.MissedRunSkip,
		NextRunAt:       &now,
		CreatedAt:       now,
		UpdatedAt:       now,
		CreatedBy:       "tester",
	}
}

func TestCreateAndGetSchedule(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	schedule := newSchedule("nightly")
	if err := store.CreateSchedule(ctx, schedule); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, ref := range []string{schedule.ID, "nightly"} {
		found, err := store.GetSchedule(ctx, ref)
		if err != nil {
			t.Fatalf("get %q: %v", ref, err)
		}
		if found.ID != schedule.ID || found.Cron != "0 2 * * *" || found.Timezone != "UTC" {
			t.Fatalf("unexpected schedule: %+v", found)
		}
		if found.NextRunAt == nil {
			t.Fatal("next_run_at 应当被保存")
		}
	}

	if _, err := store.GetSchedule(ctx, "missing"); domain.CodeOf(err) != v1.CodeScheduleNotFound {
		t.Fatalf("want SCHEDULE_NOT_FOUND, got %v", err)
	}
}

func TestCreateScheduleRejectsDuplicateName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.CreateSchedule(ctx, newSchedule("dup")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.CreateSchedule(ctx, newSchedule("dup")); domain.CodeOf(err) != v1.CodeScheduleInvalid {
		t.Fatalf("重名必须被拒绝，got %v", err)
	}
}

func TestCreateScheduleValidatesInput(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	invalid := newSchedule("bad")
	invalid.Cron = "not a cron"
	if err := store.CreateSchedule(ctx, invalid); domain.CodeOf(err) != v1.CodeScheduleInvalid {
		t.Fatalf("非法 cron 必须被拒绝，got %v", err)
	}
}

func TestListEnabledSchedulesSkipsDisabled(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	active := newSchedule("active")
	disabled := newSchedule("disabled")
	disabled.Enabled = false
	for _, schedule := range []*domain.Schedule{active, disabled} {
		if err := store.CreateSchedule(ctx, schedule); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	enabled, err := store.ListEnabledSchedules(ctx)
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	if len(enabled) != 1 || enabled[0].Name != "active" {
		t.Fatalf("停用的计划不应被载入调度: %+v", enabled)
	}

	all, err := store.ListSchedules(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 schedules, got %d", len(all))
	}
}

func TestSetScheduleEnabledAndProgress(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	schedule := newSchedule("toggle")
	if err := store.CreateSchedule(ctx, schedule); err != nil {
		t.Fatalf("create: %v", err)
	}

	paused, err := store.SetScheduleEnabled(ctx, schedule.ID, false, now)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if paused.Enabled {
		t.Fatal("停用后 enabled 应为 false")
	}

	next := now.Add(24 * time.Hour)
	if err := store.UpdateScheduleProgress(ctx, schedule.ID, domain.ScheduleProgress{
		NextRunAt:  &next,
		LastRunAt:  &now,
		LastResult: string(domain.RunDispatched),
	}, now); err != nil {
		t.Fatalf("update progress: %v", err)
	}

	updated, err := store.GetSchedule(ctx, schedule.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.NextRunAt == nil || !updated.NextRunAt.Equal(next) {
		t.Fatalf("next_run_at 未更新: %+v", updated.NextRunAt)
	}
	if updated.LastResult != string(domain.RunDispatched) {
		t.Fatalf("last_result 未更新: %q", updated.LastResult)
	}

	if err := store.DeleteSchedule(ctx, schedule.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.DeleteSchedule(ctx, schedule.ID); domain.CodeOf(err) != v1.CodeScheduleNotFound {
		t.Fatalf("重复删除 want SCHEDULE_NOT_FOUND, got %v", err)
	}
}

func TestDispatchScheduledRunCreatesOperationOnce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	scheduledFor := now

	schedule := newSchedule("dispatch")
	if err := store.CreateSchedule(ctx, schedule); err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	operation := newOperation(schedule.Resource)
	operation.CreatedAt = now

	run, handled, err := store.DispatchScheduledRun(ctx, domain.ScheduledDispatch{
		ScheduleID:   schedule.ID,
		ScheduledFor: scheduledFor,
		RunID:        idgen.New("run"),
		Operation:    operation,
		Now:          now,
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if handled {
		t.Fatal("首次派发不应被判为已处理")
	}
	if run.Result != domain.RunDispatched || run.OperationID != operation.ID {
		t.Fatalf("unexpected run: %+v", run)
	}
	if _, err := store.GetOperation(ctx, operation.ID); err != nil {
		t.Fatalf("派发应当创建 Operation: %v", err)
	}

	// 同一时刻再次派发必须被识别为已处理，且不产生第二条 Operation。
	second := newOperation(schedule.Resource)
	_, handled, err = store.DispatchScheduledRun(ctx, domain.ScheduledDispatch{
		ScheduleID:   schedule.ID,
		ScheduledFor: scheduledFor,
		RunID:        idgen.New("run"),
		Operation:    second,
		Now:          now,
	})
	if err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if !handled {
		t.Fatal("同一时刻重复派发必须被判为已处理")
	}
	if _, err := store.GetOperation(ctx, second.ID); domain.CodeOf(err) != v1.CodeOperationNotFound {
		t.Fatalf("重复派发不得创建第二条 Operation，got %v", err)
	}

	runs, err := store.ListScheduleRuns(ctx, schedule.ID, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
}

// 资源被占用时该次触发记为 failed/LOCK_BUSY，且不创建 Operation、不排队。
func TestDispatchScheduledRunRecordsLockBusy(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	schedule := newSchedule("busy")
	if err := store.CreateSchedule(ctx, schedule); err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	// 先在同一个 resource 上放一个未完成的操作。
	occupier := newOperation(schedule.Resource)
	if _, err := store.CreateOperation(ctx, occupier); err != nil {
		t.Fatalf("create occupier: %v", err)
	}

	operation := newOperation(schedule.Resource)
	run, handled, err := store.DispatchScheduledRun(ctx, domain.ScheduledDispatch{
		ScheduleID:   schedule.ID,
		ScheduledFor: now,
		RunID:        idgen.New("run"),
		Operation:    operation,
		Now:          now,
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if handled {
		t.Fatal("资源被占用时仍应落一条记录，而不是判为已处理")
	}
	if run.Result != domain.RunFailed || run.ErrorCode != string(v1.CodeLockBusy) {
		t.Fatalf("want failed/LOCK_BUSY, got %+v", run)
	}
	if _, err := store.GetOperation(ctx, operation.ID); domain.CodeOf(err) != v1.CodeOperationNotFound {
		t.Fatalf("资源被占用时不得创建 Operation，got %v", err)
	}
}

func TestRecordMissedRunIsIdempotentPerMoment(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	schedule := newSchedule("missed")
	if err := store.CreateSchedule(ctx, schedule); err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	handled, err := store.RecordMissedRun(ctx, schedule.ID, idgen.New("run"), now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("record missed: %v", err)
	}
	if handled {
		t.Fatal("首次记录不应被判为已处理")
	}

	handled, err = store.RecordMissedRun(ctx, schedule.ID, idgen.New("run"), now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("second record: %v", err)
	}
	if !handled {
		t.Fatal("同一时刻重复记录必须被判为已处理")
	}

	runs, err := store.ListScheduleRuns(ctx, schedule.ID, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 || runs[0].Result != domain.RunMissed {
		t.Fatalf("unexpected runs: %+v", runs)
	}
}

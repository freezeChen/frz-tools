package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

const scheduleColumns = `id, name, enabled, kind, cron, interval_seconds, timezone, resource,
	operation_kind, spec_json, missed_run_policy, next_run_at, last_run_at, last_result,
	created_at, updated_at, created_by`

const scheduleRunColumns = `id, schedule_id, scheduled_for, started_at, finished_at,
	result, operation_id, error_code, error_message`

func (s *Store) CreateSchedule(ctx context.Context, schedule *domain.Schedule) error {
	if err := schedule.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO schedules (
			id, name, enabled, kind, cron, interval_seconds, timezone, resource, operation_kind,
			spec_json, missed_run_policy, next_run_at, last_run_at, last_result, created_at, updated_at, created_by
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '', ?, ?, ?)`,
		schedule.ID, schedule.Name, boolToInt(schedule.Enabled), string(schedule.Kind),
		nullString(schedule.Cron), intervalSeconds(schedule.Interval), schedule.Timezone,
		schedule.Resource, schedule.OperationKindOrDefault(), string(schedule.Spec),
		string(schedule.MissedRunPolicy),
		nullTime(schedule.NextRunAt), formatTime(schedule.CreatedAt), formatTime(schedule.UpdatedAt),
		nullString(schedule.CreatedBy))
	if isUniqueViolation(err) {
		return domain.NewError(v1.CodeScheduleInvalid, "计划名称 %q 已存在", schedule.Name)
	}
	return err
}

// GetSchedule 同时接受不透明 ID 与计划名称，便于 CLI 直接用名称引用。
func (s *Store) GetSchedule(ctx context.Context, ref string) (*domain.Schedule, error) {
	schedule, err := scanSchedule(s.db.QueryRowContext(ctx,
		`SELECT `+scheduleColumns+` FROM schedules WHERE id = ? OR name = ?`, ref, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NewError(v1.CodeScheduleNotFound, "计划 %q 不存在", ref)
	}
	if err != nil {
		return nil, err
	}
	return schedule, nil
}

func (s *Store) ListSchedules(ctx context.Context, limit int) ([]domain.Schedule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+scheduleColumns+` FROM schedules ORDER BY created_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSchedules(rows)
}

// ListEnabledSchedules 供调度循环启动时载入。只取启用中的计划，
// 停用的计划不应当占用任何调度算力。
func (s *Store) ListEnabledSchedules(ctx context.Context) ([]domain.Schedule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+scheduleColumns+` FROM schedules WHERE enabled = 1 ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSchedules(rows)
}

func (s *Store) SetScheduleEnabled(ctx context.Context, id string, enabled bool, now time.Time) (*domain.Schedule, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE schedules SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), formatTime(now), id)
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, domain.NewError(v1.CodeScheduleNotFound, "计划 %q 不存在", id)
	}
	return s.GetSchedule(ctx, id)
}

func (s *Store) UpdateScheduleProgress(ctx context.Context, id string, progress domain.ScheduleProgress, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE schedules SET next_run_at = ?, last_run_at = ?, last_result = ?, updated_at = ?
		WHERE id = ?`,
		nullTime(progress.NextRunAt), nullTime(progress.LastRunAt),
		nullString(progress.LastResult), formatTime(now), id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return domain.NewError(v1.CodeScheduleNotFound, "计划 %q 不存在", id)
	}
	return nil
}

func (s *Store) DeleteSchedule(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return domain.NewError(v1.CodeScheduleNotFound, "计划 %q 不存在", id)
	}
	return nil
}

// DispatchScheduledRun 在一个事务内落一条 ScheduleRun，并决定这次触发的结果：
// 资源空闲就创建 Operation 记为 dispatched，资源被占用就记为 failed/LOCK_BUSY
// 且不排队。同一 (schedule_id, scheduled_for) 已存在时返回 alreadyHandled=true
// 且不做任何改动——这是重启后重算触发时间不会重复执行的保证。
func (s *Store) DispatchScheduledRun(ctx context.Context, in domain.ScheduledDispatch) (domain.ScheduleRun, bool, error) {
	run := domain.ScheduleRun{
		ID:           in.RunID,
		ScheduleID:   in.ScheduleID,
		ScheduledFor: in.ScheduledFor,
		StartedAt:    in.Now,
		Result:       domain.RunDispatched,
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return run, false, err
	}
	defer tx.Rollback()

	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM schedule_runs WHERE schedule_id = ? AND scheduled_for = ?`,
		in.ScheduleID, formatTime(in.ScheduledFor)).Scan(&existing)
	switch {
	case err == nil:
		return run, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return run, false, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO schedule_runs (id, schedule_id, scheduled_for, started_at, finished_at,
			result, operation_id, error_code, error_message)
		VALUES (?, ?, ?, ?, NULL, ?, NULL, NULL, NULL)`,
		in.RunID, in.ScheduleID, formatTime(in.ScheduledFor), formatTime(in.Now), string(domain.RunDispatched))
	if isUniqueViolation(err) {
		// 并发的另一次派发先落库，视作已处理。
		return run, true, nil
	}
	if err != nil {
		return run, false, err
	}

	if in.Operation != nil {
		busy, err := hasIncompleteOperation(ctx, tx, in.Operation.Resource)
		if err != nil {
			return run, false, err
		}
		if busy {
			run.Result = domain.RunFailed
			run.ErrorCode = string(v1.CodeLockBusy)
			run.ErrorMessage = "resource " + in.Operation.Resource + " 正在执行，本次触发不排队"
			finished := in.Now
			run.FinishedAt = &finished
			if _, err := tx.ExecContext(ctx, `
				UPDATE schedule_runs SET result = ?, finished_at = ?, error_code = ?, error_message = ?
				WHERE id = ?`,
				string(run.Result), formatTime(in.Now), run.ErrorCode, run.ErrorMessage, in.RunID); err != nil {
				return run, false, err
			}
		} else {
			if err := insertOperation(ctx, tx, in.Operation); err != nil {
				return run, false, err
			}
			run.OperationID = in.Operation.ID
			if _, err := tx.ExecContext(ctx,
				`UPDATE schedule_runs SET operation_id = ? WHERE id = ?`,
				in.Operation.ID, in.RunID); err != nil {
				return run, false, err
			}
			if err := insertAudit(ctx, tx, domain.AuditEvent{
				EventType:   domain.EventOperationCreated,
				Actor:       in.Operation.CreatedBy,
				OperationID: in.Operation.ID,
				Resource:    in.Operation.Resource,
				Result:      string(domain.StatusPending),
				Time:        in.Now,
				Details:     map[string]string{"scheduleId": in.ScheduleID, "scheduledFor": formatTime(in.ScheduledFor)},
			}); err != nil {
				return run, false, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return run, false, err
	}
	return run, false, nil
}

// RecordMissedRun 记录一个被策略判定为不该补跑的触发时刻。
func (s *Store) RecordMissedRun(ctx context.Context, scheduleID, runID string, scheduledFor, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO schedule_runs (id, schedule_id, scheduled_for, started_at, finished_at,
			result, operation_id, error_code, error_message)
		VALUES (?, ?, ?, ?, ?, ?, NULL, NULL, NULL)`,
		runID, scheduleID, formatTime(scheduledFor), formatTime(now), formatTime(now),
		string(domain.RunMissed))
	if isUniqueViolation(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 0, nil
}

func (s *Store) ListScheduleRuns(ctx context.Context, scheduleID string, limit int) ([]domain.ScheduleRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+scheduleRunColumns+` FROM schedule_runs
		WHERE schedule_id = ?
		ORDER BY scheduled_for DESC, id DESC
		LIMIT ?`, scheduleID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []domain.ScheduleRun
	for rows.Next() {
		run, err := scanScheduleRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, *run)
	}
	return runs, rows.Err()
}

func scanSchedules(rows *sql.Rows) ([]domain.Schedule, error) {
	var schedules []domain.Schedule
	for rows.Next() {
		schedule, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		schedules = append(schedules, *schedule)
	}
	return schedules, rows.Err()
}

func scanSchedule(sc scanner) (*domain.Schedule, error) {
	var (
		schedule   domain.Schedule
		enabled    int
		kind       string
		cronExpr   sql.NullString
		interval   sql.NullInt64
		policy     string
		nextRunAt  sql.NullString
		lastRunAt  sql.NullString
		lastResult sql.NullString
		createdAt  string
		updatedAt  string
		createdBy  sql.NullString
		spec       string
		opKind     sql.NullString
	)
	if err := sc.Scan(&schedule.ID, &schedule.Name, &enabled, &kind, &cronExpr, &interval,
		&schedule.Timezone, &schedule.Resource, &opKind, &spec, &policy, &nextRunAt, &lastRunAt,
		&lastResult, &createdAt, &updatedAt, &createdBy); err != nil {
		return nil, err
	}
	// 老行没有这一列时取默认值：省略该字段的计划行为逐字节不变。
	if opKind.Valid && opKind.String != "" {
		schedule.OperationKind = opKind.String
	} else {
		schedule.OperationKind = v1.KindExecutorCommand
	}

	schedule.Enabled = enabled != 0
	schedule.Kind = domain.ScheduleKind(kind)
	schedule.Cron = cronExpr.String
	if interval.Valid {
		schedule.Interval = time.Duration(interval.Int64) * time.Second
	}
	schedule.Spec = []byte(spec)
	schedule.MissedRunPolicy = domain.MissedRunPolicy(policy)
	schedule.LastResult = lastResult.String
	schedule.CreatedBy = createdBy.String

	var err error
	if schedule.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if schedule.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	if nextRunAt.Valid {
		t, err := parseTime(nextRunAt.String)
		if err != nil {
			return nil, err
		}
		schedule.NextRunAt = &t
	}
	if lastRunAt.Valid {
		t, err := parseTime(lastRunAt.String)
		if err != nil {
			return nil, err
		}
		schedule.LastRunAt = &t
	}
	return &schedule, nil
}

func scanScheduleRun(sc scanner) (*domain.ScheduleRun, error) {
	var (
		run         domain.ScheduleRun
		scheduledAt string
		startedAt   string
		finishedAt  sql.NullString
		result      string
		operationID sql.NullString
		errorCode   sql.NullString
		errorMsg    sql.NullString
	)
	if err := sc.Scan(&run.ID, &run.ScheduleID, &scheduledAt, &startedAt, &finishedAt,
		&result, &operationID, &errorCode, &errorMsg); err != nil {
		return nil, err
	}

	run.Result = domain.ScheduleRunResult(result)
	run.OperationID = operationID.String
	run.ErrorCode = errorCode.String
	run.ErrorMessage = errorMsg.String

	var err error
	if run.ScheduledFor, err = parseTime(scheduledAt); err != nil {
		return nil, err
	}
	if run.StartedAt, err = parseTime(startedAt); err != nil {
		return nil, err
	}
	if finishedAt.Valid {
		t, err := parseTime(finishedAt.String)
		if err != nil {
			return nil, err
		}
		run.FinishedAt = &t
	}
	return &run, nil
}

func intervalSeconds(interval time.Duration) any {
	if interval <= 0 {
		return nil
	}
	return int64(interval / time.Second)
}

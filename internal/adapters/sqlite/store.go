package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/domain"

	_ "modernc.org/sqlite"
)

const timeLayout = time.RFC3339Nano

const operationColumns = `id, kind, resource, status, phase, dry_run, idempotency_key, request_hash,
	retry_of, exit_code, error_code, error_message, spec_json, created_at, started_at, finished_at, created_by`

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create database file: %w", err)
	}
	file.Close()

	// _txlock=immediate 避免读写事务升级时出现 SQLITE_BUSY；连接池限制为单连接
	// 则在此基础上进一步串行化访问。
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=busy_timeout(5000)" +
		"&_txlock=immediate"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	return &Store{db: db}, nil
}

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) Migrate(ctx context.Context) error { return Migrate(ctx, s.db) }

func (s *Store) JournalMode(ctx context.Context) (string, error) {
	var mode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return "", err
	}
	return mode, nil
}

func (s *Store) ForeignKeysEnabled(ctx context.Context) (bool, error) {
	var enabled int
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); err != nil {
		return false, err
	}
	return enabled == 1, nil
}

func (s *Store) CreateOperation(ctx context.Context, op *domain.Operation) (*domain.CreateResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if op.IdempotencyKey != "" {
		existing, err := getOperationByKey(ctx, tx, op.IdempotencyKey)
		switch {
		case err == nil:
			if existing.RequestHash != op.RequestHash {
				return nil, domain.NewError(v1.CodeIdempotencyConflict,
					"idempotency key %q was already used with a different request", op.IdempotencyKey).
					WithDetail("operationId", existing.ID)
			}
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return &domain.CreateResult{Operation: existing, Created: false}, nil
		case !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
	}

	incomplete, err := hasIncompleteOperation(ctx, tx, op.Resource)
	if err != nil {
		return nil, err
	}
	if incomplete {
		return nil, domain.NewError(v1.CodeLockBusy,
			"resource %q already has an incomplete operation", op.Resource)
	}

	if err := insertOperation(ctx, tx, op); err != nil {
		return nil, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		EventType:   domain.EventOperationCreated,
		Actor:       op.CreatedBy,
		OperationID: op.ID,
		Resource:    op.Resource,
		Result:      string(op.Status),
		Time:        op.CreatedAt,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &domain.CreateResult{Operation: op, Created: true}, nil
}

func (s *Store) GetOperation(ctx context.Context, id string) (*domain.Operation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+operationColumns+` FROM operations WHERE id = ?`, id)
	op, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NewError(v1.CodeOperationNotFound, "operation %q not found", id)
	}
	if err != nil {
		return nil, err
	}
	return op, nil
}

// ClaimNextPending 在同一事务内获取资源锁，并把最旧的、可运行的 pending 操作
// 推进为 running。没有可运行的操作时返回 (nil, nil)，例如所有 pending 操作的
// 资源当前都被占用。
func (s *Store) ClaimNextPending(ctx context.Context, now time.Time) (*domain.Operation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var id string
	err = tx.QueryRowContext(ctx, `
		SELECT o.id FROM operations o
		WHERE o.status = 'pending'
		  AND NOT EXISTS (
		    SELECT 1 FROM resource_locks l
		    WHERE l.resource = o.resource AND l.released_at IS NULL
		  )
		ORDER BY o.created_at, o.id
		LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	op, err := loadOperation(ctx, tx, id)
	if err != nil {
		return nil, err
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO resource_locks (resource, owner_operation_id, acquired_at, released_at)
		VALUES (?, ?, ?, NULL)
		ON CONFLICT (resource) DO UPDATE SET
			owner_operation_id = excluded.owner_operation_id,
			acquired_at = excluded.acquired_at,
			released_at = NULL
		WHERE resource_locks.released_at IS NOT NULL`,
		op.Resource, op.ID, formatTime(now))
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, nil
	}

	res, err = tx.ExecContext(ctx,
		`UPDATE operations SET status = ?, phase = ?, started_at = ? WHERE id = ? AND status = ?`,
		string(domain.StatusRunning), domain.PhaseExecute, formatTime(now), op.ID, string(domain.StatusPending))
	if err != nil {
		return nil, err
	}
	affected, err = res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, nil
	}

	if err := insertAudit(ctx, tx, domain.AuditEvent{
		EventType:   domain.EventLockAcquired,
		OperationID: op.ID,
		Resource:    op.Resource,
		Result:      string(domain.StatusRunning),
		Time:        now,
	}); err != nil {
		return nil, err
	}
	if err := insertLog(ctx, tx, domain.LogEntry{
		OperationID: op.ID,
		Level:       "info",
		Phase:       domain.PhaseExecute,
		Message:     "operation started",
		Time:        now,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	op.Status = domain.StatusRunning
	op.Phase = domain.PhaseExecute
	started := now
	op.StartedAt = &started
	return op, nil
}

// Finish 把 running 操作推进到终态并释放其资源锁。若操作已经不在 running
// 状态，则为空操作。
func (s *Store) Finish(ctx context.Context, in domain.FinishInput, now time.Time) (*domain.Operation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	op, err := loadOperation(ctx, tx, in.OperationID)
	if err != nil {
		return nil, err
	}
	if op.Status != domain.StatusRunning {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return op, nil
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE operations
		SET status = ?, phase = ?, exit_code = ?, error_code = ?, error_message = ?, finished_at = ?
		WHERE id = ? AND status = ?`,
		string(in.Status), domain.PhaseFinalize,
		nullableInt(in.ExitCode), nullString(in.ErrorCode), nullString(in.ErrorMessage),
		formatTime(now), in.OperationID, string(domain.StatusRunning))
	if err != nil {
		return nil, err
	}
	if affected, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if affected == 0 {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return op, nil
	}

	if err := releaseLock(ctx, tx, op.Resource, op.ID, now); err != nil {
		return nil, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		EventType:   terminalEvent(in.Status),
		OperationID: op.ID,
		Resource:    op.Resource,
		Result:      string(in.Status),
		Time:        now,
		Details:     errorDetail(in.ErrorCode),
	}); err != nil {
		return nil, err
	}
	if err := insertLog(ctx, tx, domain.LogEntry{
		OperationID: op.ID,
		Level:       logLevel(in.Status),
		Phase:       domain.PhaseFinalize,
		Message:     finishMessage(in),
		Fields:      errorDetail(in.ErrorCode),
		Time:        now,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	op.Status = in.Status
	op.Phase = domain.PhaseFinalize
	op.ExitCode = in.ExitCode
	op.ErrorCode = in.ErrorCode
	op.ErrorMessage = in.ErrorMessage
	finished := now
	op.FinishedAt = &finished
	return op, nil
}

// CancelPending 取消尚未开始执行的操作，并返回是否真的取消了。running 操作
// 由 worker 通过 context 取消。
func (s *Store) CancelPending(ctx context.Context, id string, now time.Time) (*domain.Operation, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	op, err := loadOperation(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}
	if op.Status != domain.StatusPending {
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return op, false, nil
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE operations SET status = ?, phase = ?, finished_at = ? WHERE id = ? AND status = ?`,
		string(domain.StatusCancelled), domain.PhaseFinalize, formatTime(now), id, string(domain.StatusPending))
	if err != nil {
		return nil, false, err
	}
	if affected, err := res.RowsAffected(); err != nil {
		return nil, false, err
	} else if affected == 0 {
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return op, false, nil
	}

	if err := insertAudit(ctx, tx, domain.AuditEvent{
		EventType:   domain.EventOperationCancelled,
		OperationID: op.ID,
		Resource:    op.Resource,
		Result:      string(domain.StatusCancelled),
		Time:        now,
	}); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}

	op.Status = domain.StatusCancelled
	op.Phase = domain.PhaseFinalize
	finished := now
	op.FinishedAt = &finished
	return op, true, nil
}

func (s *Store) AppendLog(ctx context.Context, entry domain.LogEntry) error {
	return insertLog(ctx, s.db, entry)
}

func (s *Store) AppendAudit(ctx context.Context, event domain.AuditEvent) error {
	return insertAudit(ctx, s.db, event)
}

func (s *Store) ListLogs(ctx context.Context, operationID string, afterID int64, limit int) ([]domain.LogEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, operation_id, level, ts, COALESCE(phase, ''), message, fields_json
		FROM operation_logs
		WHERE operation_id = ? AND id > ?
		ORDER BY id
		LIMIT ?`, operationID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []domain.LogEntry
	for rows.Next() {
		var (
			entry  domain.LogEntry
			ts     string
			fields sql.NullString
		)
		if err := rows.Scan(&entry.ID, &entry.OperationID, &entry.Level, &ts, &entry.Phase, &entry.Message, &fields); err != nil {
			return nil, err
		}
		parsed, err := parseTime(ts)
		if err != nil {
			return nil, err
		}
		entry.Time = parsed
		if fields.Valid && fields.String != "" {
			if err := json.Unmarshal([]byte(fields.String), &entry.Fields); err != nil {
				return nil, err
			}
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (s *Store) ListAudit(ctx context.Context, operationID string) ([]domain.AuditEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_type, COALESCE(actor, ''), COALESCE(operation_id, ''), COALESCE(resource, ''),
		       COALESCE(result, ''), ts, details_json
		FROM audit_events
		WHERE operation_id = ?
		ORDER BY id`, operationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []domain.AuditEvent
	for rows.Next() {
		var (
			event  domain.AuditEvent
			ts     string
			detail sql.NullString
		)
		if err := rows.Scan(&event.EventType, &event.Actor, &event.OperationID, &event.Resource,
			&event.Result, &ts, &detail); err != nil {
			return nil, err
		}
		parsed, err := parseTime(ts)
		if err != nil {
			return nil, err
		}
		event.Time = parsed
		if detail.Valid && detail.String != "" {
			if err := json.Unmarshal([]byte(detail.String), &event.Details); err != nil {
				return nil, err
			}
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) ActiveLock(ctx context.Context, resource string) (*domain.ResourceLock, error) {
	var (
		lock       domain.ResourceLock
		acquiredAt string
		releasedAt sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT resource, owner_operation_id, acquired_at, released_at
		FROM resource_locks
		WHERE resource = ? AND released_at IS NULL`, resource).
		Scan(&lock.Resource, &lock.OwnerOperationID, &acquiredAt, &releasedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lock.AcquiredAt, err = parseTime(acquiredAt)
	if err != nil {
		return nil, err
	}
	return &lock, nil
}

// RecoverRunning 把上一个守护进程实例遗留的 running 操作标记为失败，并释放
// 它们持有的锁。
func (s *Store) RecoverRunning(ctx context.Context, now time.Time) ([]domain.Operation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT `+operationColumns+` FROM operations WHERE status = ? ORDER BY created_at`, string(domain.StatusRunning))
	if err != nil {
		return nil, err
	}
	var stale []domain.Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		stale = append(stale, *op)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, op := range stale {
		if _, err := tx.ExecContext(ctx, `
			UPDATE operations
			SET status = ?, phase = ?, error_code = ?, error_message = ?, finished_at = ?
			WHERE id = ? AND status = ?`,
			string(domain.StatusFailed), domain.PhaseFinalize, string(v1.CodeDaemonRestarted),
			"daemon restarted while the operation was running", formatTime(now),
			op.ID, string(domain.StatusRunning)); err != nil {
			return nil, err
		}
		if err := releaseLock(ctx, tx, op.Resource, op.ID, now); err != nil {
			return nil, err
		}
		if err := insertAudit(ctx, tx, domain.AuditEvent{
			EventType:   domain.EventDaemonRecovered,
			OperationID: op.ID,
			Resource:    op.Resource,
			Result:      string(domain.StatusFailed),
			Time:        now,
			Details:     map[string]string{"errorCode": string(v1.CodeDaemonRestarted)},
		}); err != nil {
			return nil, err
		}
		if err := insertLog(ctx, tx, domain.LogEntry{
			OperationID: op.ID,
			Level:       "error",
			Phase:       domain.PhaseFinalize,
			Message:     "daemon restarted while the operation was running",
			Fields:      map[string]string{"errorCode": string(v1.CodeDaemonRestarted)},
			Time:        now,
		}); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return stale, nil
}

func (s *Store) CountByStatus(ctx context.Context, status domain.Status) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE status = ?`, string(status)).Scan(&n)
	return n, err
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func insertOperation(ctx context.Context, ex execer, op *domain.Operation) error {
	_, err := ex.ExecContext(ctx, `
		INSERT INTO operations (
			id, kind, resource, status, phase, dry_run, idempotency_key, request_hash, retry_of,
			exit_code, error_code, error_message, spec_json, created_at, started_at, finished_at, created_by
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID, op.Kind, op.Resource, string(op.Status), op.Phase, boolToInt(op.DryRun),
		nullString(op.IdempotencyKey), op.RequestHash, nullString(op.RetryOf),
		nullableInt(op.ExitCode), nullString(op.ErrorCode), nullString(op.ErrorMessage),
		string(op.Spec), formatTime(op.CreatedAt), nullTime(op.StartedAt), nullTime(op.FinishedAt),
		nullString(op.CreatedBy))
	return err
}

func insertAudit(ctx context.Context, ex execer, event domain.AuditEvent) error {
	var details any
	if len(event.Details) > 0 {
		encoded, err := json.Marshal(event.Details)
		if err != nil {
			return err
		}
		details = string(encoded)
	}
	_, err := ex.ExecContext(ctx, `
		INSERT INTO audit_events (event_type, actor, operation_id, resource, result, ts, details_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		event.EventType, nullString(event.Actor), nullString(event.OperationID),
		nullString(event.Resource), nullString(event.Result), formatTime(event.Time), details)
	return err
}

func insertLog(ctx context.Context, ex execer, entry domain.LogEntry) error {
	var fields any
	if len(entry.Fields) > 0 {
		encoded, err := json.Marshal(entry.Fields)
		if err != nil {
			return err
		}
		fields = string(encoded)
	}
	_, err := ex.ExecContext(ctx, `
		INSERT INTO operation_logs (operation_id, level, ts, phase, message, fields_json)
		VALUES (?, ?, ?, ?, ?, ?)`,
		entry.OperationID, entry.Level, formatTime(entry.Time), nullString(entry.Phase), entry.Message, fields)
	return err
}

func releaseLock(ctx context.Context, ex execer, resource, ownerID string, now time.Time) error {
	_, err := ex.ExecContext(ctx, `
		UPDATE resource_locks SET released_at = ?
		WHERE resource = ? AND owner_operation_id = ? AND released_at IS NULL`,
		formatTime(now), resource, ownerID)
	return err
}

// hasIncompleteOperation 判断 resource 上是否已经存在未到达终态的操作。
// 同一 resource 最多允许一个这样的操作，后续请求返回 LOCK_BUSY。
func hasIncompleteOperation(ctx context.Context, q queryer, resource string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `
		SELECT 1 FROM operations
		WHERE resource = ? AND status IN (?, ?)
		LIMIT 1`, resource, string(domain.StatusPending), string(domain.StatusRunning)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func loadOperation(ctx context.Context, q queryer, id string) (*domain.Operation, error) {
	op, err := scanOperation(q.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM operations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NewError(v1.CodeOperationNotFound, "operation %q not found", id)
	}
	return op, err
}

func getOperationByKey(ctx context.Context, q queryer, key string) (*domain.Operation, error) {
	return scanOperation(q.QueryRowContext(ctx,
		`SELECT `+operationColumns+` FROM operations WHERE idempotency_key = ?`, key))
}

type scanner interface {
	Scan(dest ...any) error
}

func scanOperation(sc scanner) (*domain.Operation, error) {
	var (
		op         domain.Operation
		status     string
		dryRun     int
		idem       sql.NullString
		retryOf    sql.NullString
		exitCode   sql.NullInt64
		errorCode  sql.NullString
		errorMsg   sql.NullString
		spec       string
		createdAt  string
		startedAt  sql.NullString
		finishedAt sql.NullString
		createdBy  sql.NullString
	)
	if err := sc.Scan(
		&op.ID, &op.Kind, &op.Resource, &status, &op.Phase, &dryRun,
		&idem, &op.RequestHash, &retryOf, &exitCode, &errorCode, &errorMsg,
		&spec, &createdAt, &startedAt, &finishedAt, &createdBy,
	); err != nil {
		return nil, err
	}
	op.Status = domain.Status(status)
	op.DryRun = dryRun != 0
	op.IdempotencyKey = idem.String
	op.RetryOf = retryOf.String
	op.ErrorCode = errorCode.String
	op.ErrorMessage = errorMsg.String
	op.CreatedBy = createdBy.String
	op.Spec = json.RawMessage(spec)
	if exitCode.Valid {
		code := int(exitCode.Int64)
		op.ExitCode = &code
	}
	var err error
	if op.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if startedAt.Valid {
		t, err := parseTime(startedAt.String)
		if err != nil {
			return nil, err
		}
		op.StartedAt = &t
	}
	if finishedAt.Valid {
		t, err := parseTime(finishedAt.String)
		if err != nil {
			return nil, err
		}
		op.FinishedAt = &t
	}
	return &op, nil
}

func terminalEvent(status domain.Status) string {
	switch status {
	case domain.StatusSucceeded:
		return domain.EventOperationSucceeded
	case domain.StatusCancelled:
		return domain.EventOperationCancelled
	default:
		return domain.EventOperationFailed
	}
}

func logLevel(status domain.Status) string {
	if status == domain.StatusSucceeded {
		return "info"
	}
	return "error"
}

func finishMessage(in domain.FinishInput) string {
	if in.ErrorCode != "" {
		return "operation finished with " + in.ErrorCode
	}
	return "operation " + string(in.Status)
}

func errorDetail(code string) map[string]string {
	if code == "" {
		return nil
	}
	return map[string]string{"errorCode": code}
}

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(value string) (time.Time, error) {
	return time.Parse(timeLayout, value)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

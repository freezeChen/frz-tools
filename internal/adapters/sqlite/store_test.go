package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/idgen"
	"github.com/freezeChen/frz-tools/migrations"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "opsd.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func newOperation(resource string) *domain.Operation {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &domain.Operation{
		ID:          idgen.NewOperationID(),
		Kind:        v1.KindExecutorCommand,
		Resource:    resource,
		Status:      domain.StatusPending,
		RequestHash: idgen.New("rh"),
		Spec:        json.RawMessage(`{"argv":["/usr/bin/true"]}`),
		CreatedAt:   now,
		CreatedBy:   "test",
	}
}

func TestMigrateConfiguresWALAndForeignKeys(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	mode, err := store.JournalMode(ctx)
	if err != nil {
		t.Fatalf("journal mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("want journal_mode wal, got %q", mode)
	}
	enabled, err := store.ForeignKeysEnabled(ctx)
	if err != nil {
		t.Fatalf("foreign keys: %v", err)
	}
	if !enabled {
		t.Fatal("foreign_keys must be enabled")
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	want, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}

	var applied int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != len(want) {
		t.Fatalf("want %d applied migrations, got %d", len(want), applied)
	}
}

func TestCreateOperationIdempotency(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	op := newOperation("demo")
	op.IdempotencyKey = "key-1"

	first, err := store.CreateOperation(ctx, op)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !first.Created {
		t.Fatal("first create must report Created=true")
	}

	duplicate := newOperation("demo")
	duplicate.IdempotencyKey = "key-1"
	duplicate.RequestHash = op.RequestHash

	second, err := store.CreateOperation(ctx, duplicate)
	if err != nil {
		t.Fatalf("duplicate create: %v", err)
	}
	if second.Created {
		t.Fatal("duplicate request must reuse the original operation")
	}
	if second.Operation.ID != first.Operation.ID {
		t.Fatalf("want reuse of %s, got %s", first.Operation.ID, second.Operation.ID)
	}

	conflicting := newOperation("demo")
	conflicting.IdempotencyKey = "key-1"
	conflicting.RequestHash = "different-hash"

	if _, err := store.CreateOperation(ctx, conflicting); domain.CodeOf(err) != v1.CodeIdempotencyConflict {
		t.Fatalf("want IDEMPOTENCY_CONFLICT, got %v", err)
	}
}

func TestSubmitRejectedWhileResourceLocked(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	running := newOperation("shared")
	if _, err := store.CreateOperation(ctx, running); err != nil {
		t.Fatalf("create: %v", err)
	}
	claimed, err := store.ClaimNextPending(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected to claim the pending operation")
	}

	if _, err := store.CreateOperation(ctx, newOperation("shared")); domain.CodeOf(err) != v1.CodeLockBusy {
		t.Fatalf("want LOCK_BUSY, got %v", err)
	}
}

func TestCreateRejectsResourceWithIncompleteOperation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first := newOperation("serial")
	first.CreatedAt = now.Add(-time.Second)
	if _, err := store.CreateOperation(ctx, first); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := store.CreateOperation(ctx, newOperation("serial")); domain.CodeOf(err) != v1.CodeLockBusy {
		t.Fatalf("want LOCK_BUSY while a pending operation exists, got %v", err)
	}

	claimed, err := store.ClaimNextPending(ctx, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ID != first.ID {
		t.Fatalf("expected to claim the pending operation, got %+v", claimed)
	}
	if _, err := store.CreateOperation(ctx, newOperation("serial")); domain.CodeOf(err) != v1.CodeLockBusy {
		t.Fatalf("want LOCK_BUSY while a running operation exists, got %v", err)
	}

	if _, err := store.Finish(ctx, domain.FinishInput{
		OperationID: first.ID,
		Status:      domain.StatusSucceeded,
	}, now); err != nil {
		t.Fatalf("finish: %v", err)
	}

	if _, err := store.CreateOperation(ctx, newOperation("serial")); err != nil {
		t.Fatalf("resource must be reusable once the operation is terminal: %v", err)
	}
}

func TestFinishReleasesLock(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	op := newOperation("release")
	if _, err := store.CreateOperation(ctx, op); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.ClaimNextPending(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if lock, err := store.ActiveLock(ctx, "release"); err != nil || lock == nil {
		t.Fatalf("expected an active lock, got %+v (%v)", lock, err)
	}

	exit := 0
	finished, err := store.Finish(ctx, domain.FinishInput{
		OperationID: op.ID,
		Status:      domain.StatusSucceeded,
		ExitCode:    &exit,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if finished.Status != domain.StatusSucceeded {
		t.Fatalf("want succeeded, got %s", finished.Status)
	}
	if lock, err := store.ActiveLock(ctx, "release"); err != nil || lock != nil {
		t.Fatalf("lock must be released, got %+v (%v)", lock, err)
	}
}

func TestRecoverRunningMarksFailedAndReleasesLocks(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	op := newOperation("crash")
	if _, err := store.CreateOperation(ctx, op); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.ClaimNextPending(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("claim: %v", err)
	}

	recovered, err := store.RecoverRunning(ctx, time.Now().UTC(), nil)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(recovered) != 1 || recovered[0].ID != op.ID {
		t.Fatalf("want the running operation recovered, got %+v", recovered)
	}

	stored, err := store.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status != domain.StatusFailed {
		t.Fatalf("want failed, got %s", stored.Status)
	}
	if stored.ErrorCode != string(v1.CodeDaemonRestarted) {
		t.Fatalf("want DAEMON_RESTARTED, got %q", stored.ErrorCode)
	}
	if lock, err := store.ActiveLock(ctx, "crash"); err != nil || lock != nil {
		t.Fatalf("lock must be released after recovery, got %+v (%v)", lock, err)
	}

	events, err := store.ListAudit(ctx, op.ID)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	found := false
	for _, e := range events {
		if e.EventType == domain.EventDaemonRecovered {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a daemon.recovered audit event")
	}
}

func TestGetOperationNotFound(t *testing.T) {
	store := newTestStore(t)
	_, err := store.GetOperation(context.Background(), "op_missing")
	if domain.CodeOf(err) != v1.CodeOperationNotFound {
		t.Fatalf("want OPERATION_NOT_FOUND, got %v", err)
	}
}

func TestCancelPendingOperation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	op := newOperation("cancel-me")
	if _, err := store.CreateOperation(ctx, op); err != nil {
		t.Fatalf("create: %v", err)
	}

	cancelled, ok, err := store.CancelPending(ctx, op.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !ok || cancelled.Status != domain.StatusCancelled {
		t.Fatalf("want cancelled=true and status cancelled, got %v %s", ok, cancelled.Status)
	}

	if _, ok, err := store.CancelPending(ctx, op.ID, time.Now().UTC()); err != nil || ok {
		t.Fatalf("second cancel must be a no-op, got ok=%v err=%v", ok, err)
	}
}

func TestLogsAreOrderedAndCursorable(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	op := newOperation("logs")
	now := time.Now().UTC()

	if _, err := store.CreateOperation(ctx, op); err != nil {
		t.Fatalf("create: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := store.AppendLog(ctx, domain.LogEntry{
			OperationID: op.ID,
			Level:       "info",
			Message:     "line",
			Phase:       domain.PhaseExecute,
			Time:        now,
		}); err != nil {
			t.Fatalf("append log: %v", err)
		}
	}

	all, err := store.ListLogs(ctx, op.ID, 0, 10)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 logs, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatal("logs must be ordered by id")
		}
	}

	tail, err := store.ListLogs(ctx, op.ID, all[0].ID, 10)
	if err != nil {
		t.Fatalf("list logs after cursor: %v", err)
	}
	if len(tail) != 2 {
		t.Fatalf("want 2 logs after cursor, got %d", len(tail))
	}
}

func TestUnknownErrorMapsToInternal(t *testing.T) {
	if domain.CodeOf(errors.New("boom")) != v1.CodeInternal {
		t.Fatal("uncoded error must map to INTERNAL")
	}
}

// SQLite 按**字符串**比较时间列（`ORDER BY created_at`、`not_before <= ?`），所以写入格式
// 必须让字符串序等于时间序。RFC3339Nano 不满足：它裁掉末尾的零、并在小数部分为零时把小数点
// 整段省略，于是同一秒内 "…T00:00:00Z" 按字典序**大于** "…T00:00:00.5Z"，而时间上更早。
// 本用例在修复前失败，修 timeLayout 后才通过——它是这次改动的起点。
func TestFormatTimeKeepsChronologicalOrder(t *testing.T) {
	second := time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC)
	cases := []struct {
		name           string
		earlier, later time.Time
	}{
		{"整秒在前、半秒在后", second, second.Add(500 * time.Millisecond)},
		{"整秒在前、一纳秒在后", second, second.Add(time.Nanosecond)},
		{"同秒内两个带小数的时刻", second.Add(500 * time.Millisecond), second.Add(900 * time.Millisecond)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.earlier.Before(tc.later) {
				t.Fatalf("用例本身写反了：%s 不早于 %s", tc.earlier, tc.later)
			}
			earlier, later := formatTime(tc.earlier), formatTime(tc.later)
			if earlier >= later {
				t.Fatalf("时间上更早的值必须按字典序也更小，got %q >= %q", earlier, later)
			}
		})
	}
}

// 窄写（定宽）之后仍必须读得回原值，且读回同样吃得下 0007 之前的可变宽度值——升级窗口里
// 两种格式会短暂共存，parseTime 不能只认其中一种。
func TestTimeRoundTripsThroughStorage(t *testing.T) {
	second := time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC)
	values := []time.Time{
		second,
		second.Add(time.Nanosecond),
		second.Add(120 * time.Millisecond),
		second.Add(500 * time.Millisecond),
		second.Add(123456000 * time.Nanosecond),
	}
	for _, want := range values {
		stored := formatTime(want)
		got, err := parseTime(stored)
		if err != nil {
			t.Fatalf("parseTime(%q): %v", stored, err)
		}
		if !got.Equal(want) {
			t.Fatalf("定宽格式必须读回原值：want %s, got %s（存储值 %q）", want, got, stored)
		}
		// 旧格式（RFC3339Nano）也必须读得动。
		legacy, err := parseTime(want.Format(time.RFC3339Nano))
		if err != nil {
			t.Fatalf("parseTime(旧格式 %q): %v", want.Format(time.RFC3339Nano), err)
		}
		if !legacy.Equal(want) {
			t.Fatalf("旧格式必须读回原值：want %s, got %s", want, legacy)
		}
	}
}

// 上面的缺陷对查询的实际影响：同一秒内提交的操作会被乱序领取。
func TestClaimNextPendingOrdersSameSecondOperationsByTime(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	second := time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC)

	older := newOperation("resource-older")
	older.CreatedAt = second
	newer := newOperation("resource-newer")
	newer.CreatedAt = second.Add(500 * time.Millisecond)

	// 先插新的：领取顺序若只是跟着插入顺序走，这个用例就证明了别的东西。
	for _, op := range []*domain.Operation{newer, older} {
		if _, err := store.CreateOperation(ctx, op); err != nil {
			t.Fatalf("create operation: %v", err)
		}
	}

	first, err := store.ClaimNextPending(ctx, second.Add(time.Second))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if first == nil || first.ID != older.ID {
		t.Fatalf("应当先领取时间更早的那条（%s），got %+v", older.ID, first)
	}

	secondClaim, err := store.ClaimNextPending(ctx, second.Add(time.Second))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if secondClaim == nil || secondClaim.ID != newer.ID {
		t.Fatalf("第二次应当领取时间更晚的那条（%s），got %+v", newer.ID, secondClaim)
	}
}

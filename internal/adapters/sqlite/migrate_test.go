package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/freezeChen/frz-tools/migrations"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "opsd.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func migrationFiles(t *testing.T) []string {
	t.Helper()
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	sort.Strings(files)
	return files
}

// applyMigrationsUpTo 复现「旧版本的库」：只应用 0001..max，并像 Migrate 那样记录
// 版本号。迁移的价值在升级路径上，只在全新库上验证等于没验证升级。
func applyMigrationsUpTo(t *testing.T, ctx context.Context, store *Store, max int) {
	t.Helper()
	if _, err := store.DB().ExecContext(ctx, schemaMigrationsDDL); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	for _, name := range migrationFiles(t) {
		version, err := migrationVersion(name)
		if err != nil {
			t.Fatalf("parse migration name %q: %v", name, err)
		}
		if version > max {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if err := applyMigration(ctx, store.DB(), version, string(body)); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
}

func tableExists(t *testing.T, store *Store, name string) bool {
	t.Helper()
	var found string
	err := store.DB().QueryRowContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	return true
}

func recordedVersions(t *testing.T, store *Store) []int {
	t.Helper()
	rows, err := store.DB().QueryContext(context.Background(),
		`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()

	var versions []int
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate versions: %v", err)
	}
	return versions
}

func TestMigrateApplies0004OnFreshDatabase(t *testing.T) {
	store := newTestStore(t)

	for _, table := range []string{"application_specs", "hosts", "environments"} {
		if !tableExists(t, store, table) {
			t.Fatalf("table %q must exist after migration", table)
		}
	}
	if versions := recordedVersions(t, store); !slices.Contains(versions, 4) {
		t.Fatalf("migration 0004 must be recorded, got %v", versions)
	}
}

// 0007 之前的库把时间列写成可变宽度（time.RFC3339Nano）：整秒无小数部分，其余按需裁零。
// 升级时必须把它们规范化成定宽，否则新旧行混在一起，字符串序仍然不等于时间序，
// schedule_runs 的 (schedule_id, scheduled_for) 去重也会按字符串比较而认不出同一个时刻。
func TestMigrateNormalizesLegacyTimestampWidth(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	applyMigrationsUpTo(t, ctx, store, 5)

	second := time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC)
	whole := second                            // 旧写法：没有小数部分 → "…T02:00:00Z"
	half := second.Add(500 * time.Millisecond) // 旧写法：".5"
	micro := second.Add(123456000 * time.Nanosecond)
	trimmed := second.Add(120 * time.Millisecond) // 旧写法：".12"（末尾的零被裁掉）

	// 两级插入顺序刻意与时间顺序相反：规范化之后的领取顺序若只是跟着插入顺序走，
	// 下面的领取断言就证明了别的东西。
	legacyOp := func(id, resource string, createdAt, startedAt, finishedAt time.Time) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx, `
			INSERT INTO operations (id, kind, resource, status, request_hash, spec_json,
				created_at, started_at, finished_at, not_before)
			VALUES (?, 'executor.command', ?, 'pending', ?, '{}', ?, ?, ?, NULL)`,
			id, resource, "rh_"+id,
			createdAt.Format(time.RFC3339Nano),
			startedAt.Format(time.RFC3339Nano),
			finishedAt.Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("insert legacy operation: %v", err)
		}
	}
	legacyOp("op_legacy_half", "legacy-half", half, trimmed, micro)
	legacyOp("op_legacy_whole", "legacy-whole", whole, whole, whole)

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("insert legacy row (%s): %v", query, err)
		}
	}
	legacy := func(tm time.Time) string { return tm.Format(time.RFC3339Nano) }

	exec(`INSERT INTO operation_logs (operation_id, level, ts, message) VALUES ('op_legacy_whole', 'info', ?, 'legacy line')`, legacy(half))
	exec(`INSERT INTO resource_locks (resource, owner_operation_id, acquired_at, released_at) VALUES ('legacy-whole', 'op_legacy_whole', ?, ?)`, legacy(whole), legacy(half))
	exec(`INSERT INTO audit_events (event_type, ts) VALUES ('operation.created', ?)`, legacy(whole))
	exec(`INSERT INTO artifacts (id, digest, size, created_at, deleted_at) VALUES ('art_legacy', 'sha256:aa', 1, ?, ?)`, legacy(whole), legacy(half))
	exec(`INSERT INTO artifacts (id, digest, size, created_at, deleted_at) VALUES ('art_legacy_live', 'sha256:bb', 1, ?, NULL)`, legacy(trimmed))
	exec(`INSERT INTO applications (id, name, created_at, updated_at) VALUES ('app_legacy', 'legacy', ?, ?)`, legacy(whole), legacy(half))
	exec(`INSERT INTO releases (id, application_id, artifact_id, version, created_at) VALUES ('rel_legacy', 'app_legacy', 'art_legacy', '1.0.0', ?)`, legacy(whole))
	exec(`INSERT INTO application_specs (application_id, spec_json, updated_at) VALUES ('app_legacy', '{}', ?)`, legacy(micro))
	exec(`INSERT INTO hosts (id, name, created_at, updated_at) VALUES ('host_legacy', 'legacy', ?, ?)`, legacy(whole), legacy(half))
	// 带时区偏移的值不是本工具的写入形态，迁移必须原样留着（按无偏移的表达式改写只会得到垃圾）。
	exec(`INSERT INTO hosts (id, name, created_at, updated_at) VALUES ('host_offset', 'offset', ?, ?)`,
		"2026-09-24T10:00:00+08:00", "2026-09-24T10:00:00+08:00")
	exec(`INSERT INTO environments (id, name, created_at, updated_at) VALUES ('env_legacy', 'legacy', ?, ?)`, legacy(whole), legacy(trimmed))
	exec(`INSERT INTO schedules (id, name, kind, resource, spec_json, created_at, updated_at, next_run_at, last_run_at)
		VALUES ('sch_legacy', 'legacy', 'interval', 'legacy-whole', '{}', ?, ?, ?, ?)`,
		legacy(whole), legacy(half), legacy(trimmed), legacy(micro))
	exec(`INSERT INTO schedule_runs (id, schedule_id, scheduled_for, started_at, finished_at, result)
		VALUES ('run_legacy', 'sch_legacy', ?, ?, ?, 'dispatched')`, legacy(whole), legacy(half), legacy(micro))
	// applied_at 由 applyMigration 写；这里把它改回旧写法，验证它也在迁移范围内。
	exec(`UPDATE schema_migrations SET applied_at = ? WHERE version = 5`, legacy(whole))

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 每一列都必须与写入侧（formatTime）逐字符一致。
	normalized := []struct {
		name  string
		query string
		want  time.Time
	}{
		{"operations.created_at", `SELECT created_at FROM operations WHERE id = 'op_legacy_whole'`, whole},
		{"operations.started_at", `SELECT started_at FROM operations WHERE id = 'op_legacy_half'`, trimmed},
		{"operations.finished_at", `SELECT finished_at FROM operations WHERE id = 'op_legacy_half'`, micro},
		{"operation_logs.ts", `SELECT ts FROM operation_logs WHERE operation_id = 'op_legacy_whole'`, half},
		{"resource_locks.acquired_at", `SELECT acquired_at FROM resource_locks WHERE resource = 'legacy-whole'`, whole},
		{"resource_locks.released_at", `SELECT released_at FROM resource_locks WHERE resource = 'legacy-whole'`, half},
		{"audit_events.ts", `SELECT ts FROM audit_events WHERE event_type = 'operation.created'`, whole},
		{"artifacts.created_at", `SELECT created_at FROM artifacts WHERE id = 'art_legacy'`, whole},
		{"artifacts.deleted_at", `SELECT deleted_at FROM artifacts WHERE id = 'art_legacy'`, half},
		{"artifacts.created_at(无删除时间的那条)", `SELECT created_at FROM artifacts WHERE id = 'art_legacy_live'`, trimmed},
		{"applications.created_at", `SELECT created_at FROM applications WHERE id = 'app_legacy'`, whole},
		{"applications.updated_at", `SELECT updated_at FROM applications WHERE id = 'app_legacy'`, half},
		{"releases.created_at", `SELECT created_at FROM releases WHERE id = 'rel_legacy'`, whole},
		{"application_specs.updated_at", `SELECT updated_at FROM application_specs WHERE application_id = 'app_legacy'`, micro},
		{"hosts.created_at", `SELECT created_at FROM hosts WHERE id = 'host_legacy'`, whole},
		{"hosts.updated_at", `SELECT updated_at FROM hosts WHERE id = 'host_legacy'`, half},
		{"environments.created_at", `SELECT created_at FROM environments WHERE id = 'env_legacy'`, whole},
		{"environments.updated_at", `SELECT updated_at FROM environments WHERE id = 'env_legacy'`, trimmed},
		{"schedules.created_at", `SELECT created_at FROM schedules WHERE id = 'sch_legacy'`, whole},
		{"schedules.updated_at", `SELECT updated_at FROM schedules WHERE id = 'sch_legacy'`, half},
		{"schedules.next_run_at", `SELECT next_run_at FROM schedules WHERE id = 'sch_legacy'`, trimmed},
		{"schedules.last_run_at", `SELECT last_run_at FROM schedules WHERE id = 'sch_legacy'`, micro},
		{"schedule_runs.scheduled_for", `SELECT scheduled_for FROM schedule_runs WHERE id = 'run_legacy'`, whole},
		{"schedule_runs.started_at", `SELECT started_at FROM schedule_runs WHERE id = 'run_legacy'`, half},
		{"schedule_runs.finished_at", `SELECT finished_at FROM schedule_runs WHERE id = 'run_legacy'`, micro},
		{"schema_migrations.applied_at", `SELECT applied_at FROM schema_migrations WHERE version = 5`, whole},
	}
	assertNormalized := func(t *testing.T) {
		t.Helper()
		for _, c := range normalized {
			var got string
			if err := store.DB().QueryRowContext(ctx, c.query).Scan(&got); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if got != formatTime(c.want) {
				t.Errorf("%s 必须是定宽格式：want %q, got %q", c.name, formatTime(c.want), got)
			}
		}
	}
	assertNormalized(t)

	// 整秒那条是这次缺陷的核心：旧写法 "…T02:00:00Z" 必须变成定宽（而不是被漏掉）。
	var created string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT created_at FROM operations WHERE id = 'op_legacy_whole'`).Scan(&created); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	if created != "2026-09-24T02:00:00.000000000Z" {
		t.Fatalf("整秒必须补零成定宽，got %q", created)
	}

	// NULL 仍是 NULL：迁移不能把「没有删除时间」变成「某个时间」。
	var deleted sql.NullString
	if err := store.DB().QueryRowContext(ctx,
		`SELECT deleted_at FROM artifacts WHERE id = 'art_legacy_live'`).Scan(&deleted); err != nil {
		t.Fatalf("read deleted_at: %v", err)
	}
	if deleted.Valid {
		t.Fatalf("NULL 的 deleted_at 必须保持 NULL，got %q", deleted.String)
	}

	// 带时区偏移的值原样保留（不是本工具写的形态）。
	var offset string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT created_at FROM hosts WHERE id = 'host_offset'`).Scan(&offset); err != nil {
		t.Fatalf("read offset created_at: %v", err)
	}
	if offset != "2026-09-24T10:00:00+08:00" {
		t.Fatalf("带偏移的值不该被改写，got %q", offset)
	}

	// 表达式是恒等变换：重放一次迁移体不得改变任何东西（因此不需要「跑过没有」的标记）。
	body, err := migrations.FS.ReadFile("0007_normalize_timestamp_width.sql")
	if err != nil {
		t.Fatalf("read 0007: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("replay 0007: %v", err)
	}
	assertNormalized(t)

	if versions := recordedVersions(t, store); !slices.Contains(versions, 7) {
		t.Fatalf("migration 0007 must be recorded, got %v", versions)
	}

	// 升级后的库必须真的按时间序领取：整秒那条（时间更早）先被领走。
	// 领取会改写 resource_locks，所以这一条放在所有规范化断言之后。
	claimed, err := store.ClaimNextPending(ctx, second.Add(time.Hour))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ID != "op_legacy_whole" {
		t.Fatalf("升级后应当先领取时间更早的那条（op_legacy_whole），got %+v", claimed)
	}
}

func TestMigrateUpgradesFrom0003(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	applyMigrationsUpTo(t, ctx, store, 3)

	if tableExists(t, store, "hosts") {
		t.Fatal("hosts must not exist before 0004, otherwise this test proves nothing")
	}

	// 升级前已有的数据：升级只能加表，不能丢表丢行。
	app := newApplication("billing-api")
	if err := store.CreateApplication(ctx, app); err != nil {
		t.Fatalf("create application: %v", err)
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// 已经是最新的库上重复执行必须是空操作。
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	versions := recordedVersions(t, store)
	if want := len(migrationFiles(t)); len(versions) != want {
		t.Fatalf("want %d applied migrations without duplicates, got %v", want, versions)
	}
	if !slices.Contains(versions, 4) {
		t.Fatalf("migration 0004 must be applied on upgrade, got %v", versions)
	}

	// 升级后的库必须真的能用：新表可写可读，旧数据原样在。
	host := newHost("local", "")
	if err := store.CreateHost(ctx, host); err != nil {
		t.Fatalf("create host after upgrade: %v", err)
	}
	if err := store.PutApplicationSpec(ctx, app.ID, mustJSON(t, newAppSpec(app.Name)), app.UpdatedAt, "alice"); err != nil {
		t.Fatalf("put spec after upgrade: %v", err)
	}
	if _, err := store.GetApplicationSpec(ctx, app.ID); err != nil {
		t.Fatalf("get spec after upgrade: %v", err)
	}

	found, err := store.GetApplication(ctx, app.Name)
	if err != nil {
		t.Fatalf("get application after upgrade: %v", err)
	}
	if found.ID != app.ID || found.Name != app.Name {
		t.Fatalf("pre-existing application must survive the upgrade, got %+v", found)
	}
	for _, table := range []string{"operations", "artifacts", "schedules", "schedule_runs"} {
		if !tableExists(t, store, table) {
			t.Fatalf("table %q must still exist after the upgrade", table)
		}
	}
}

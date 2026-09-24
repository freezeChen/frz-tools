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

package sqlite

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/idgen"
	"path/filepath"
	"strconv"
	"strings"
)

// newAppSpec 返回一份覆盖了所有字段的合法规格：迁移与仓储都不解释 JSON，
// 因此测试要能证明「写进去的字段全部原样读回来」，字段越全越有说服力。
func newAppSpec(application string) *domain.ApplicationSpec {
	return &domain.ApplicationSpec{
		APIVersion:  domain.ManifestAPIVersion,
		Kind:        domain.ManifestKind,
		Application: application,
		Runtime:     domain.RuntimeKindGo,
		Artifact: domain.SpecArtifact{
			ID:     "art_0123456789",
			Unpack: domain.SpecUnpack{Strategy: domain.UnpackTarGz, StrategyExplicit: true, StripComponents: 1},
		},
		Exec: domain.SpecExec{
			Argv:             []string{"/opt/billing-api/bin/billing-api", "--config", "/var/lib/billing-api/app.yaml"},
			WorkingDirectory: "/var/lib/billing-api",
			RunUser:          "billing-api",
			Environment:      map[string]string{"GOMEMLIMIT": "40MiB"},
			SecretEnvironment: map[string]domain.SecretRef{
				"DB_PASSWORD": {Kind: domain.SecretKindEnv, Name: "billing_db_password"},
				"TLS_KEY":     {Kind: domain.SecretKindFile, Name: "/etc/opsd/secrets/billing-tls.key"},
			},
			Ports: []int{8080, 9090},
		},
		Health: domain.SpecHealth{
			Readiness: domain.SpecReadiness{
				Type:                 domain.ReadinessHTTP,
				Target:               "http://127.0.0.1:8080/healthz",
				ConsecutiveSuccesses: 2,
			},
			StartTimeout: 60 * time.Second,
			StopTimeout:  30 * time.Second,
		},
		Logs:    domain.SpecLogs{Directory: "/var/log/billing-api"},
		Systemd: domain.SpecSystemd{UnitName: "billing-api.service", RestartPolicy: domain.RestartOnFailure},
	}
}

func newApplication(name string) *domain.Application {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &domain.Application{
		ID:        idgen.New("app"),
		Name:      name,
		Labels:    map[string]string{"team": "billing"},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func mustJSON(t *testing.T, spec *domain.ApplicationSpec) []byte {
	t.Helper()
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	return encoded
}

func countSpecs(t *testing.T, store *Store, applicationID string) int {
	t.Helper()
	var count int
	err := store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM application_specs WHERE application_id = ?`, applicationID).Scan(&count)
	if err != nil {
		t.Fatalf("count specs: %v", err)
	}
	return count
}

func TestPutApplicationSpecUpsertsAndOverwrites(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	app := newApplication("billing-api")
	if err := store.CreateApplication(ctx, app); err != nil {
		t.Fatalf("create application: %v", err)
	}

	spec := newAppSpec(app.Name)
	if err := store.PutApplicationSpec(ctx, app.ID, mustJSON(t, spec), app.UpdatedAt, "alice"); err != nil {
		t.Fatalf("put spec: %v", err)
	}

	// 同一份规格重复提交是幂等的：一个应用永远只有一份当前规格。
	if err := store.PutApplicationSpec(ctx, app.ID, mustJSON(t, spec), app.UpdatedAt, "alice"); err != nil {
		t.Fatalf("repeat put spec: %v", err)
	}
	if count := countSpecs(t, store, app.ID); count != 1 {
		t.Fatalf("want exactly one spec row, got %d", count)
	}

	stored, err := store.GetApplicationSpec(ctx, app.ID)
	if err != nil {
		t.Fatalf("get spec: %v", err)
	}
	if !reflect.DeepEqual(stored, spec) {
		t.Fatalf("spec round-trip mismatch:\n got %+v\nwant %+v", stored, spec)
	}

	// 覆盖：写一份改了端口与 unit 的规格，读回来的必须是新的那一份。
	updated := newAppSpec(app.Name)
	updated.Exec.Ports = []int{8081}
	updated.Systemd.UnitName = "billing-api-v2.service"
	updatedAt := app.UpdatedAt.Add(time.Minute)
	if err := store.PutApplicationSpec(ctx, app.ID, mustJSON(t, updated), updatedAt, "bob"); err != nil {
		t.Fatalf("overwrite spec: %v", err)
	}
	if count := countSpecs(t, store, app.ID); count != 1 {
		t.Fatalf("overwrite must not insert a second row, got %d", count)
	}

	stored, err = store.GetApplicationSpec(ctx, app.ID)
	if err != nil {
		t.Fatalf("get overwritten spec: %v", err)
	}
	if !reflect.DeepEqual(stored, updated) {
		t.Fatalf("overwritten spec mismatch:\n got %+v\nwant %+v", stored, updated)
	}

	var (
		raw       string
		updatedBy string
		stamp     string
	)
	if err := store.DB().QueryRowContext(ctx,
		`SELECT spec_json, updated_by, updated_at FROM application_specs WHERE application_id = ?`,
		app.ID).Scan(&raw, &updatedBy, &stamp); err != nil {
		t.Fatalf("read spec metadata: %v", err)
	}
	if raw != string(mustJSON(t, updated)) {
		t.Fatalf("spec_json must be stored verbatim:\n got %s\nwant %s", raw, mustJSON(t, updated))
	}
	if updatedBy != "bob" || stamp != formatTime(updatedAt) {
		t.Fatalf("want updated_by=bob updated_at=%s, got %s %s", formatTime(updatedAt), updatedBy, stamp)
	}
}

func TestGetApplicationSpecNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	app := newApplication("billing-api")
	if err := store.CreateApplication(ctx, app); err != nil {
		t.Fatalf("create application: %v", err)
	}

	if _, err := store.GetApplicationSpec(ctx, app.ID); domain.CodeOf(err) != v1.CodeSpecNotFound {
		t.Fatalf("want SPEC_NOT_FOUND, got %v", err)
	}
}

func TestPutApplicationSpecRequiresExistingApplication(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.PutApplicationSpec(ctx, "app_missing", mustJSON(t, newAppSpec("billing-api")),
		time.Now().UTC(), "alice")
	if domain.CodeOf(err) != v1.CodeApplicationNotFound {
		t.Fatalf("want APPLICATION_NOT_FOUND, got %v", err)
	}

	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM application_specs`).Scan(&count); err != nil {
		t.Fatalf("count specs: %v", err)
	}
	if count != 0 {
		t.Fatalf("rejected put must not write anything, got %d rows", count)
	}
}

// 规格有两层：应用级（release_id IS NULL）与每个 release 一份（迭代 3 D8）。
//
// 这条用例钉的是 SQLite 的一个陷阱：**NULL 互不相等**，因此
// `ON CONFLICT (application_id, release_id)` 对应用级那一行永远不触发——每写一次就多一行，
// 而读的时候只取第一行，看起来一切正常。迁移 0009 用偏索引把它钉死了，这里守住它。
func TestApplicationSpecKeepsOneRowPerLayer(t *testing.T) {
	store, ctx := newSpecStore(t)
	appID := seedApplication(t, store, ctx, "orders-api")

	for i := 0; i < 3; i++ {
		if err := store.PutApplicationSpec(ctx, appID, []byte(`{"v":`+itoa(i)+`}`), time.Now().UTC(), "tester"); err != nil {
			t.Fatalf("PutApplicationSpec #%d: %v", i, err)
		}
	}
	assertRowCount(t, store, appID, "1", `SELECT COUNT(*) FROM application_specs WHERE release_id IS NULL`)
	assertRowCount(t, store, appID, "0", `SELECT COUNT(*) FROM application_specs WHERE release_id IS NOT NULL`)

	// 每个 release 一份，互不覆盖。
	for _, releaseID := range []string{"rel_1", "rel_2"} {
		if err := store.PutApplicationSpecForRelease(ctx, appID, releaseID,
			[]byte(`{"application":"`+releaseID+`"}`), time.Now().UTC(), "tester"); err != nil {
			t.Fatalf("PutApplicationSpecForRelease %s: %v", releaseID, err)
		}
	}
	assertRowCount(t, store, appID, "2", `SELECT COUNT(*) FROM application_specs WHERE release_id IS NOT NULL`)

	// 读回来验证两层各自的内容。这里直接查原始 JSON：写进去的就是那段字节，
	// 用领域类型解出来只会忽略掉未知字段（那是另一条用例的事）。
	assertRawSpec(t, store, appID, "SELECT spec_json FROM application_specs WHERE application_id = ? AND release_id IS NULL", `{"v":2}`)
	assertRawSpec(t, store, appID, "SELECT spec_json FROM application_specs WHERE application_id = ? AND release_id = 'rel_1'", `{"application":"rel_1"}`)

	// GetApplicationSpec 必须**只**读应用级那一份：不加 release_id IS NULL 会取到任意一行，
	// 而「任意一行」在测试里往往恰好是对的。
	if _, err := store.GetApplicationSpec(ctx, appID); err != nil {
		t.Fatalf("GetApplicationSpec: %v", err)
	}
	if _, err := store.GetApplicationSpecForRelease(ctx, appID, "rel_1"); err != nil {
		t.Fatalf("GetApplicationSpecForRelease: %v", err)
	}

	// 不存在的 release：SPEC_NOT_FOUND，而不是「悄悄返回应用级那一份」。
	if _, err := store.GetApplicationSpecForRelease(ctx, appID, "rel_missing"); domain.CodeOf(err) != v1.CodeSpecNotFound {
		t.Fatalf("want SPEC_NOT_FOUND, got %v", err)
	}
}

// release 的状态机：每次转移都带 status 条件，因此重复调用幂等；激活是**两件事一个事务**
// （原来的 active 变 superseded、这个变 active）。
func TestReleaseStateMachine(t *testing.T) {
	store, ctx := newSpecStore(t)
	appID := seedApplication(t, store, ctx, "orders-api")
	artifactID := seedArtifact(t, store, ctx)

	now := time.Now().UTC()
	newRelease := func(id, version string) *domain.Release {
		release, err := store.CreateRelease(ctx, &domain.Release{
			ID: id, ApplicationID: appID, ArtifactID: artifactID,
			Version: version, CreatedAt: now, CreatedBy: "tester",
		})
		if err != nil {
			t.Fatalf("CreateRelease %s: %v", id, err)
		}
		return release
	}

	first := newRelease("rel_1", "1.0.0")
	if first.Status != domain.ReleaseCreated {
		t.Fatalf("新建的 release 状态应当是 created，got %q", first.Status)
	}

	if err := store.MarkReleaseDeploying(ctx, "rel_1", "/opt/opsd/apps/orders-api/releases/rel_1", now); err != nil {
		t.Fatalf("MarkReleaseDeploying: %v", err)
	}
	if _, err := store.ActivateRelease(ctx, "rel_1", now); err != nil {
		t.Fatalf("ActivateRelease: %v", err)
	}
	active, err := store.ActiveRelease(ctx, appID)
	if err != nil {
		t.Fatalf("ActiveRelease: %v", err)
	}
	if active == nil || active.ID != "rel_1" {
		t.Fatalf("当前激活的应当是 rel_1，got %+v", active)
	}

	// 第二个版本上线：上一个变 superseded，仍然可以回滚回去。
	newRelease("rel_2", "2.0.0")
	if err := store.MarkReleaseDeploying(ctx, "rel_2", "/opt/opsd/apps/orders-api/releases/rel_2", now); err != nil {
		t.Fatalf("MarkReleaseDeploying rel_2: %v", err)
	}
	superseded, err := store.ActivateRelease(ctx, "rel_2", now)
	if err != nil {
		t.Fatalf("ActivateRelease rel_2: %v", err)
	}
	if superseded != 1 {
		t.Fatalf("应当有一个版本被取代，got %d", superseded)
	}
	previous, err := store.GetRelease(ctx, "rel_1")
	if err != nil {
		t.Fatalf("GetRelease rel_1: %v", err)
	}
	if previous.Status != domain.ReleaseSuperseded || !previous.Status.Rollbackable() {
		t.Fatalf("被取代的版本应当仍可回滚，got %q", previous.Status)
	}

	// 同一应用最多一个 active。
	var activeCount int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM releases WHERE application_id = ? AND status = 'active'`, appID).
		Scan(&activeCount); err != nil {
		t.Fatalf("count active: %v", err)
	}
	if activeCount != 1 {
		t.Fatalf("同一应用最多一个 active，got %d", activeCount)
	}

	// 失败标记会清空目录（失败的 release 目录会被删掉，状态因此自洽）。
	newRelease("rel_3", "3.0.0")
	if err := store.MarkReleaseDeploying(ctx, "rel_3", "/opt/opsd/apps/orders-api/releases/rel_3", now); err != nil {
		t.Fatalf("MarkReleaseDeploying rel_3: %v", err)
	}
	if err := store.FailRelease(ctx, "rel_3", "RUNTIME_NOT_READY", "就绪超时", now); err != nil {
		t.Fatalf("FailRelease: %v", err)
	}
	failed, err := store.GetRelease(ctx, "rel_3")
	if err != nil {
		t.Fatalf("GetRelease rel_3: %v", err)
	}
	if failed.Status != domain.ReleaseFailed || failed.Directory != "" || failed.ErrorCode != "RUNTIME_NOT_READY" {
		t.Fatalf("失败的 release 状态不对: %+v", failed)
	}
	if failed.Status.Rollbackable() {
		t.Fatal("失败的版本不该可回滚（它从没跑起来过）")
	}

	// 保留策略清理非 active 的版本：active 的那个必须拒绝标记。
	if err := store.MarkReleaseRemoved(ctx, "rel_1", now); err != nil {
		t.Fatalf("MarkReleaseRemoved: %v", err)
	}
	if err := store.MarkReleaseRemoved(ctx, "rel_2", now); err != nil {
		t.Fatalf("对 active 的 MarkReleaseRemoved 不该报错（条件不满足即 0 行）: %v", err)
	}
	stillActive, err := store.GetRelease(ctx, "rel_2")
	if err != nil {
		t.Fatalf("GetRelease rel_2: %v", err)
	}
	if stillActive.Status != domain.ReleaseActive {
		t.Fatalf("active 的版本不该被标记为 removed，got %q", stillActive.Status)
	}
}

// ==== 迭代 3 的测试助手 ====

func newSpecStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "opsd.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store, ctx
}

func seedApplication(t *testing.T, store *Store, ctx context.Context, name string) string {
	t.Helper()
	app := &domain.Application{ID: "app_" + name, Name: name, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := store.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	return app.ID
}

func seedArtifact(t *testing.T, store *Store, ctx context.Context) string {
	t.Helper()
	artifact := &domain.Artifact{
		ID: "art_seed", Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
		Size: 1, CreatedAt: time.Now().UTC(),
	}
	if _, _, err := store.CreateArtifact(ctx, artifact); err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	return artifact.ID
}

func assertRowCount(t *testing.T, store *Store, appID, want, query string) {
	t.Helper()
	var count int
	if err := store.db.QueryRow(query).Scan(&count); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	if itoa(count) != want {
		t.Fatalf("行数不符（%s）：want %s, got %d", query, want, count)
	}
}

func itoa(value int) string { return strconv.Itoa(value) }

// assertRawSpec 直接查那一行里存的原始 JSON。
func assertRawSpec(t *testing.T, store *Store, appID, query, want string) {
	t.Helper()
	var raw string
	if err := store.db.QueryRow(query, appID).Scan(&raw); err != nil {
		t.Fatalf("query: %v", err)
	}
	if raw != want {
		t.Fatalf("规格内容不符：want %s, got %s", want, raw)
	}
}

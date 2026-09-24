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

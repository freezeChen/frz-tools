package application

import (
	"context"
	"reflect"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// validSpec 返回一份字段填满的合法规格，用于证明「存进去什么，读出来就是什么」。
func validSpec(application string) *domain.ApplicationSpec {
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
			Argv:             []string{"/opt/billing-api/bin/billing-api"},
			WorkingDirectory: "/var/lib/billing-api",
			RunUser:          "billing-api",
			Environment:      map[string]string{"GOMEMLIMIT": "40MiB"},
			SecretEnvironment: map[string]domain.SecretRef{
				"DB_PASSWORD": {Kind: domain.SecretKindEnv, Name: "billing_db_password"},
				"TLS_KEY":     {Kind: domain.SecretKindFile, Name: "/etc/opsd/secrets/billing-tls.key"},
			},
			Ports: []int{8080},
		},
		Health: domain.SpecHealth{
			Readiness: domain.SpecReadiness{
				Type:                 domain.ReadinessTCP,
				Target:               "127.0.0.1:8080",
				ConsecutiveSuccesses: 2,
			},
			StartTimeout: 60 * time.Second,
			StopTimeout:  30 * time.Second,
		},
		Logs:    domain.SpecLogs{Directory: "/var/log/billing-api"},
		Systemd: domain.SpecSystemd{UnitName: "billing-api.service", RestartPolicy: domain.RestartOnFailure},
	}
}

func TestPutSpecAndGetSpec(t *testing.T) {
	rt, _ := newTestRuntimeWith(t, Options{})
	ctx := context.Background()

	app, err := rt.Catalogs.CreateApplication(ctx, "billing-api", nil)
	if err != nil {
		t.Fatalf("create application: %v", err)
	}

	spec := validSpec(app.Name)
	stored, err := rt.Specs.PutSpec(ctx, app.Name, spec, "alice")
	if err != nil {
		t.Fatalf("put spec: %v", err)
	}
	if !reflect.DeepEqual(stored, spec) {
		t.Fatalf("put must return the stored spec:\n got %+v\nwant %+v", stored, spec)
	}

	// 名称与 ID 都能引用同一个应用。
	for _, ref := range []string{app.Name, app.ID} {
		found, err := rt.Specs.GetSpec(ctx, ref)
		if err != nil {
			t.Fatalf("get spec by %q: %v", ref, err)
		}
		if !reflect.DeepEqual(found, spec) {
			t.Fatalf("spec round-trip mismatch for %q:\n got %+v\nwant %+v", ref, found, spec)
		}
	}
}

func TestPutSpecOverwritesPreviousSpec(t *testing.T) {
	rt, _ := newTestRuntimeWith(t, Options{})
	ctx := context.Background()

	app, err := rt.Catalogs.CreateApplication(ctx, "billing-api", nil)
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	if _, err := rt.Specs.PutSpec(ctx, app.ID, validSpec(app.Name), "alice"); err != nil {
		t.Fatalf("put spec: %v", err)
	}

	updated := validSpec(app.Name)
	updated.Exec.Ports = []int{8081}
	updated.Systemd.RestartPolicy = domain.RestartAlways
	if _, err := rt.Specs.PutSpec(ctx, app.ID, updated, "bob"); err != nil {
		t.Fatalf("overwrite spec: %v", err)
	}

	found, err := rt.Specs.GetSpec(ctx, app.ID)
	if err != nil {
		t.Fatalf("get spec: %v", err)
	}
	if !reflect.DeepEqual(found, updated) {
		t.Fatalf("want the overwritten spec:\n got %+v\nwant %+v", found, updated)
	}
}

func TestGetSpecNotFound(t *testing.T) {
	rt, _ := newTestRuntimeWith(t, Options{})
	ctx := context.Background()

	app, err := rt.Catalogs.CreateApplication(ctx, "billing-api", nil)
	if err != nil {
		t.Fatalf("create application: %v", err)
	}

	if _, err := rt.Specs.GetSpec(ctx, app.ID); domain.CodeOf(err) != v1.CodeSpecNotFound {
		t.Fatalf("want SPEC_NOT_FOUND, got %v", err)
	}
}

func TestPutSpecRejectsInvalidManifest(t *testing.T) {
	rt, _ := newTestRuntimeWith(t, Options{})
	ctx := context.Background()

	app, err := rt.Catalogs.CreateApplication(ctx, "billing-api", nil)
	if err != nil {
		t.Fatalf("create application: %v", err)
	}

	invalid := validSpec("")
	if _, err := rt.Specs.PutSpec(ctx, app.ID, invalid, "alice"); domain.CodeOf(err) != v1.CodeManifestInvalid {
		t.Fatalf("want MANIFEST_INVALID for a missing application name, got %v", err)
	}
	if _, err := rt.Specs.PutSpec(ctx, app.ID, nil, "alice"); domain.CodeOf(err) != v1.CodeManifestInvalid {
		t.Fatalf("want MANIFEST_INVALID for a nil spec, got %v", err)
	}

	// 被拒绝的提交不得留下任何残留。
	if _, err := rt.Specs.GetSpec(ctx, app.ID); domain.CodeOf(err) != v1.CodeSpecNotFound {
		t.Fatalf("rejected put must not store anything, got %v", err)
	}
}

func TestPutSpecRequiresExistingApplication(t *testing.T) {
	rt, _ := newTestRuntimeWith(t, Options{})
	ctx := context.Background()

	if _, err := rt.Specs.PutSpec(ctx, "missing", validSpec("billing-api"), "alice"); domain.CodeOf(err) != v1.CodeApplicationNotFound {
		t.Fatalf("want APPLICATION_NOT_FOUND, got %v", err)
	}
	if _, err := rt.Specs.GetSpec(ctx, "missing"); domain.CodeOf(err) != v1.CodeApplicationNotFound {
		t.Fatalf("want APPLICATION_NOT_FOUND, got %v", err)
	}
}

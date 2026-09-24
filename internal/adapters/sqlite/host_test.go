package sqlite

import (
	"context"
	"reflect"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/idgen"
)

func newHost(name, address string) *domain.Host {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &domain.Host{
		ID:        idgen.New("host"),
		Name:      name,
		Address:   address,
		Labels:    map[string]string{"role": "edge"},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func newEnvironment(name string) *domain.Environment {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &domain.Environment{
		ID:        idgen.New("env"),
		Name:      name,
		Labels:    map[string]string{"tier": "prod"},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func TestCreateAndGetHost(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	host := newHost("edge-01", "10.0.0.9")
	if err := store.CreateHost(ctx, host); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, ref := range []string{host.ID, host.Name} {
		found, err := store.GetHost(ctx, ref)
		if err != nil {
			t.Fatalf("get %q: %v", ref, err)
		}
		if !reflect.DeepEqual(found, host) {
			t.Fatalf("host round-trip mismatch:\n got %+v\nwant %+v", found, host)
		}
	}

	if _, err := store.GetHost(ctx, "missing"); domain.CodeOf(err) != v1.CodeHostNotFound {
		t.Fatalf("want HOST_NOT_FOUND, got %v", err)
	}
}

func TestCreateHostRejectsDuplicateName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.CreateHost(ctx, newHost("edge-01", "10.0.0.9")); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 另一个 id、同名：唯一约束必须变成一个稳定的错误码，而不是裸的 SQLite 错误。
	err := store.CreateHost(ctx, newHost("edge-01", "10.0.0.10"))
	if domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}

	hosts, err := store.ListHosts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("conflicting insert must not be written, got %d hosts", len(hosts))
	}
}

func TestListHostsIsOrderedByCreation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Millisecond)
	first := newHost("first", "")
	first.CreatedAt, first.UpdatedAt = base, base
	local := newHost("local", "")
	local.CreatedAt, local.UpdatedAt = base.Add(time.Second), base.Add(time.Second)

	if err := store.CreateHost(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if err := store.CreateHost(ctx, local); err != nil {
		t.Fatalf("create local: %v", err)
	}

	hosts, err := store.ListHosts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hosts) != 2 || hosts[0].Name != "first" || hosts[1].Name != "local" {
		t.Fatalf("want [first local], got %+v", hosts)
	}
}

func TestFindLocalHost(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	remote := newHost("edge-01", "10.0.0.9")
	if err := store.CreateHost(ctx, remote); err != nil {
		t.Fatalf("create remote: %v", err)
	}

	// 远程主机不构成本机记录：没有 address 为空的行时必须返回 nil，让自举去创建。
	local, err := store.FindLocalHost(ctx)
	if err != nil {
		t.Fatalf("find local: %v", err)
	}
	if local != nil {
		t.Fatalf("remote host must not be treated as local: %+v", local)
	}

	created := newHost("local", "")
	if err := store.CreateHost(ctx, created); err != nil {
		t.Fatalf("create local: %v", err)
	}
	local, err = store.FindLocalHost(ctx)
	if err != nil {
		t.Fatalf("find local: %v", err)
	}
	if local == nil || local.ID != created.ID {
		t.Fatalf("want %s, got %+v", created.ID, local)
	}
}

func TestCreateAndGetEnvironment(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	environment := newEnvironment("staging")
	if err := store.CreateEnvironment(ctx, environment); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, ref := range []string{environment.ID, environment.Name} {
		found, err := store.GetEnvironment(ctx, ref)
		if err != nil {
			t.Fatalf("get %q: %v", ref, err)
		}
		if !reflect.DeepEqual(found, environment) {
			t.Fatalf("environment round-trip mismatch:\n got %+v\nwant %+v", found, environment)
		}
	}

	if _, err := store.GetEnvironment(ctx, "missing"); domain.CodeOf(err) != v1.CodeEnvNotFound {
		t.Fatalf("want ENVIRONMENT_NOT_FOUND, got %v", err)
	}
}

func TestCreateEnvironmentRejectsDuplicateName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.CreateEnvironment(ctx, newEnvironment("staging")); err != nil {
		t.Fatalf("create: %v", err)
	}
	err := store.CreateEnvironment(ctx, newEnvironment("staging"))
	if domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}

	environments, err := store.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(environments) != 1 {
		t.Fatalf("conflicting insert must not be written, got %d environments", len(environments))
	}
}

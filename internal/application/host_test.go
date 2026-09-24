package application

import (
	"context"
	"reflect"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

func newStoredHost(id, name, address string) *domain.Host {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &domain.Host{
		ID:        id,
		Name:      name,
		Address:   address,
		Labels:    map[string]string{"role": "edge"},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func TestEnsureLocalHostIsIdempotent(t *testing.T) {
	rt, store := newTestRuntimeWith(t, Options{})
	ctx := context.Background()

	first, err := rt.Hosts.EnsureLocalHost(ctx)
	if err != nil {
		t.Fatalf("ensure local host: %v", err)
	}
	if first.Name != localHostName || first.Address != "" {
		t.Fatalf("want the local host named %q with an empty address, got %+v", localHostName, first)
	}

	second, err := rt.Hosts.EnsureLocalHost(ctx)
	if err != nil {
		t.Fatalf("second ensure local host: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("bootstrap must reuse the existing record: %s != %s", second.ID, first.ID)
	}

	hosts, err := store.ListHosts(ctx)
	if err != nil {
		t.Fatalf("list hosts: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("want exactly one host record, got %d", len(hosts))
	}
}

func TestEnsureLocalHostKeepsExistingRecord(t *testing.T) {
	rt, store := newTestRuntimeWith(t, Options{})
	ctx := context.Background()

	// 运维可能给本机记录改了名字或打了标签，重启不得把它们覆盖回去。
	existing := newStoredHost("host_existing", "edge-01", "")
	if err := store.CreateHost(ctx, existing); err != nil {
		t.Fatalf("create host: %v", err)
	}

	local, err := rt.Hosts.EnsureLocalHost(ctx)
	if err != nil {
		t.Fatalf("ensure local host: %v", err)
	}
	if !reflect.DeepEqual(local, existing) {
		t.Fatalf("existing local host must be returned untouched:\n got %+v\nwant %+v", local, existing)
	}

	hosts, err := store.ListHosts(ctx)
	if err != nil {
		t.Fatalf("list hosts: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("want exactly one host record, got %d", len(hosts))
	}
}

func TestEnsureLocalHostSurvivesNameCollision(t *testing.T) {
	rt, store := newTestRuntimeWith(t, Options{})
	ctx := context.Background()

	// 远程主机占用了本机记录的固定名字：名字只是标签，自举不能因此失败。
	remote := newStoredHost("host_remote", localHostName, "10.0.0.9")
	if err := store.CreateHost(ctx, remote); err != nil {
		t.Fatalf("create remote host: %v", err)
	}

	local, err := rt.Hosts.EnsureLocalHost(ctx)
	if err != nil {
		t.Fatalf("ensure local host: %v", err)
	}
	if local.Address != "" || local.ID == remote.ID {
		t.Fatalf("want a new local record, got %+v", local)
	}
	if local.Name == localHostName {
		t.Fatalf("want a distinct name when %q is taken, got %q", localHostName, local.Name)
	}

	again, err := rt.Hosts.EnsureLocalHost(ctx)
	if err != nil {
		t.Fatalf("second ensure local host: %v", err)
	}
	if again.ID != local.ID {
		t.Fatalf("bootstrap must reuse the record: %s != %s", again.ID, local.ID)
	}

	hosts, err := store.ListHosts(ctx)
	if err != nil {
		t.Fatalf("list hosts: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("want the remote host plus one local record, got %d", len(hosts))
	}
}

func TestHostServiceCreatesHosts(t *testing.T) {
	rt, _ := newTestRuntimeWith(t, Options{})
	ctx := context.Background()

	host, err := rt.Hosts.CreateHost(ctx, " edge-01 ", "10.0.0.9", map[string]string{"role": "edge"})
	if err != nil {
		t.Fatalf("create host: %v", err)
	}
	if host.Name != "edge-01" {
		t.Fatalf("host name must be trimmed, got %q", host.Name)
	}

	found, err := rt.Hosts.GetHost(ctx, "edge-01")
	if err != nil {
		t.Fatalf("get host: %v", err)
	}
	if found.ID != host.ID || found.Address != "10.0.0.9" || found.Labels["role"] != "edge" {
		t.Fatalf("unexpected host: %+v", found)
	}

	if _, err := rt.Hosts.CreateHost(ctx, "edge-01", "", nil); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST for a duplicate name, got %v", err)
	}
	if _, err := rt.Hosts.CreateHost(ctx, "  ", "", nil); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST for an empty name, got %v", err)
	}
	if _, err := rt.Hosts.GetHost(ctx, "missing"); domain.CodeOf(err) != v1.CodeHostNotFound {
		t.Fatalf("want HOST_NOT_FOUND, got %v", err)
	}

	hosts, err := rt.Hosts.ListHosts(ctx)
	if err != nil {
		t.Fatalf("list hosts: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("want one host, got %d", len(hosts))
	}
}

func TestHostServiceCreatesEnvironments(t *testing.T) {
	rt, _ := newTestRuntimeWith(t, Options{})
	ctx := context.Background()

	environment, err := rt.Hosts.CreateEnvironment(ctx, " staging ", map[string]string{"tier": "prod"})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	if environment.Name != "staging" {
		t.Fatalf("environment name must be trimmed, got %q", environment.Name)
	}

	found, err := rt.Hosts.GetEnvironment(ctx, "staging")
	if err != nil {
		t.Fatalf("get environment: %v", err)
	}
	if found.ID != environment.ID || found.Labels["tier"] != "prod" {
		t.Fatalf("unexpected environment: %+v", found)
	}

	if _, err := rt.Hosts.CreateEnvironment(ctx, "staging", nil); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST for a duplicate name, got %v", err)
	}
	if _, err := rt.Hosts.GetEnvironment(ctx, "missing"); domain.CodeOf(err) != v1.CodeEnvNotFound {
		t.Fatalf("want ENVIRONMENT_NOT_FOUND, got %v", err)
	}

	environments, err := rt.Hosts.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("list environments: %v", err)
	}
	if len(environments) != 1 {
		t.Fatalf("want one environment, got %d", len(environments))
	}
}

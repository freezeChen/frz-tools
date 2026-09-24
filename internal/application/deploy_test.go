package application

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/release/local"
	"github.com/freezeChen/frz-tools/internal/adapters/sqlite"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// ==== 迭代 3b：部署与回滚 ====
//
// 这一组用例走的是**编排**：物化的顺序、切换与状态落库、失败后回到哪里。真实进程的启停
// 由 e2e（走 CLI + 真实的 opsd + proc 适配器）覆盖，真机由 test/host 覆盖。
//
// RuntimeAdapter 用假的（可注入失败）：同一包内的测试不能 import adapters/runtime/proc
// ——它依赖 application，会成环。ReleaseAdapter 用**真的**（local），因为它只依赖 domain，
// 而且「制品真的被解出来了」这件事只有真解一遍才证明得了。

// memStore 是应用层测试用的内存 StorageBackend。
//
// 同一包内的测试不能 import adapters/blob（它依赖 application），而部署流程确实需要
// 「把制品字节交出来」这一件事。
type memStore struct {
	mu    sync.Mutex
	blobs map[domain.Digest][]byte
}

func (m *memStore) Put(_ context.Context, r io.Reader, expected domain.Digest) (Stored, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Stored{}, err
	}
	digest, size, err := domain.DigestOf(bytes.NewReader(data))
	if err != nil {
		return Stored{}, err
	}
	if expected != "" && expected != digest {
		return Stored{}, domain.NewError(v1.CodeArtifactChecksum, "摘要不符")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[digest] = data
	return Stored{Digest: digest, Size: size}, nil
}

func (m *memStore) Open(_ context.Context, digest domain.Digest) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.blobs[digest]
	if !ok {
		return nil, domain.NewError(v1.CodeArtifactNotFound, "blob %s 不存在", digest)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *memStore) Stat(_ context.Context, digest domain.Digest) (Stored, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.blobs[digest]
	if !ok {
		return Stored{}, domain.NewError(v1.CodeArtifactNotFound, "blob %s 不存在", digest)
	}
	return Stored{Digest: digest, Size: int64(len(data))}, nil
}

func (m *memStore) Delete(_ context.Context, digest domain.Digest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blobs, digest)
	return nil
}

func (m *memStore) List(context.Context) ([]Stored, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Stored, 0, len(m.blobs))
	for digest, data := range m.blobs {
		out = append(out, Stored{Digest: digest, Size: int64(len(data))})
	}
	return out, nil
}

func testOwner(string) (int, int, error) { return os.Getuid(), os.Getgid(), nil }

type deployFixture struct {
	rt       *Runtime
	store    *sqlite.Store
	releases *local.Adapter
	adapter  *fakeRuntimeAdapter
	// root 是 release 目录的路径前缀（local.WithRoot），断言真实产物时用它拼。
	root string
}

func newDeployFixture(t *testing.T, adapter *fakeRuntimeAdapter) *deployFixture {
	t.Helper()

	root := t.TempDir()
	releases := local.New(local.WithRoot(root), local.WithOwnerResolver(testOwner))
	if adapter == nil {
		adapter = &fakeRuntimeAdapter{health: domain.RuntimeHealth{Ready: true}}
	}
	rt, store := newTestRuntimeWith(t, Options{
		Store:          &memStore{blobs: map[domain.Digest][]byte{}},
		ReleaseAdapter: releases,
		RuntimeAdapter: adapter,
	})
	return &deployFixture{rt: rt, store: store, releases: releases, adapter: adapter, root: root}
}

// upload 造一个单文件制品并上传（unpack.strategy=none + fileName 是部署最简单的形态）。
func (f *deployFixture) upload(t *testing.T, name string, content []byte) *domain.Artifact {
	t.Helper()
	return f.uploadWith(t, name, "application/octet-stream", content)
}

// uploadWith 指定 media type 上传一段字节：用来构造「制品本身有问题」的场景
// （例如声称是 gzip、给的却不是 gzip 的字节）。
func (f *deployFixture) uploadWith(t *testing.T, name, mediaType string, content []byte) *domain.Artifact {
	t.Helper()
	artifact, _, err := f.rt.Artifacts.Put(context.Background(), PutArtifactInput{
		Name: name, MediaType: mediaType, Body: bytes.NewReader(content),
	})
	if err != nil {
		t.Fatalf("上传制品 %s: %v", name, err)
	}
	return artifact
}

// uploadTar 造一个 tar 制品（一个 bin/app + 一个 README）。
func (f *deployFixture) uploadTar(t *testing.T, name string) *domain.Artifact {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, entry := range []struct {
		name string
		body string
		mode int64
	}{
		{"bin/app", "#!/bin/sh\necho " + name + "\n", 0o755},
		{"README", "release " + name + "\n", 0o644},
	} {
		if err := writer.WriteHeader(&tar.Header{
			Name: entry.name, Typeflag: tar.TypeReg, Mode: entry.mode,
			Size: int64(len(entry.body)), Format: tar.FormatPAX,
		}); err != nil {
			t.Fatalf("写 tar 头: %v", err)
		}
		if _, err := writer.Write([]byte(entry.body)); err != nil {
			t.Fatalf("写 tar 内容: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 tar: %v", err)
	}
	return f.upload(t, name, buf.Bytes())
}

// spec 造一份合法规格：默认是单文件制品（最简单），可注入改动。
func (f *deployFixture) spec(artifact *domain.Artifact, version string, mutate ...func(*domain.ApplicationSpec)) *domain.ApplicationSpec {
	spec := &domain.ApplicationSpec{
		APIVersion:  domain.ManifestAPIVersion,
		Kind:        domain.ManifestKind,
		Application: "orders-api",
		Runtime:     domain.RuntimeKindGo,
		Artifact: domain.SpecArtifact{
			ID: artifact.ID, Version: version, FileName: "bin/app",
			Unpack: domain.SpecUnpack{Strategy: domain.UnpackNone},
		},
		Exec: domain.SpecExec{
			Argv:             []string{"bin/app"},
			WorkingDirectory: "/var/lib/orders-api",
			RunUser:          "orders-api",
		},
		Health: domain.SpecHealth{
			Readiness:    domain.SpecReadiness{Type: domain.ReadinessTCP, Target: "127.0.0.1:18099", ConsecutiveSuccesses: 1},
			StartTimeout: time.Second,
		},
		Logs:      domain.SpecLogs{Directory: "/var/log/orders-api"},
		Resources: domain.SpecResources{},
		Release:   domain.SpecRelease{KeepLast: 3},
	}
	for _, apply := range mutate {
		apply(spec)
	}
	return spec
}

func (f *deployFixture) app(t *testing.T, name string) *domain.Application {
	t.Helper()
	app, err := f.rt.Catalogs.CreateApplication(context.Background(), name, nil)
	if err != nil {
		t.Fatalf("创建应用: %v", err)
	}
	return app
}

// deploy 走完整的一遍「解析 → 建 Operation → worker 执行」。
func (f *deployFixture) deploy(t *testing.T, app string, spec *domain.ApplicationSpec) *domain.Operation {
	t.Helper()
	ctx := context.Background()

	target, err := f.rt.Deploys.PrepareDeploy(ctx, app, spec, "tester")
	if err != nil {
		t.Fatalf("PrepareDeploy: %v", err)
	}
	if target.AlreadyActive {
		t.Fatal("这个版本不该已经是当前版本")
	}
	raw, err := json.Marshal(struct {
		ReleaseID string `json:"releaseId"`
	}{ReleaseID: target.Release.ID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	op, _, err := f.rt.Service.Create(ctx, v1.CreateOperationRequest{
		Kind: v1.KindAppDeploy, Resource: app, Spec: raw,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("worker: %v", err)
	}
	stored, err := f.rt.Service.Get(ctx, op.ID)
	if err != nil {
		t.Fatalf("get operation: %v", err)
	}
	return stored
}

// releaseDirOf 返回某个 release 在测试前缀下的真实目录。
func (f *deployFixture) releaseDirOf(app, releaseID string) string {
	return filepath.Join(f.root, "opt", "opsd", "apps", app, "releases", releaseID)
}

func (f *deployFixture) currentTarget(t *testing.T, app string) string {
	t.Helper()
	link := filepath.Join(f.root, "opt", "opsd", "apps", app, "releases", "current")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("读取 current 指针: %v", err)
	}
	return target
}

func (f *deployFixture) activeRelease(t *testing.T, app string) *domain.Release {
	t.Helper()
	application, err := f.rt.Catalogs.GetApplication(context.Background(), app)
	if err != nil {
		t.Fatalf("取应用: %v", err)
	}
	release, err := f.store.ActiveRelease(context.Background(), application.ID)
	if err != nil {
		t.Fatalf("取 active release: %v", err)
	}
	return release
}

// ==== 用例 ====

func TestDeployMaterializesActivatesAndRecords(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	artifact := f.upload(t, "orders-1.0.0", []byte("#!/bin/sh\necho 1.0.0\n"))

	op := f.deploy(t, app.Name, f.spec(artifact, "1.0.0"))
	if op.Status != domain.StatusSucceeded {
		t.Fatalf("部署应当成功，got %s (%s: %s)", op.Status, op.ErrorCode, op.ErrorMessage)
	}

	// 真实产物：解出来的文件在，且就是制品的内容。
	active := f.activeRelease(t, app.Name)
	if active == nil || active.Version != "1.0.0" || active.Status != domain.ReleaseActive {
		t.Fatalf("应当有一个 active 的 1.0.0，got %+v", active)
	}
	content, err := os.ReadFile(filepath.Join(f.releaseDirOf(app.Name, active.ID), "bin", "app"))
	if err != nil {
		t.Fatalf("读取解包出来的文件: %v", err)
	}
	if string(content) != "#!/bin/sh\necho 1.0.0\n" {
		t.Fatalf("制品内容不对: %q", content)
	}
	// current 指向它，而且用的是**相对名字**。
	if target := f.currentTarget(t, app.Name); target != active.ID {
		t.Fatalf("current 应当指向 %s，got %s", active.ID, target)
	}
	// 这一版采用的规格被单独记下来了（回滚要连配置一起回滚）。
	spec, err := f.store.GetApplicationSpecForRelease(context.Background(), app.ID, active.ID)
	if err != nil {
		t.Fatalf("取该 release 的规格: %v", err)
	}
	if spec.Artifact.Version != "1.0.0" {
		t.Fatalf("该 release 的规格不对: %+v", spec.Artifact)
	}
}

// 部署的编排顺序：物化 → 准备 → 启动 → 等待就绪。顺序错了会出现「先切上去再解包」
// 这类窗口，而那个窗口里的服务指向一个不存在的目录。
func TestDeployOrdersMaterializeBeforeStart(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	artifact := f.uploadTar(t, "orders-1.0.0")

	op := f.deploy(t, app.Name, f.spec(artifact, "1.0.0", func(spec *domain.ApplicationSpec) {
		spec.Artifact.FileName = ""
		spec.Artifact.Unpack = domain.SpecUnpack{Strategy: domain.UnpackTar}
		spec.Exec.Argv = []string{"bin/app"}
	}))
	if op.Status != domain.StatusSucceeded {
		t.Fatalf("部署应当成功，got %s (%s)", op.Status, op.ErrorMessage)
	}
	if calls := f.adapter.callNames(); len(calls) < 2 || calls[0] != "prepare" || calls[1] != "start" {
		t.Fatalf("准备必须先于启动，got %v", calls)
	}
	active := f.activeRelease(t, app.Name)
	if _, err := os.Stat(filepath.Join(f.releaseDirOf(app.Name, active.ID), "README")); err != nil {
		t.Fatalf("tar 里的 README 应当被解出来: %v", err)
	}
}

// 失败必须**回到上一个稳定版本**：current 切回去、旧版本仍在跑、新目录被删掉、
// release 记成 failed、Operation 报 DEPLOY_ROLLED_BACK。
func TestDeployFailureRollsBackToPrevious(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")

	first := f.upload(t, "orders-1.0.0", []byte("v1"))
	if op := f.deploy(t, app.Name, f.spec(first, "1.0.0")); op.Status != domain.StatusSucceeded {
		t.Fatalf("第一次部署应当成功: %s", op.ErrorMessage)
	}
	stable := f.activeRelease(t, app.Name)

	// 第二次部署：进程起来了但永远不就绪（假适配器的 health 保持 not-ready），
	// 启动超时 1 秒，因此这条用例大约耗时 1 秒。
	f.adapter.health = domain.RuntimeHealth{Ready: false, Detail: "端口没有监听"}
	second := f.upload(t, "orders-2.0.0", []byte("v2"))
	op := f.deploy(t, app.Name, f.spec(second, "2.0.0"))
	if op.Status != domain.StatusFailed {
		t.Fatalf("部署应当失败，got %s", op.Status)
	}
	if op.ErrorCode != string(v1.CodeDeployRolledBack) {
		t.Fatalf("want DEPLOY_ROLLED_BACK, got %s: %s", op.ErrorCode, op.ErrorMessage)
	}

	// current 回到上一个稳定版本，而且库里那个也回去了。
	if target := f.currentTarget(t, app.Name); target != stable.ID {
		t.Fatalf("current 应当回到 %s，got %s", stable.ID, target)
	}
	if active := f.activeRelease(t, app.Name); active == nil || active.ID != stable.ID {
		t.Fatalf("active 应当仍是 %s，got %+v", stable.ID, active)
	}

	// 失败的那一版：目录被删、状态是 failed、没有它自己的 active 痕迹。
	failed, err := f.rt.Catalogs.GetRelease(context.Background(), releaseIDOfVersion(t, f, app.Name, "2.0.0"))
	if err != nil {
		t.Fatalf("取失败的 release: %v", err)
	}
	if failed.Status != domain.ReleaseFailed {
		t.Fatalf("失败的版本状态应当是 failed，got %s", failed.Status)
	}
	if _, err := os.Stat(f.releaseDirOf(app.Name, failed.ID)); !os.IsNotExist(err) {
		t.Fatalf("失败版本的目录应当被删掉")
	}
}

// 第一次部署就失败：没有可回退的版本，只能停掉它并撤掉 current 指针——留着指针去指一个
// 马上要删掉的目录，会让「现在跑的是哪个版本」这句话指向不存在的东西。
func TestFirstDeployFailureTearsDown(t *testing.T) {
	f := newDeployFixture(t, &fakeRuntimeAdapter{health: domain.RuntimeHealth{Ready: false, Detail: "没监听"}})
	app := f.app(t, "orders-api")
	artifact := f.upload(t, "orders-1.0.0", []byte("v1"))

	op := f.deploy(t, app.Name, f.spec(artifact, "1.0.0"))
	if op.Status != domain.StatusFailed || op.ErrorCode != string(v1.CodeDeployRolledBack) {
		t.Fatalf("want DEPLOY_ROLLED_BACK，got %s / %s", op.Status, op.ErrorCode)
	}
	if _, err := os.Lstat(filepath.Join(f.root, "opt", "opsd", "apps", app.Name, "releases", "current")); !os.IsNotExist(err) {
		t.Fatal("第一次部署失败后不该留下 current 指针")
	}
	// 停过：失败路径里必须把它收掉，否则 unit 会一直重启一个起不来的东西。
	calls := f.adapter.callNames()
	if !containsString(calls, "stop") {
		t.Fatalf("第一次部署失败应当停掉它，got %v", calls)
	}
}

// 回滚**连配置一起回滚**：用的是目标版本当时那份 manifest，而不是"当前规格"。
func TestRollbackRestoresVersionAndItsSpec(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")

	first := f.upload(t, "orders-1.0.0", []byte("v1"))
	firstSpec := f.spec(first, "1.0.0", func(spec *domain.ApplicationSpec) {
		spec.Exec.Environment = map[string]string{"RELEASE": "1.0.0"}
	})
	if op := f.deploy(t, app.Name, firstSpec); op.Status != domain.StatusSucceeded {
		t.Fatalf("v1 部署应当成功: %s", op.ErrorMessage)
	}
	stable := f.activeRelease(t, app.Name)

	second := f.upload(t, "orders-2.0.0", []byte("v2"))
	secondSpec := f.spec(second, "2.0.0", func(spec *domain.ApplicationSpec) {
		spec.Exec.Environment = map[string]string{"RELEASE": "2.0.0"}
	})
	if op := f.deploy(t, app.Name, secondSpec); op.Status != domain.StatusSucceeded {
		t.Fatalf("v2 部署应当成功: %s", op.ErrorMessage)
	}

	// 回滚（不带 --to：回到上一个曾经激活过的版本）。
	target, err := f.rt.Deploys.PrepareRollback(context.Background(), app.Name, "")
	if err != nil {
		t.Fatalf("PrepareRollback: %v", err)
	}
	if target.Release.ID != stable.ID {
		t.Fatalf("回滚目标应当是 %s，got %s", stable.ID, target.Release.ID)
	}
	raw, err := json.Marshal(struct {
		ReleaseID string `json:"releaseId"`
	}{ReleaseID: target.Release.ID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	op, _, err := f.rt.Service.Create(context.Background(), v1.CreateOperationRequest{
		Kind: v1.KindAppRollback, Resource: app.Name, Spec: raw,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.rt.Pool.ProcessNext(context.Background()); err != nil {
		t.Fatalf("worker: %v", err)
	}
	finished, err := f.rt.Service.Get(context.Background(), op.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if finished.Status != domain.StatusSucceeded {
		t.Fatalf("回滚应当成功，got %s (%s)", finished.Status, finished.ErrorMessage)
	}

	if target := f.currentTarget(t, app.Name); target != stable.ID {
		t.Fatalf("回滚后 current 应当是 %s，got %s", stable.ID, target)
	}
	if active := f.activeRelease(t, app.Name); active == nil || active.ID != stable.ID {
		t.Fatalf("回滚后 active 应当是 %s，got %+v", stable.ID, active)
	}
	// 关键：适配器最后拿到的规格是 **v1 的**（配置也回去了），而不是 v2 的。
	last := f.adapter.lastSpec()
	if last == nil || last.Exec.Environment["RELEASE"] != "1.0.0" {
		t.Fatalf("回滚必须连配置一起回滚，适配器最后拿到的环境是 %v", last.Exec.Environment)
	}
}

func TestDeployIsIdempotentForActiveVersion(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	artifact := f.upload(t, "orders-1.0.0", []byte("v1"))
	spec := f.spec(artifact, "1.0.0")

	if op := f.deploy(t, app.Name, spec); op.Status != domain.StatusSucceeded {
		t.Fatalf("部署应当成功: %s", op.ErrorMessage)
	}

	// 同一版本再提交一次：什么都不做，也不产生 Operation。
	target, err := f.rt.Deploys.PrepareDeploy(context.Background(), app.Name, spec, "tester")
	if err != nil {
		t.Fatalf("重复部署不该报错: %v", err)
	}
	if !target.AlreadyActive {
		t.Fatal("要上的版本已经是当前版本，应当报告 AlreadyActive")
	}
}

// 同一个版本号下换制品是最难查的一类漂移：跑的是哪个版本的制品，谁也说不清。
func TestDeployRejectsVersionReuseWithDifferentArtifact(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")
	first := f.upload(t, "orders-1.0.0", []byte("v1"))
	if op := f.deploy(t, app.Name, f.spec(first, "1.0.0")); op.Status != domain.StatusSucceeded {
		t.Fatalf("部署应当成功: %s", op.ErrorMessage)
	}

	other := f.upload(t, "orders-other", []byte("different"))
	_, err := f.rt.Deploys.PrepareDeploy(context.Background(), app.Name, f.spec(other, "1.0.0"), "tester")
	if domain.CodeOf(err) != v1.CodeReleaseConflict {
		t.Fatalf("want RELEASE_CONFLICT, got %v", err)
	}
}

// 保留策略：keepLast 数的是**盘上有目录的版本**，其中一个是 current 自己。
func TestDeployRetentionKeepsCurrent(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")

	var ids []string
	for i, version := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		artifact := f.upload(t, "orders-"+version, []byte("v"+version))
		spec := f.spec(artifact, version, func(spec *domain.ApplicationSpec) {
			spec.Release.KeepLast = 1
		})
		if op := f.deploy(t, app.Name, spec); op.Status != domain.StatusSucceeded {
			t.Fatalf("第 %d 次部署应当成功: %s", i+1, op.ErrorMessage)
		}
		ids = append(ids, f.activeRelease(t, app.Name).ID)
	}

	// keepLast=1：除 current 之外一个都不留。
	current := f.activeRelease(t, app.Name)
	if current.ID != ids[2] {
		t.Fatalf("当前应当是最后一次部署的那个，got %s", current.ID)
	}
	for _, id := range ids[:2] {
		if _, err := os.Stat(f.releaseDirOf(app.Name, id)); !os.IsNotExist(err) {
			t.Fatalf("keepLast=1 时 %s 的目录应当被清掉", id)
		}
	}
	if _, err := os.Stat(f.releaseDirOf(app.Name, current.ID)); err != nil {
		t.Fatalf("current 的目录永远不能被清: %v", err)
	}

	// 被清掉的版本在库里是 removed（记录仍在，历史可查）。
	removed, err := f.rt.Catalogs.GetRelease(context.Background(), ids[0])
	if err != nil {
		t.Fatalf("取 release: %v", err)
	}
	if removed.Status != domain.ReleaseRemoved {
		t.Fatalf("被清掉的版本状态应当是 removed，got %s", removed.Status)
	}
}

func TestRollbackRequiresATarget(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")

	_, err := f.rt.Deploys.PrepareRollback(context.Background(), app.Name, "")
	if domain.CodeOf(err) != v1.CodeReleaseNotFound {
		t.Fatalf("只部署过一次时应当报「没有可回滚的版本」，got %v", err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// releaseIDOfVersion 按版本号找出 release ID（断言里用）。
func releaseIDOfVersion(t *testing.T, f *deployFixture, app, version string) string {
	t.Helper()
	application, err := f.rt.Catalogs.GetApplication(context.Background(), app)
	if err != nil {
		t.Fatalf("取应用: %v", err)
	}
	release, err := f.store.GetReleaseByVersion(context.Background(), application.ID, version)
	if err != nil {
		t.Fatalf("按版本取 release: %v", err)
	}
	if release == nil {
		t.Fatalf("找不到版本 %s", version)
	}
	return release.ID
}

// 换版本**必须重启**：`Start` 对已经 active 的 unit 是幂等的 no-op——那是 runtime.start
// 刻意要的性质（重复提交无害），但对部署不适用：不重启，旧进程会继续占着它自己的端口，
// 新版本的就绪检查永远等不到，而库里最后会写着「部署成功」。
//
// 这条用例钉的是调用的**顺序**（stop 在 start 之前）。它是容器验证抓出来的：第二版部署
// 一直以「15 秒内未就绪」失败，而原因正是旧的进程根本没被停掉。
func TestDeployRestartsPreviousVersion(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")

	first := f.upload(t, "orders-1.0.0", []byte("v1"))
	if op := f.deploy(t, app.Name, f.spec(first, "1.0.0")); op.Status != domain.StatusSucceeded {
		t.Fatalf("v1 部署应当成功: %s", op.ErrorMessage)
	}
	// 换版本（不同端口，因此只有真的重启过才会「切过去」）。
	second := f.upload(t, "orders-2.0.0", []byte("v2"))
	if op := f.deploy(t, app.Name, f.spec(second, "2.0.0", func(spec *domain.ApplicationSpec) {
		spec.Exec.Argv = []string{"bin/app", "--port", "28602"}
	})); op.Status != domain.StatusSucceeded {
		t.Fatalf("v2 部署应当成功: %s", op.ErrorMessage)
	}

	calls := f.adapter.callNames()
	// 第二次部署的那一段里必须有 stop，且它在 start 之前。
	stopIndex, startIndex := -1, -1
	for i, name := range calls {
		if name == "stop" && stopIndex < 0 {
			stopIndex = i
		}
	}
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i] == "start" {
			startIndex = i
			break
		}
	}
	if stopIndex < 0 {
		t.Fatalf("换版本必须先停掉上一个版本，调用序列是 %v", calls)
	}
	if startIndex < stopIndex {
		t.Fatalf("stop 必须在最后一次 start 之前，调用序列是 %v", calls)
	}
}

// Prepare 阶段就失败时，线上**没有被碰过**：报的是原因码，而不是 DEPLOY_ROLLED_BACK。
//
// 这条来自真机验证。java 的解释器路径写错时，部署报的是 DEPLOY_ROLLED_BACK
// （「已回到上一个稳定版本」），可实际上一行都没动过——运维会拿着这句话去查
// 「为什么回滚了」，而该改的是那份 manifest。`DEPLOY_ROLLED_BACK` 只说一件事：
// **我们改动过线上，并且把它撤销了**。
func TestDeployFailureBeforeTouchingLiveStateReportsCauseCode(t *testing.T) {
	interpreterError := domain.NewError(v1.CodeManifestInvalid,
		"runtime: java 的解释器 \"/opt/jdk-does-not-exist/bin/java\" 在本机不可用")

	t.Run("第一次部署：本来就没有正在运行的版本", func(t *testing.T) {
		f := newDeployFixture(t, nil)
		app := f.app(t, "orders-api")
		f.adapter.prepareErrWhen = func(*domain.ApplicationSpec) error { return interpreterError }

		artifact := f.upload(t, "orders-1.0.0", []byte("v1"))
		op := f.deploy(t, app.Name, f.spec(artifact, "1.0.0"))
		if op.Status != domain.StatusFailed {
			t.Fatalf("部署应当失败，got %s", op.Status)
		}
		if op.ErrorCode != string(v1.CodeManifestInvalid) {
			t.Fatalf("want MANIFEST_INVALID（原因码），got %s：%s", op.ErrorCode, op.ErrorMessage)
		}
		if !strings.Contains(op.ErrorMessage, "线上没有任何变化") {
			t.Fatalf("失败信息必须说清线上没被碰过，got %q", op.ErrorMessage)
		}
		// 一次都不该停：压根没有正在运行的版本可停。
		if calls := f.adapter.callNames(); containsString(calls, "stop") {
			t.Fatalf("第一次部署在 Prepare 失败时不该调 Stop，got %v", calls)
		}
	})

	t.Run("重新部署：上一个版本没有被停过", func(t *testing.T) {
		f := newDeployFixture(t, nil)
		app := f.app(t, "orders-api")

		first := f.upload(t, "orders-1.0.0", []byte("v1"))
		if op := f.deploy(t, app.Name, f.spec(first, "1.0.0")); op.Status != domain.StatusSucceeded {
			t.Fatalf("第一次部署应当成功: %s", op.ErrorMessage)
		}
		stable := f.activeRelease(t, app.Name)
		before := len(f.adapter.callNames())

		f.adapter.prepareErrWhen = func(spec *domain.ApplicationSpec) error {
			if spec.Artifact.Version == "2.0.0" {
				return interpreterError
			}
			return nil
		}
		second := f.upload(t, "orders-2.0.0", []byte("v2"))
		op := f.deploy(t, app.Name, f.spec(second, "2.0.0"))
		if op.ErrorCode != string(v1.CodeManifestInvalid) {
			t.Fatalf("want MANIFEST_INVALID（原因码），got %s：%s", op.ErrorCode, op.ErrorMessage)
		}
		if !strings.Contains(op.ErrorMessage, "1.0.0") {
			t.Fatalf("失败信息应当指明上一个版本没有被动过，got %q", op.ErrorMessage)
		}
		// 「没被动过」的行为定义：这次失败之后没有任何 Stop——旧版本还在跑着，
		// 只是新版本没上成。
		if calls := f.adapter.callNames()[before:]; containsString(calls, "stop") {
			t.Fatalf("Prepare 失败时不该停掉上一个版本，got %v", calls)
		}
		if active := f.activeRelease(t, app.Name); active == nil || active.ID != stable.ID {
			t.Fatalf("active 应当仍是 %s，got %+v", stable.ID, active)
		}
	})
}

// **停掉上一个版本之后、切成新版本之前**的失败必须把旧版本放回去。
//
// 这是真机验证暴露出来的第二个缺陷：`switched` 只在 Activate 成功后才置位，于是
// 「停了旧的、新的还没切上去就失败」这段窗口里的失败被当成「什么都没发生」——
// 旧版本停在那里没人管，而 Operation 写着「已回到上一个稳定版本」。服务实际上是停的。
func TestDeployFailureAfterStoppingPreviousRestoresIt(t *testing.T) {
	f := newDeployFixture(t, nil)
	app := f.app(t, "orders-api")

	first := f.upload(t, "orders-1.0.0", []byte("v1"))
	if op := f.deploy(t, app.Name, f.spec(first, "1.0.0")); op.Status != domain.StatusSucceeded {
		t.Fatalf("第一次部署应当成功: %s", op.ErrorMessage)
	}
	stable := f.activeRelease(t, app.Name)

	// 第二版声称是 tar-gz，给的却是普通字节：Prepare 会过，物化会失败——
	// 也就是失败点正好落在「已经停掉旧版本、还没切到新版本」那段窗口里。
	broken := f.uploadWith(t, "orders-2.0.0", "application/gzip", []byte("not-a-gzip"))
	spec := f.spec(broken, "2.0.0", func(s *domain.ApplicationSpec) {
		s.Artifact.Unpack = domain.SpecUnpack{Strategy: domain.UnpackTarGz, StrategyExplicit: true}
		s.Artifact.FileName = ""
	})

	op := f.deploy(t, app.Name, spec)
	if op.Status != domain.StatusFailed {
		t.Fatalf("部署应当失败，got %s", op.Status)
	}
	if op.ErrorCode != string(v1.CodeDeployRolledBack) {
		t.Fatalf("这次**确实**回滚了，want DEPLOY_ROLLED_BACK，got %s：%s", op.ErrorCode, op.ErrorMessage)
	}

	// 回滚动作真的发生过：最后一次调用是 start，而且是拿**上一个版本**的规格起的。
	calls := f.adapter.callNames()
	if len(calls) == 0 || calls[len(calls)-1] != "start" {
		t.Fatalf("停掉旧版本之后失败，必须把它重新启动，got %v", calls)
	}
	if last := f.adapter.lastSpec(); last == nil || last.Artifact.Version != "1.0.0" {
		t.Fatalf("重新启动的应当是上一个版本，got %+v", last)
	}
	if target := f.currentTarget(t, app.Name); target != stable.ID {
		t.Fatalf("current 应当回到 %s，got %s", stable.ID, target)
	}
	if active := f.activeRelease(t, app.Name); active == nil || active.ID != stable.ID {
		t.Fatalf("active 应当仍是 %s，got %+v", stable.ID, active)
	}
}

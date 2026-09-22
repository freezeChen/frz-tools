package application_test

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/adapters/blob"
	"frz-tools/internal/adapters/sqlite"
	"frz-tools/internal/application"
	"frz-tools/internal/domain"
	"frz-tools/internal/idgen"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newArtifactRuntime(t *testing.T, policy application.ArtifactPolicy) (*application.Runtime, *sqlite.Store, string) {
	t.Helper()

	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := blob.NewLocal(root, 0o640, 0o750)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	sqlStore, err := sqlite.Open(filepath.Join(t.TempDir(), "opsd.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { sqlStore.Close() })
	if err := sqlStore.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if policy.MaxUploadBytes == 0 {
		policy = application.ArtifactPolicy{MaxUploadBytes: 1 << 20, QuotaBytes: 1 << 22}
	}

	rt := application.NewRuntime(application.Options{
		Repo:           sqlStore,
		Store:          store,
		ArtifactPolicy: policy,
		Defaults:       application.Defaults{Timeout: 5 * time.Second, MaxOutputBytes: 4096},
		Workers:        1,
		Logger:         discardLogger(),
	})
	return rt, sqlStore, root
}

func putArtifact(t *testing.T, rt *application.Runtime, content string) *domain.Artifact {
	t.Helper()
	artifact, _, err := rt.Artifacts.Put(context.Background(), application.PutArtifactInput{
		Name:      "demo.tar.gz",
		MediaType: "application/gzip",
		Body:      strings.NewReader(content),
		CreatedBy: "tester",
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	return artifact
}

func TestArtifactPutIsContentAddressed(t *testing.T) {
	rt, _, _ := newArtifactRuntime(t, application.ArtifactPolicy{})
	ctx := context.Background()

	first, created, err := rt.Artifacts.Put(ctx, application.PutArtifactInput{Body: strings.NewReader("payload-one"), Name: "a"})
	if err != nil || !created {
		t.Fatalf("首次上传应当创建记录：created=%v err=%v", created, err)
	}

	second, created, err := rt.Artifacts.Put(ctx, application.PutArtifactInput{Body: strings.NewReader("payload-one"), Name: "renamed"})
	if err != nil {
		t.Fatalf("重复上传：%v", err)
	}
	if created {
		t.Fatal("相同内容不得产生新记录")
	}
	if second.ID != first.ID {
		t.Fatalf("相同内容应复用记录：%s != %s", second.ID, first.ID)
	}

	digest, size, err := domain.DigestOf(strings.NewReader("payload-one"))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if first.Digest != digest || first.Size != size {
		t.Fatalf("摘要或大小不符：%+v", first)
	}
}

func TestArtifactPutRejectsDeclaredDigestMismatch(t *testing.T) {
	rt, store, root := newArtifactRuntime(t, application.ArtifactPolicy{})
	ctx := context.Background()

	wrong := domain.Digest("sha256:" + strings.Repeat("ab", 32))
	_, _, err := rt.Artifacts.Put(ctx, application.PutArtifactInput{Body: strings.NewReader("real"), Digest: wrong})
	if domain.CodeOf(err) != v1.CodeArtifactChecksum {
		t.Fatalf("want ARTIFACT_CHECKSUM_MISMATCH, got %v", err)
	}

	if _, err := store.GetArtifact(ctx, wrong.String()); domain.CodeOf(err) != v1.CodeArtifactNotFound {
		t.Fatalf("被拒绝的上传不得留下记录，got %v", err)
	}
	if files := countBlobs(t, root); files != 0 {
		t.Fatalf("被拒绝的上传不得留下内容，got %d", files)
	}
}

func TestArtifactPutRejectsRelativeRoot(t *testing.T) {
	if _, err := blob.NewLocal("relative/artifacts", 0o640, 0o750); domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("want CONFIG_INVALID, got %v", err)
	}
}

func TestArtifactPutRejectsSymlinkRoot(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("无法创建符号链接：%v", err)
	}
	if _, err := blob.NewLocal(link, 0o640, 0o750); domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("want CONFIG_INVALID, got %v", err)
	}
}

func TestArtifactVerifyDetectsTampering(t *testing.T) {
	rt, _, root := newArtifactRuntime(t, application.ArtifactPolicy{})
	ctx := context.Background()
	artifact := putArtifact(t, rt, "original content")

	if _, err := rt.Artifacts.Verify(ctx, artifact.ID); err != nil {
		t.Fatalf("verify clean artifact: %v", err)
	}

	tamperFirstBlob(t, root)

	if _, err := rt.Artifacts.Verify(ctx, artifact.ID); domain.CodeOf(err) != v1.CodeArtifactChecksum {
		t.Fatalf("want ARTIFACT_CHECKSUM_MISMATCH, got %v", err)
	}
}

func TestArtifactDeleteRequiresNoReferences(t *testing.T) {
	rt, store, root := newArtifactRuntime(t, application.ArtifactPolicy{})
	ctx := context.Background()
	artifact := putArtifact(t, rt, "release payload")

	now := time.Now().UTC()
	app := &domain.Application{ID: idgen.NewApplicationID(), Name: "demo-app", CreatedAt: now, UpdatedAt: now}
	if err := store.CreateApplication(ctx, app); err != nil {
		t.Fatalf("create application: %v", err)
	}
	if _, err := store.CreateRelease(ctx, &domain.Release{
		ID:            idgen.NewReleaseID(),
		ApplicationID: app.ID,
		ArtifactID:    artifact.ID,
		Version:       "1.0.0",
		CreatedAt:     now,
	}); err != nil {
		t.Fatalf("create release: %v", err)
	}

	if err := rt.Artifacts.Delete(ctx, artifact.ID); domain.CodeOf(err) != v1.CodeArtifactInUse {
		t.Fatalf("被引用的制品必须拒绝删除，got %v", err)
	}
	if files := countBlobs(t, root); files != 1 {
		t.Fatalf("拒绝删除时不得回收内容，剩余 %d", files)
	}

	if _, err := store.DB().ExecContext(ctx, `DELETE FROM releases`); err != nil {
		t.Fatalf("delete release: %v", err)
	}
	if err := rt.Artifacts.Delete(ctx, artifact.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.GetArtifact(ctx, artifact.ID); domain.CodeOf(err) != v1.CodeArtifactNotFound {
		t.Fatalf("删除后不应再查得到，got %v", err)
	}
	if files := countBlobs(t, root); files != 0 {
		t.Fatalf("删除后内容应当被回收，剩余 %d", files)
	}
}

func TestArtifactCollectRespectsRetentionAndDryRun(t *testing.T) {
	rt, _, root := newArtifactRuntime(t, application.ArtifactPolicy{})
	ctx := context.Background()

	for _, content := range []string{"one", "two", "three"} {
		putArtifact(t, rt, content)
	}

	dry, err := rt.Artifacts.Collect(ctx, 1, 0, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(dry.Removed) != 2 {
		t.Fatalf("预演应报告 2 个待清理制品，got %d", len(dry.Removed))
	}
	if files := countBlobs(t, root); files != 3 {
		t.Fatalf("预演不得删除任何内容，剩余 %d", files)
	}

	real, err := rt.Artifacts.Collect(ctx, 1, 0, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(real.Removed) != 2 || real.Kept != 1 {
		t.Fatalf("保留策略不符合预期：%+v", real)
	}
	if files := countBlobs(t, root); files != 1 {
		t.Fatalf("只应保留 1 份内容，剩余 %d", files)
	}
}

func TestArtifactCollectRemovesOrphanContent(t *testing.T) {
	rt, _, root := newArtifactRuntime(t, application.ArtifactPolicy{})
	ctx := context.Background()

	// 直接向存储写入内容而不登记元数据，模拟崩溃在 rename 与写库之间。
	orphan, err := blob.NewLocal(root, 0o640, 0o750)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	if _, err := orphan.Put(ctx, strings.NewReader("no metadata"), ""); err != nil {
		t.Fatalf("put orphan: %v", err)
	}

	result, err := rt.Artifacts.Collect(ctx, 0, 0, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(result.Orphans) != 1 {
		t.Fatalf("应当回收 1 个孤儿内容，got %+v", result.Orphans)
	}
	if files := countBlobs(t, root); files != 0 {
		t.Fatalf("孤儿内容应当被清理，剩余 %d", files)
	}
}

func TestArtifactUploadLimitAndQuota(t *testing.T) {
	rt, _, _ := newArtifactRuntime(t, application.ArtifactPolicy{MaxUploadBytes: 8, QuotaBytes: 16})
	ctx := context.Background()

	_, _, err := rt.Artifacts.Put(ctx, application.PutArtifactInput{Body: strings.NewReader(strings.Repeat("x", 32))})
	if domain.CodeOf(err) != v1.CodeUploadTooLarge {
		t.Fatalf("want UPLOAD_TOO_LARGE, got %v", err)
	}

	if _, _, err := rt.Artifacts.Put(ctx, application.PutArtifactInput{Body: strings.NewReader("12345678")}); err != nil {
		t.Fatalf("刚好达到上限应当被接受：%v", err)
	}
	if _, _, err := rt.Artifacts.Put(ctx, application.PutArtifactInput{Body: strings.NewReader("87654321")}); err != nil {
		t.Fatalf("第二份内容应当被接受：%v", err)
	}
	// 配额已用满（16 字节）。
	_, _, err = rt.Artifacts.Put(ctx, application.PutArtifactInput{Body: strings.NewReader("z")})
	if domain.CodeOf(err) != v1.CodeStorageQuotaExceeded {
		t.Fatalf("want STORAGE_QUOTA_EXCEEDED, got %v", err)
	}
}

func TestArtifactOperationsRequireConfiguredStore(t *testing.T) {
	sqlStore, err := sqlite.Open(filepath.Join(t.TempDir(), "opsd.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { sqlStore.Close() })
	if err := sqlStore.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	rt := application.NewRuntime(application.Options{
		Repo:    sqlStore,
		Workers: 1,
		Logger:  discardLogger(),
	})
	ctx := context.Background()

	if _, _, err := rt.Artifacts.Put(ctx, application.PutArtifactInput{Body: strings.NewReader("x")}); domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("put want CONFIG_INVALID, got %v", err)
	}
	if _, err := rt.Artifacts.List(ctx, "", 10); domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("list want CONFIG_INVALID, got %v", err)
	}
	if _, err := rt.Artifacts.Get(ctx, "art_x"); domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("get want CONFIG_INVALID, got %v", err)
	}
	if err := rt.Artifacts.Delete(ctx, "art_x"); domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("delete want CONFIG_INVALID, got %v", err)
	}
}

func TestArtifactPutRejectsInvalidDeclaredDigest(t *testing.T) {
	rt, _, _ := newArtifactRuntime(t, application.ArtifactPolicy{})
	_, _, err := rt.Artifacts.Put(context.Background(), application.PutArtifactInput{
		Body:   bytes.NewReader([]byte("x")),
		Digest: domain.Digest("not-a-digest"),
	})
	if domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
}

func countBlobs(t *testing.T, root string) int {
	t.Helper()
	count := 0
	blobRoot := filepath.Join(root, "blobs")
	if _, err := os.Stat(blobRoot); err != nil {
		return 0
	}
	err := filepath.WalkDir(blobRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk blobs: %v", err)
	}
	return count
}

func tamperFirstBlob(t *testing.T, root string) {
	t.Helper()
	target := ""
	err := filepath.WalkDir(filepath.Join(root, "blobs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && target == "" {
			target = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk blobs: %v", err)
	}
	if target == "" {
		t.Fatal("未找到已落盘的 blob")
	}
	if err := os.WriteFile(target, []byte("tampered content that changes the digest"), 0o640); err != nil {
		t.Fatalf("tamper: %v", err)
	}
}

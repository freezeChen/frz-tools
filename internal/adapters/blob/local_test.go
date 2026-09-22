package blob

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/application/storagecontract"
	"github.com/freezeChen/frz-tools/internal/domain"
)

func newTestStore(t *testing.T) *Local {
	t.Helper()
	store, err := NewLocal(filepath.Join(t.TempDir(), "artifacts"), 0o640, 0o750)
	if err != nil {
		t.Fatalf("new local store: %v", err)
	}
	return store
}

func TestLocalSatisfiesStorageContract(t *testing.T) {
	storagecontract.Run(t, func(t *testing.T) application.StorageBackend {
		return newTestStore(t)
	})
}

func TestLocalCreatesDirectoriesWithConfiguredModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := NewLocal(root, 0o640, 0o750)
	if err != nil {
		t.Fatalf("new local store: %v", err)
	}

	for _, dir := range []string{root, filepath.Join(root, "tmp"), filepath.Join(root, "blobs", "sha256")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if got := info.Mode().Perm(); got != 0o750 {
			t.Fatalf("目录 %s 模式：期望 0750，实际 %04o", dir, got)
		}
	}

	if _, err := store.Put(context.Background(), strings.NewReader("mode check"), ""); err != nil {
		t.Fatalf("put: %v", err)
	}

	var blobPath string
	err = filepath.WalkDir(filepath.Join(root, "blobs"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			blobPath = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if blobPath == "" {
		t.Fatal("未找到落盘的 blob")
	}

	info, err := os.Stat(blobPath)
	if err != nil {
		t.Fatalf("stat blob: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("blob 模式：期望 0640，实际 %04o", got)
	}
}

func TestLocalRootMustBeAbsolute(t *testing.T) {
	_, err := NewLocal("relative/path", 0o640, 0o750)
	if domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("want CONFIG_INVALID, got %v", err)
	}
}

// 路径穿越是结构性防护的结果：路径只由 digest 派生，调用方传入的任何名字都不参与。
func TestLocalRejectsTraversalInDigest(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	malicious := []domain.Digest{
		domain.Digest("sha256:../../../../etc/passwd"),
		domain.Digest("sha256:/etc/passwd"),
		domain.Digest("../../etc/passwd"),
		domain.Digest("sha256:" + strings.Repeat("ab", 32) + "/../../escape"),
	}
	for _, digest := range malicious {
		if _, err := store.Stat(ctx, digest); err == nil {
			t.Fatalf("digest %q 必须被拒绝", digest)
		}
		if _, err := store.Open(ctx, digest); err == nil {
			t.Fatalf("digest %q 必须被拒绝", digest)
		}
	}
}

func TestLocalCleanupTempRemovesStaleUploads(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	stale := filepath.Join(store.tmpPath(), "upload-stale")
	if err := os.WriteFile(stale, []byte("half written"), 0o600); err != nil {
		t.Fatalf("write stale upload: %v", err)
	}
	keep, err := store.Put(ctx, strings.NewReader("keep"), "")
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	removed, err := store.CleanupTemp(ctx)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if removed != 1 {
		t.Fatalf("want 1 removed, got %d", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("临时文件应当被删除")
	}
	if _, err := store.Stat(ctx, keep.Digest); err != nil {
		t.Fatalf("清理不得影响已提交内容: %v", err)
	}
}

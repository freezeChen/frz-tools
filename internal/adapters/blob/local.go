package blob

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/application"
	"frz-tools/internal/domain"
)

const (
	algorithmDir = "sha256"
	blobsDir     = "blobs"
	tmpDir       = "tmp"
	copyBufSize  = 64 * 1024
)

// Local 是内容寻址的本地实现。
//
//	<root>/tmp/                          上传中的临时文件
//	<root>/blobs/sha256/ab/cd/<hex>      两级 fanout，路径完全由 digest 派生
//
// 临时文件与 blobs 位于同一文件系统，因此 rename 是原子的。
type Local struct {
	root     string
	fileMode os.FileMode
	dirMode  os.FileMode
}

func NewLocal(root string, fileMode, dirMode os.FileMode) (*Local, error) {
	if !filepath.IsAbs(root) {
		return nil, domain.NewError(v1.CodeConfigInvalid, "artifact store root %q must be an absolute path", root)
	}
	// 根目录是符号链接时，写入位置可能被指向别处，直接拒绝。
	if info, err := os.Lstat(root); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, domain.NewError(v1.CodeConfigInvalid, "artifact store root %q must not be a symlink", root)
	}
	store := &Local{root: root, fileMode: fileMode, dirMode: dirMode}
	for _, dir := range []string{store.root, store.tmpPath(), store.blobRoot()} {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return nil, domain.NewError(v1.CodeConfigInvalid, "cannot create artifact directory %q: %v", dir, err)
		}
	}
	return store, nil
}

func (l *Local) Root() string { return l.root }

func (l *Local) tmpPath() string  { return filepath.Join(l.root, tmpDir) }
func (l *Local) blobRoot() string { return filepath.Join(l.root, blobsDir, algorithmDir) }

// path 只由 digest 派生，调用方提供的文件名永远不参与，因此不存在路径穿越面。
func (l *Local) path(digest domain.Digest) (string, error) {
	if err := digest.Validate(); err != nil {
		return "", err
	}
	hexPart := strings.TrimPrefix(digest.String(), algorithmDir+":")
	return filepath.Join(l.blobRoot(), hexPart[0:2], hexPart[2:4], hexPart), nil
}

func (l *Local) Put(ctx context.Context, r io.Reader, expected domain.Digest) (application.Stored, error) {
	if expected != "" {
		if err := expected.Validate(); err != nil {
			return application.Stored{}, err
		}
	}

	tmp, err := os.CreateTemp(l.tmpPath(), "upload-*")
	if err != nil {
		return application.Stored{}, domain.NewError(v1.CodeInternal, "cannot create temporary upload file: %v", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(l.fileMode); err != nil {
		return application.Stored{}, domain.NewError(v1.CodeInternal, "cannot set temporary file mode: %v", err)
	}

	hasher := domain.NewHasher()
	if _, err := copyWithContext(ctx, io.MultiWriter(tmp, hasher), r); err != nil {
		return application.Stored{}, err
	}
	if err := tmp.Sync(); err != nil {
		return application.Stored{}, domain.NewError(v1.CodeInternal, "cannot flush temporary upload file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return application.Stored{}, domain.NewError(v1.CodeInternal, "cannot close temporary upload file: %v", err)
	}

	digest := hasher.Digest()
	if expected != "" && expected != digest {
		return application.Stored{}, domain.NewError(v1.CodeArtifactChecksum,
			"uploaded content digest %s does not match declared %s", digest, expected)
	}

	final, err := l.path(digest)
	if err != nil {
		return application.Stored{}, err
	}

	// 相同内容已经存在时直接丢弃临时文件：制品不可变，重复上传是幂等的。
	if info, statErr := os.Stat(final); statErr == nil {
		return application.Stored{Digest: digest, Size: info.Size(), ModifiedAt: info.ModTime().UTC()}, nil
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return application.Stored{}, domain.NewError(v1.CodeInternal, "cannot stat %s: %v", final, statErr)
	}

	if err := os.MkdirAll(filepath.Dir(final), l.dirMode); err != nil {
		return application.Stored{}, domain.NewError(v1.CodeInternal, "cannot create blob directory: %v", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return application.Stored{}, domain.NewError(v1.CodeInternal, "cannot commit blob: %v", err)
	}
	committed = true

	info, err := os.Stat(final)
	if err != nil {
		return application.Stored{}, domain.NewError(v1.CodeInternal, "cannot stat committed blob: %v", err)
	}
	return application.Stored{Digest: digest, Size: info.Size(), ModifiedAt: info.ModTime().UTC()}, nil
}

func (l *Local) Open(ctx context.Context, digest domain.Digest) (io.ReadCloser, error) {
	path, err := l.path(digest)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, domain.NewError(v1.CodeArtifactNotFound, "blob %s is not present in the store", digest)
	}
	if err != nil {
		return nil, domain.NewError(v1.CodeInternal, "cannot open blob %s: %v", digest, err)
	}
	return file, nil
}

func (l *Local) Stat(ctx context.Context, digest domain.Digest) (application.Stored, error) {
	path, err := l.path(digest)
	if err != nil {
		return application.Stored{}, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return application.Stored{}, domain.NewError(v1.CodeArtifactNotFound, "blob %s is not present in the store", digest)
	}
	if err != nil {
		return application.Stored{}, domain.NewError(v1.CodeInternal, "cannot stat blob %s: %v", digest, err)
	}
	if info.IsDir() {
		return application.Stored{}, domain.NewError(v1.CodeInternal, "blob path %s is a directory", path)
	}
	return application.Stored{Digest: digest, Size: info.Size(), ModifiedAt: info.ModTime().UTC()}, nil
}

func (l *Local) Delete(ctx context.Context, digest domain.Digest) error {
	path, err := l.path(digest)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return domain.NewError(v1.CodeInternal, "cannot delete blob %s: %v", digest, err)
	}
	return nil
}

func (l *Local) List(ctx context.Context) ([]application.Stored, error) {
	var stored []application.Stored
	err := filepath.WalkDir(l.blobRoot(), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() {
			return nil
		}
		digest, err := domain.ParseDigest(algorithmDir + ":" + entry.Name())
		if err != nil {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		stored = append(stored, application.Stored{
			Digest:     digest,
			Size:       info.Size(),
			ModifiedAt: info.ModTime().UTC(),
		})
		return nil
	})
	if err != nil {
		return nil, domain.NewError(v1.CodeInternal, "cannot list blob store: %v", err)
	}
	return stored, nil
}

// CleanupTemp 清理崩溃或客户端中断留下的上传临时文件，返回删除数量。
func (l *Local) CleanupTemp(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(l.tmpPath())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, domain.NewError(v1.CodeInternal, "cannot read temporary directory: %v", err)
	}
	removed := 0
	for _, entry := range entries {
		if ctx.Err() != nil {
			return removed, ctx.Err()
		}
		if entry.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(l.tmpPath(), entry.Name())); err != nil {
			return removed, domain.NewError(v1.CodeInternal, "cannot remove stale upload %s: %v", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, copyBufSize)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return total, err
			}
			total += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

var _ application.StorageBackend = (*Local)(nil)

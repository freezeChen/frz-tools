package files_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/freezeChen/frz-tools/internal/adapters/backup/files"
	"github.com/freezeChen/frz-tools/internal/application/backupcontract"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// TestAdapterContract 让 files 适配器通过共享合约测试。
//
// 这是它正确性的**主要证据**：合约只关心语义（能不能往返、越界会不会被拒），
// 不关心实现用了 tar 还是别的什么。
func TestAdapterContract(t *testing.T) {
	backupcontract.Run(t, func(t *testing.T) backupcontract.Harness {
		// 无特权环境跑真实文件系统操作：root 前缀把策略里的绝对路径重定向到临时目录。
		// 断言的是真实的文件内容与模式，而不是复制一份前缀规则。
		root := t.TempDir()
		adapter := files.New(files.WithRoot(root), files.WithTempRoot(t.TempDir()))

		declaredRoot := func(policy *domain.BackupPolicy) string {
			return adapter.RootPath(policy.Resource.Paths[0])
		}

		return backupcontract.Harness{
			Adapter: adapter,
			NewPolicy: func(t *testing.T, name string) *domain.BackupPolicy {
				// 每个子测试一份独立的声明目录，互不污染。
				return &domain.BackupPolicy{
					APIVersion: domain.BackupPolicyAPIVersion,
					Kind:       domain.BackupPolicyKind,
					Name:       name,
					Resource: domain.BackupResource{
						Kind:  domain.BackupResourceFiles,
						Paths: []string{"/var/lib/" + name + "/data"},
					},
					// 合约夹具不带排除规则：往返比对要求「备份前有什么、恢复后也有什么」，
					// 排除规则的功效由 TestExcludePatterns 单独覆盖。
					Retention: domain.BackupRetention{KeepLast: 1},
				}
			},
			Populate: func(t *testing.T, policy *domain.BackupPolicy) string {
				root := declaredRoot(policy)
				if err := os.MkdirAll(filepath.Join(root, "nested"), 0o750); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				writeFile(t, filepath.Join(root, "alpha.txt"), []byte("alpha\n"), 0o640)
				writeFile(t, filepath.Join(root, "nested", "binary.bin"),
					[]byte{0x00, 0x01, 0x02, 0xff, 0xfe}, 0o600)
				// 空文件也要能往返：tar 的零长度条目是一个容易写错的边界。
				writeFile(t, filepath.Join(root, "empty"), nil, 0o644)
				return fingerprint(t, root)
			},
			Fingerprint: func(t *testing.T, policy *domain.BackupPolicy) string {
				return fingerprint(t, declaredRoot(policy))
			},
			Wipe: func(t *testing.T, policy *domain.BackupPolicy) {
				if err := os.RemoveAll(declaredRoot(policy)); err != nil {
					t.Fatalf("wipe: %v", err)
				}
			},
		}
	})
}

func writeFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fingerprint 把一棵树的「可观察内容」压成一个字符串：相对路径、长度、摘要与权限。
//
// 权限也要进指纹：恢复把文件写回来了但模式错了（例如把 0600 的私钥恢复成 0644），
// 备份工具最不该犯的错就是这种静默的权限放宽。
func fingerprint(t *testing.T, root string) string {
	t.Helper()

	// 被清空的资源指纹就是「空」：Wipe 之后这里本来就没有东西可走。
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return ""
	}

	var entries []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			entries = append(entries, rel+":dir:"+info.Mode().Perm().String())
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		entries = append(entries, rel+":"+hex.EncodeToString(sum[:])+":"+info.Mode().Perm().String())
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(entries)
	return strings.Join(entries, "|")
}

// 排除规则的功效单独覆盖：合约的往返用例要求「备份前有什么、恢复后也有什么」，
// 因此它的夹具不带排除规则。
func TestExcludePatterns(t *testing.T) {
	root := t.TempDir()
	adapter := files.New(files.WithRoot(root))

	policy := &domain.BackupPolicy{
		Kind: domain.BackupPolicyKind,
		Name: "exclude-case",
		Resource: domain.BackupResource{
			Kind:     domain.BackupResourceFiles,
			Paths:    []string{"/srv/data"},
			Exclude:  []string{"*.tmp", "cache"},
			Symlinks: domain.SymlinkSkip,
		},
		Retention: domain.BackupRetention{KeepLast: 1},
	}
	base := adapter.RootPath("/srv/data")
	for _, dir := range []string{"cache/inner", "keep"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	writeFile(t, filepath.Join(base, "keep", "a.txt"), []byte("keep"), 0o644)
	writeFile(t, filepath.Join(base, "keep", "b.tmp"), []byte("tmp"), 0o644)
	writeFile(t, filepath.Join(base, "cache", "c.txt"), []byte("cached"), 0o644)
	writeFile(t, filepath.Join(base, "cache", "inner", "d.txt"), []byte("deep"), 0o644)

	names := archiveEntries(t, adapter, policy)

	// 命中的目录要连子树一起跳过：只排除目录本身的话，cache/inner/d.txt 照样进归档。
	assertNotPresent(t, names, "srv/data/cache")
	assertNotPresent(t, names, "srv/data/cache/inner/d.txt")
	assertNotPresent(t, names, "srv/data/keep/b.tmp")
	assertPresent(t, names, "srv/data/keep/a.txt")
}

func TestSymlinkPolicies(t *testing.T) {
	root := t.TempDir()
	adapter := files.New(files.WithRoot(root))

	base := adapter.RootPath("/srv/data")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(base, "real.txt"), []byte("real-content"), 0o644)
	if err := os.Symlink(filepath.Join(base, "real.txt"), filepath.Join(base, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	newPolicy := func(mode domain.SymlinkPolicy) *domain.BackupPolicy {
		return &domain.BackupPolicy{
			Kind: domain.BackupPolicyKind,
			Name: "symlink-" + string(mode),
			Resource: domain.BackupResource{
				Kind:     domain.BackupResourceFiles,
				Paths:    []string{"/srv/data"},
				Symlinks: mode,
			},
			Retention: domain.BackupRetention{KeepLast: 1},
		}
	}

	t.Run("skip 不带链接进归档", func(t *testing.T) {
		names := archiveEntries(t, adapter, newPolicy(domain.SymlinkSkip))
		assertNotPresent(t, names, "srv/data/link.txt")
		assertPresent(t, names, "srv/data/real.txt")
	})

	t.Run("follow 把目标的内容搬进来", func(t *testing.T) {
		policy := newPolicy(domain.SymlinkFollow)
		names := archiveEntries(t, adapter, policy)
		assertPresent(t, names, "srv/data/link.txt")

		// 恢复出来应当是一份普通文件，内容与目标一致。
		stream := backupBytes(t, adapter, policy)
		dest := t.TempDir()
		isolated := files.New(files.WithRoot(dest))
		if err := isolated.Restore(context.Background(), policy, bytes.NewReader(stream), domain.RestoreInPlace); err != nil {
			t.Fatalf("restore: %v", err)
		}
		content, err := os.ReadFile(filepath.Join(dest, "srv/data/link.txt"))
		if err != nil {
			t.Fatalf("read restored link: %v", err)
		}
		if string(content) != "real-content" {
			t.Fatalf("跟随链接的内容不对: %q", content)
		}
		info, err := os.Lstat(filepath.Join(dest, "srv/data/link.txt"))
		if err != nil {
			t.Fatalf("lstat: %v", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Fatal("恢复出来的应当是普通文件，而不是又一个链接")
		}
	})

	t.Run("error 直接拒绝", func(t *testing.T) {
		var buf bytes.Buffer
		_, err := adapter.Backup(context.Background(), newPolicy(domain.SymlinkError), &buf)
		if err == nil {
			t.Fatal("symlinks=error 时遇到链接必须失败")
		}
	})
}

// 归档里不得出现绝对路径或 .. 的条目名；解包侧也要拒绝它们。
func TestRejectsTraversalEntries(t *testing.T) {
	adapter := files.New(files.WithRoot(t.TempDir()))
	policy := &domain.BackupPolicy{
		Kind:      domain.BackupPolicyKind,
		Name:      "traversal",
		Resource:  domain.BackupResource{Kind: domain.BackupResourceFiles, Paths: []string{"/srv/data"}},
		Retention: domain.BackupRetention{KeepLast: 1},
	}

	// 手写一个带 .. 的归档：不能靠「我们自己的 Backup 不产生这种条目」来保证安全，
	// 因为归档可能是别人给的。
	stream := tarWithEntry(t, "../escaped.txt", "pwned")

	if err := adapter.Verify(context.Background(), policy, bytes.NewReader(stream)); err == nil {
		t.Fatal("Verify 必须拒绝逃出目标根的条目名")
	}
	if err := adapter.Restore(context.Background(), policy, bytes.NewReader(stream), domain.RestoreIsolated); err == nil {
		t.Fatal("Restore 必须拒绝逃出目标根的条目名")
	}
}

// 同一份内容两次备份必须产出同样的字节：否则内容寻址的 digest 每次都变，
// 去重与比对都无从谈起。
func TestBackupIsDeterministic(t *testing.T) {
	root := t.TempDir()
	adapter := files.New(files.WithRoot(root))

	base := adapter.RootPath("/srv/data")
	if err := os.MkdirAll(filepath.Join(base, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"b.txt", "a.txt", "sub/c.txt"} {
		writeFile(t, filepath.Join(base, name), []byte("content of "+name), 0o644)
	}

	policy := &domain.BackupPolicy{
		Kind:      domain.BackupPolicyKind,
		Name:      "deterministic",
		Resource:  domain.BackupResource{Kind: domain.BackupResourceFiles, Paths: []string{"/srv/data"}},
		Retention: domain.BackupRetention{KeepLast: 1},
	}

	first := backupBytes(t, adapter, policy)
	second := backupBytes(t, adapter, policy)
	if !bytes.Equal(first, second) {
		t.Fatalf("两次备份的字节不一致：len %d vs %d", len(first), len(second))
	}
}

func archiveEntries(t *testing.T, adapter *files.Adapter, policy *domain.BackupPolicy) []string {
	t.Helper()
	stream := backupBytes(t, adapter, policy)

	reader := tar.NewReader(bytes.NewReader(stream))
	var names []string
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		names = append(names, header.Name)
	}
	return names
}

// tarWithEntry 手写一个只含一条指定名字的归档。
func tarWithEntry(t *testing.T, entryName, content string) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	if err := writer.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     entryName,
		Mode:     0o644,
		Size:     int64(len(content)),
	}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes()
}

func backupBytes(t *testing.T, adapter *files.Adapter, policy *domain.BackupPolicy) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := adapter.Backup(context.Background(), policy, &buf); err != nil {
		t.Fatalf("backup: %v", err)
	}
	return buf.Bytes()
}

func assertPresent(t *testing.T, names []string, want string) {
	t.Helper()
	for _, name := range names {
		if name == want {
			return
		}
	}
	t.Fatalf("归档里应当有 %q，实际：%v", want, names)
}

func assertNotPresent(t *testing.T, names []string, unwanted string) {
	t.Helper()
	for _, name := range names {
		if name == unwanted {
			t.Fatalf("归档里不该有 %q，实际：%v", unwanted, names)
		}
	}
}

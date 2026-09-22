package secret

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

func TestResolveEnvSecret(t *testing.T) {
	t.Setenv("FRZ_TEST_TOKEN", "s3cr3t-value")
	resolver := NewResolver(nil)

	value, err := resolver.Resolve(context.Background(), domain.SecretRef{Kind: domain.SecretKindEnv, Name: "FRZ_TEST_TOKEN"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if value != "s3cr3t-value" {
		t.Fatalf("unexpected value %q", value)
	}
}

func TestResolveMissingEnvSecret(t *testing.T) {
	resolver := NewResolver(nil)
	_, err := resolver.Resolve(context.Background(), domain.SecretRef{Kind: domain.SecretKindEnv, Name: "FRZ_TEST_ABSENT"})
	if domain.CodeOf(err) != v1.CodeSecretUnresolved {
		t.Fatalf("want SECRET_UNRESOLVED, got %v", err)
	}
}

func TestResolveFileSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	resolver := NewResolver([]string{dir})
	value, err := resolver.Resolve(context.Background(), domain.SecretRef{Kind: domain.SecretKindFile, Name: path})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if value != "file-secret" {
		t.Fatalf("应当去掉行尾换行，got %q", value)
	}
}

func TestResolveFileSecretRejectsPermissiveMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("leaky"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	resolver := NewResolver([]string{dir})
	_, err := resolver.Resolve(context.Background(), domain.SecretRef{Kind: domain.SecretKindFile, Name: path})
	if domain.CodeOf(err) != v1.CodeSecretUnresolved {
		t.Fatalf("对属组/其他用户开放的凭据文件必须被拒绝，got %v", err)
	}
}

func TestResolveFileSecretRejectsOutsideAllowedDirectories(t *testing.T) {
	allowed := t.TempDir()
	other := t.TempDir()
	path := filepath.Join(other, "token")
	if err := os.WriteFile(path, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	resolver := NewResolver([]string{allowed})
	_, err := resolver.Resolve(context.Background(), domain.SecretRef{Kind: domain.SecretKindFile, Name: path})
	if domain.CodeOf(err) != v1.CodeSecretUnresolved {
		t.Fatalf("允许目录之外的凭据必须被拒绝，got %v", err)
	}
}

// 用允许目录内的符号链接指向目录外，必须被拒绝，否则允许目录形同虚设。
func TestResolveFileSecretRejectsSymlinkEscape(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "token")
	if err := os.WriteFile(target, []byte("escaped"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	link := filepath.Join(allowed, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	resolver := NewResolver([]string{allowed})
	_, err := resolver.Resolve(context.Background(), domain.SecretRef{Kind: domain.SecretKindFile, Name: link})
	if domain.CodeOf(err) != v1.CodeSecretUnresolved {
		t.Fatalf("符号链接逃逸必须被拒绝，got %v", err)
	}
}

func TestResolveRejectsDirectoryAndEmptyAndRelative(t *testing.T) {
	dir := t.TempDir()
	resolver := NewResolver([]string{dir})

	cases := map[string]domain.SecretRef{
		"目录":     {Kind: domain.SecretKindFile, Name: dir},
		"相对路径":   {Kind: domain.SecretKindFile, Name: "relative/token"},
		"不存在的文件": {Kind: domain.SecretKindFile, Name: filepath.Join(dir, "missing")},
		"未知类型":   {Kind: domain.SecretKind("vault"), Name: "x"},
		"空名称":    {Kind: domain.SecretKindEnv, Name: "  "},
	}
	for name, ref := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := resolver.Resolve(context.Background(), ref); err == nil {
				t.Fatal("必须被拒绝")
			}
		})
	}
}

func TestResolveEmptyFileSecretIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty")
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	resolver := NewResolver([]string{dir})
	if _, err := resolver.Resolve(context.Background(), domain.SecretRef{Kind: domain.SecretKindFile, Name: path}); domain.CodeOf(err) != v1.CodeSecretUnresolved {
		t.Fatalf("want SECRET_UNRESOLVED, got %v", err)
	}
}

func TestErrorMessagesDoNotLeakSecretValues(t *testing.T) {
	t.Setenv("FRZ_TEST_LEAK", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("do-not-leak-me"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	resolver := NewResolver([]string{dir})
	_, err := resolver.Resolve(context.Background(), domain.SecretRef{Kind: domain.SecretKindFile, Name: path})
	if err == nil {
		t.Fatal("期望失败")
	}
	if msg := domain.MessageOf(err); strings.Contains(msg, "do-not-leak-me") {
		t.Fatalf("错误信息泄漏了凭据值: %s", msg)
	}
	if msg := domain.MessageOf(err); strings.Contains(msg, path) {
		t.Fatalf("错误信息应当避免回显完整路径: %s", msg)
	}
}

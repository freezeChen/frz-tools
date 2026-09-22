package secret

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

const maxSecretBytes = 64 << 10

// Resolver 在使用时刻把 SecretRef 解析为明文。解析结果不落库、不落日志，并且会
// 被交给 Redactor 做按值脱敏。
type Resolver struct {
	allowedDirectories []string
	lookupEnv          func(string) (string, bool)
}

func NewResolver(allowedDirectories []string) *Resolver {
	return &Resolver{
		allowedDirectories: append([]string(nil), allowedDirectories...),
		lookupEnv:          os.LookupEnv,
	}
}

func (r *Resolver) Resolve(ctx context.Context, ref domain.SecretRef) (string, error) {
	if err := ref.Validate(); err != nil {
		return "", err
	}
	switch ref.Kind {
	case domain.SecretKindEnv:
		return r.resolveEnv(ref)
	case domain.SecretKindFile:
		return r.resolveFile(ctx, ref)
	default:
		return "", domain.NewError(v1.CodeSecretUnresolved, "unsupported secret kind %q", ref.Kind)
	}
}

func (r *Resolver) resolveEnv(ref domain.SecretRef) (string, error) {
	value, ok := r.lookupEnv(ref.Name)
	if !ok || value == "" {
		return "", domain.NewError(v1.CodeSecretUnresolved,
			"environment secret %q is not set", ref.Name)
	}
	return value, nil
}

func (r *Resolver) resolveFile(ctx context.Context, ref domain.SecretRef) (string, error) {
	// 错误信息只暴露文件名，不回显完整路径——完整 SecretRef 内容不得出现在响应里。
	label := filepath.Base(ref.Name)
	path := ref.Name
	if !filepath.IsAbs(path) {
		return "", domain.NewError(v1.CodeSecretUnresolved,
			"file secret %q must be an absolute path", label)
	}

	// 先解析符号链接再判断是否落在允许目录内，避免用链接逃逸。
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", domain.NewError(v1.CodeSecretUnresolved, "file secret %q cannot be resolved", label)
	}
	if !r.withinAllowedDirectory(resolved) {
		return "", domain.NewError(v1.CodeSecretUnresolved,
			"file secret %q is outside secrets.allowedFileDirectories", label)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", domain.NewError(v1.CodeSecretUnresolved, "file secret %q cannot be read", label)
	}
	if !info.Mode().IsRegular() {
		return "", domain.NewError(v1.CodeSecretUnresolved, "file secret %q is not a regular file", label)
	}
	// 只要对属组或其他用户开放就拒绝（0600 或更严格）。
	if info.Mode().Perm()&0o077 != 0 {
		return "", domain.NewError(v1.CodeSecretUnresolved,
			"file secret %q must not be readable by group or others (got %04o)", label, info.Mode().Perm())
	}
	if info.Size() > maxSecretBytes {
		return "", domain.NewError(v1.CodeSecretUnresolved,
			"file secret %q exceeds %d bytes", label, maxSecretBytes)
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", domain.NewError(v1.CodeSecretUnresolved, "file secret %q cannot be read", label)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// 凭据文件通常以换行结尾，去掉行尾空白以免污染环境变量值。
	value := strings.TrimRight(string(data), "\r\n")
	if value == "" {
		return "", domain.NewError(v1.CodeSecretUnresolved, "file secret %q is empty", label)
	}
	return value, nil
}

func (r *Resolver) withinAllowedDirectory(path string) bool {
	for _, dir := range r.allowedDirectories {
		resolvedDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			// 允许目录不存在或不可解析时，它不授权任何文件。
			if !errors.Is(err, fs.ErrNotExist) {
				continue
			}
			continue
		}
		if path == resolvedDir {
			return false
		}
		if strings.HasPrefix(path, resolvedDir+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

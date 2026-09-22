package domain

import (
	"sort"
	"strings"

	v1 "frz-tools/api/v1"
)

type SecretKind string

const (
	SecretKindEnv  SecretKind = "env"
	SecretKindFile SecretKind = "file"
)

// SecretRef 只描述「到哪里取凭据」，本身不含任何明文。配置文件、manifest 和
// 数据库中只允许出现引用。
type SecretRef struct {
	Kind SecretKind
	Name string
}

func (r SecretRef) Validate() error {
	switch r.Kind {
	case SecretKindEnv, SecretKindFile:
	default:
		return NewError(v1.CodeInvalidRequest, "unsupported secret kind %q", r.Kind)
	}
	if strings.TrimSpace(r.Name) == "" {
		return NewError(v1.CodeInvalidRequest, "secret name is required")
	}
	return nil
}

func (r SecretRef) String() string { return string(r.Kind) + ":" + r.Name }

// Redactor 按「值」脱敏。仅按 key 脱敏不足以覆盖 SecretRef：一旦凭据被解析出来
// 并出现在命令环境、stdout、stderr 或错误信息里，只有按值替换才能拦住它。
type Redactor struct {
	values []string
}

func NewRedactor(values ...string) *Redactor {
	seen := map[string]bool{}
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	// 先替换较长的值，避免短值截断长值导致残留。
	sort.Slice(unique, func(i, j int) bool { return len(unique[i]) > len(unique[j]) })
	return &Redactor{values: unique}
}

func (r *Redactor) Redact(text string) string {
	if r == nil || text == "" || len(r.values) == 0 {
		return text
	}
	for _, value := range r.values {
		text = strings.ReplaceAll(text, value, RedactedPlaceholder)
	}
	return text
}

func (r *Redactor) RedactFields(fields map[string]string) map[string]string {
	if r == nil || len(fields) == 0 || len(r.values) == 0 {
		return fields
	}
	out := make(map[string]string, len(fields))
	for key, value := range fields {
		out[key] = r.Redact(value)
	}
	return out
}

func (r *Redactor) Empty() bool { return r == nil || len(r.values) == 0 }

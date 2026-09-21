package application

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/domain"
)

// BuildCommandSpec 解析并校验指定 kind 的操作 spec。
func BuildCommandSpec(kind string, raw json.RawMessage, defaults Defaults) (domain.CommandSpec, error) {
	if kind != v1.KindExecutorCommand {
		return domain.CommandSpec{}, domain.NewError(v1.CodeInvalidRequest, "unsupported operation kind %q", kind)
	}
	if len(raw) == 0 {
		return domain.CommandSpec{}, domain.NewError(v1.CodeInvalidRequest, "spec is required for kind %q", kind)
	}

	var parsed v1.ExecutorCommandSpec
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&parsed); err != nil {
		return domain.CommandSpec{}, domain.NewError(v1.CodeInvalidRequest, "invalid spec: %v", err)
	}
	if len(parsed.Argv) == 0 || strings.TrimSpace(parsed.Argv[0]) == "" {
		return domain.CommandSpec{}, domain.NewError(v1.CodeInvalidRequest, "spec.argv must contain at least the executable path")
	}
	if !filepath.IsAbs(parsed.Argv[0]) {
		return domain.CommandSpec{}, domain.NewError(v1.CodeInvalidRequest, "spec.argv[0] must be an absolute path, got %q", parsed.Argv[0])
	}
	if parsed.WorkingDirectory != "" && !filepath.IsAbs(parsed.WorkingDirectory) {
		return domain.CommandSpec{}, domain.NewError(v1.CodeInvalidRequest, "spec.workingDirectory must be an absolute path, got %q", parsed.WorkingDirectory)
	}
	if parsed.MaxOutputBytes < 0 {
		return domain.CommandSpec{}, domain.NewError(v1.CodeInvalidRequest, "spec.maxOutputBytes must not be negative")
	}
	if parsed.TimeoutSeconds < 0 {
		return domain.CommandSpec{}, domain.NewError(v1.CodeInvalidRequest, "spec.timeoutSeconds must not be negative")
	}
	for _, i := range parsed.SensitiveArgIndexes {
		if i < 0 || i >= len(parsed.Argv) {
			return domain.CommandSpec{}, domain.NewError(v1.CodeInvalidRequest, "spec.sensitiveArgIndexes contains out-of-range index %d", i)
		}
	}

	return domain.CommandSpec{
		Argv:                parsed.Argv,
		WorkingDirectory:    parsed.WorkingDirectory,
		Environment:         parsed.Environment,
		Timeout:             timeSeconds(parsed.TimeoutSeconds),
		MaxOutputBytes:      parsed.MaxOutputBytes,
		SensitiveEnvKeys:    parsed.SensitiveEnvKeys,
		SensitiveArgIndexes: parsed.SensitiveArgIndexes,
	}, nil
}

// requestHash 对规范化后的请求做摘要，用于区分「幂等键重复且请求相同」
// 与「幂等键相同但请求不同」两种情况。
func requestHash(kind, resource string, dryRun bool, spec json.RawMessage) (string, error) {
	var canonical any
	if len(spec) > 0 {
		if err := json.Unmarshal(spec, &canonical); err != nil {
			return "", domain.NewError(v1.CodeInvalidRequest, "spec is not valid JSON: %v", err)
		}
	}
	payload, err := json.Marshal(map[string]any{
		"kind":     kind,
		"resource": resource,
		"dryRun":   dryRun,
		"spec":     canonical,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func timeSeconds(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

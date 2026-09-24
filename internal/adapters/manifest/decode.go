package manifest

import (
	"bytes"

	"gopkg.in/yaml.v3"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// maxMigrationSteps 防止迁移链配置错误导致死循环。
const maxMigrationSteps = 16

type envelope struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

// decodeManifest 是各版本化 manifest 共用的解码流程：
// 读 apiVersion 信封 → 校验 kind → 按版本走迁移链 → KnownFields(true) 严格解码。
//
// ApplicationSpec 与 BackupPolicy 共用它，是为了让「新增可选字段保持兼容，而删除、改名、
// 改变语义或必填性必须提升 apiVersion」这条规则在两处**完全一致**——各写一份必然漂移，
// 而漂移的表现恰好是「用户以为字段生效了、其实被静默丢掉」。
//
// 返回的 doc 是走完迁移链之后的文档，供 convert 回看「用户到底写没写某个字段」：
// 严格解码后的结构体区分不了「写了零值」与「没写」。
func decodeManifest(raw []byte, wantKind, targetVersion string, chain map[string]func(map[string]any) error, into any) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, domain.NewError(v1.CodeManifestInvalid, "manifest 内容为空")
	}

	var env envelope
	if err := yaml.Unmarshal(raw, &env); err != nil {
		return nil, domain.NewError(v1.CodeManifestInvalid, "manifest 不是合法 YAML: %v", err)
	}
	if env.APIVersion == "" {
		return nil, domain.NewError(v1.CodeManifestInvalid, "manifest 必须声明 apiVersion")
	}
	if env.Kind != wantKind {
		return nil, domain.NewError(v1.CodeManifestInvalid,
			"manifest 的 kind 必须是 %q，got %q", wantKind, env.Kind)
	}

	doc := map[string]any{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, domain.NewError(v1.CodeManifestInvalid, "manifest 不是合法 YAML: %v", err)
	}
	if err := migrateChain(doc, env.APIVersion, targetVersion, chain); err != nil {
		return nil, err
	}

	normalized, err := yaml.Marshal(doc)
	if err != nil {
		return nil, domain.NewError(v1.CodeManifestInvalid, "manifest 无法重新序列化: %v", err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(normalized))
	decoder.KnownFields(true)
	if err := decoder.Decode(into); err != nil {
		return nil, domain.NewError(v1.CodeManifestInvalid, "manifest 字段非法: %v", err)
	}
	return doc, nil
}

func migrateChain(doc map[string]any, from, target string, chain map[string]func(map[string]any) error) error {
	version := from
	for step := 0; version != target; step++ {
		if step >= maxMigrationSteps {
			return domain.NewError(v1.CodeManifestInvalid,
				"从 %q 开始的 manifest 迁移未收敛", from)
		}
		apply, ok := chain[version]
		if !ok {
			return domain.NewError(v1.CodeManifestInvalid,
				"不支持的 manifest apiVersion %q；本版本只理解 %q", from, target)
		}
		if err := apply(doc); err != nil {
			return domain.NewError(v1.CodeManifestInvalid, "从 %q 迁移 manifest 失败: %v", version, err)
		}
		next, _ := doc["apiVersion"].(string)
		if next == version {
			return domain.NewError(v1.CodeManifestInvalid,
				"从 %q 的 manifest 迁移没有推进 apiVersion", version)
		}
		version = next
	}
	return nil
}

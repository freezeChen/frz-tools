// Package manifest 负责解析应用的 ApplicationSpec（manifest）。
//
// 解码流程与配置版本化保持一致：先用非严格解码读出 apiVersion 信封，按版本走
// 迁移链，再用 KnownFields(true) 严格解码。这样做的目的是让「新增可选字段」保持
// 向后兼容，而使「删除、改名、改变语义或必填性」必须提升 apiVersion 并登记迁移——
// 否则用户会在完全不知道的情况下静默丢掉一个字段。
package manifest

import (
	"bytes"
	"io"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"

	"gopkg.in/yaml.v3"
)

// maxMigrationSteps 防止迁移链配置错误导致死循环。
const maxMigrationSteps = 16

// migrations 登记「从某个 apiVersion 升级到下一个版本」的函数。当前只有一版，
// 因此为空；新增版本时在此登记，不要直接改历史版本的语义。
var migrations = map[string]func(doc map[string]any) error{}

type envelope struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

type wireSpec struct {
	APIVersion  string       `yaml:"apiVersion"`
	Kind        string       `yaml:"kind"`
	Application string       `yaml:"application"`
	Runtime     string       `yaml:"runtime"`
	Artifact    wireArtifact `yaml:"artifact"`
	Exec        wireExec     `yaml:"exec"`
	Health      wireHealth   `yaml:"health"`
	Logs        wireLogs     `yaml:"logs"`
	Systemd     wireSystemd  `yaml:"systemd"`
}

type wireArtifact struct {
	ID     string      `yaml:"id"`
	Digest string      `yaml:"digest"`
	Unpack *wireUnpack `yaml:"unpack"`
}

type wireUnpack struct {
	Strategy        string `yaml:"strategy"`
	StripComponents int    `yaml:"stripComponents"`
}

type wireExec struct {
	Argv              []string                 `yaml:"argv"`
	WorkingDirectory  string                   `yaml:"workingDirectory"`
	RunUser           string                   `yaml:"runUser"`
	Environment       map[string]string        `yaml:"environment"`
	SecretEnvironment map[string]wireSecretRef `yaml:"secretEnvironment"`
	Ports             []int                    `yaml:"ports"`
}

type wireSecretRef struct {
	Kind string `yaml:"kind"`
	Name string `yaml:"name"`
}

type wireHealth struct {
	Readiness          wireReadiness `yaml:"readiness"`
	StartTimeoutSecond int           `yaml:"startTimeoutSeconds"`
	StopTimeoutSecond  int           `yaml:"stopTimeoutSeconds"`
}

type wireReadiness struct {
	Type                 string `yaml:"type"`
	Target               string `yaml:"target"`
	ConsecutiveSuccesses int    `yaml:"consecutiveSuccesses"`
}

type wireLogs struct {
	Directory string `yaml:"directory"`
}

type wireSystemd struct {
	UnitName      string `yaml:"unitName"`
	RestartPolicy string `yaml:"restartPolicy"`
}

func Parse(data []byte) (*domain.ApplicationSpec, error) {
	return ParseReader(bytes.NewReader(data))
}

func ParseReader(r io.Reader) (*domain.ApplicationSpec, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, domain.NewError(v1.CodeManifestInvalid, "读取 manifest 失败: %v", err)
	}
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
	if env.Kind != domain.ManifestKind {
		return nil, domain.NewError(v1.CodeManifestInvalid,
			"manifest 的 kind 必须是 %q，got %q", domain.ManifestKind, env.Kind)
	}

	doc := map[string]any{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, domain.NewError(v1.CodeManifestInvalid, "manifest 不是合法 YAML: %v", err)
	}
	if err := migrate(doc, env.APIVersion); err != nil {
		return nil, err
	}

	normalized, err := yaml.Marshal(doc)
	if err != nil {
		return nil, domain.NewError(v1.CodeManifestInvalid, "manifest 无法重新序列化: %v", err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(normalized))
	decoder.KnownFields(true)

	var wire wireSpec
	if err := decoder.Decode(&wire); err != nil {
		return nil, domain.NewError(v1.CodeManifestInvalid, "manifest 字段非法: %v", err)
	}

	spec := convert(&wire, doc)
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return spec, nil
}

func migrate(doc map[string]any, from string) error {
	version := from
	for step := 0; version != domain.ManifestAPIVersion; step++ {
		if step >= maxMigrationSteps {
			return domain.NewError(v1.CodeManifestInvalid,
				"从 %q 开始的 manifest 迁移未收敛", from)
		}
		apply, ok := migrations[version]
		if !ok {
			return domain.NewError(v1.CodeManifestInvalid,
				"不支持的 manifest apiVersion %q；本版本只理解 %q", from, domain.ManifestAPIVersion)
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

func convert(wire *wireSpec, doc map[string]any) *domain.ApplicationSpec {
	spec := &domain.ApplicationSpec{
		APIVersion:  wire.APIVersion,
		Kind:        wire.Kind,
		Application: strings.TrimSpace(wire.Application),
		Runtime:     domain.RuntimeKind(wire.Runtime),
		Artifact: domain.SpecArtifact{
			ID:     strings.TrimSpace(wire.Artifact.ID),
			Digest: strings.TrimSpace(wire.Artifact.Digest),
		},
		Exec: domain.SpecExec{
			Argv:             wire.Exec.Argv,
			WorkingDirectory: wire.Exec.WorkingDirectory,
			RunUser:          wire.Exec.RunUser,
			Environment:      wire.Exec.Environment,
			Ports:            wire.Exec.Ports,
		},
		Health: domain.SpecHealth{
			Readiness: domain.SpecReadiness{
				Type:                 domain.ReadinessType(wire.Health.Readiness.Type),
				Target:               wire.Health.Readiness.Target,
				ConsecutiveSuccesses: wire.Health.Readiness.ConsecutiveSuccesses,
			},
			StartTimeout: seconds(wire.Health.StartTimeoutSecond),
			StopTimeout:  seconds(wire.Health.StopTimeoutSecond),
		},
		Logs: domain.SpecLogs{Directory: wire.Logs.Directory},
		Systemd: domain.SpecSystemd{
			UnitName:      wire.Systemd.UnitName,
			RestartPolicy: domain.RestartPolicy(wire.Systemd.RestartPolicy),
		},
	}

	if wire.Artifact.Unpack != nil {
		spec.Artifact.Unpack = domain.SpecUnpack{
			Strategy:        domain.UnpackStrategy(wire.Artifact.Unpack.Strategy),
			StripComponents: wire.Artifact.Unpack.StripComponents,
		}
	}
	// 只有 YAML 里真的写了 strategy 才算显式声明，用于判定 MANIFEST_CONFLICT。
	// 这里刻意回看原始文档：严格解码后的结构体无法区分「写了 none」与「没写」。
	if artifact, ok := doc["artifact"].(map[string]any); ok {
		if unpack, ok := artifact["unpack"].(map[string]any); ok {
			_, spec.Artifact.Unpack.StrategyExplicit = unpack["strategy"]
		}
	}

	if len(wire.Exec.SecretEnvironment) > 0 {
		spec.Exec.SecretEnvironment = make(map[string]domain.SecretRef, len(wire.Exec.SecretEnvironment))
		for name, ref := range wire.Exec.SecretEnvironment {
			spec.Exec.SecretEnvironment[name] = domain.SecretRef{
				Kind: domain.SecretKind(ref.Kind),
				Name: ref.Name,
			}
		}
	}
	if spec.Exec.Environment == nil {
		spec.Exec.Environment = map[string]string{}
	}
	if spec.Exec.SecretEnvironment == nil {
		spec.Exec.SecretEnvironment = map[string]domain.SecretRef{}
	}
	return spec
}

// seconds 把 manifest 里的秒数转为 Duration。0 表示未声明，由
// ApplicationSpec.applyDefaults 填默认值。
func seconds(value int) time.Duration {
	if value <= 0 {
		return 0
	}
	return time.Duration(value) * time.Second
}

// Package manifest 负责解析版本化 manifest：应用的 ApplicationSpec 与备份的
// BackupPolicy。两者共用 decode.go 里的解码流程（信封 → 迁移链 → 严格解码）。
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
)

// appSpecMigrations 登记 ApplicationSpec「从某个 apiVersion 升级到下一个版本」的函数。
// 当前只有一版，因此为空；新增版本时在此登记，不要直接改历史版本的语义。
var appSpecMigrations = map[string]func(doc map[string]any) error{}

type wireSpec struct {
	APIVersion  string         `yaml:"apiVersion"`
	Kind        string         `yaml:"kind"`
	Application string         `yaml:"application"`
	Runtime     string         `yaml:"runtime"`
	Artifact    wireArtifact   `yaml:"artifact"`
	Exec        wireExec       `yaml:"exec"`
	Health      wireHealth     `yaml:"health"`
	Logs        wireLogs       `yaml:"logs"`
	Systemd     wireSystemd    `yaml:"systemd"`
	Resources   *wireResources `yaml:"resources"`
	Release     *wireRelease   `yaml:"release"`
}

// wireResources 是资源限制。指针类型用来区分「没写」与「写了全 0」——两者语义相同
// （不限制），因此这里其实只需要能表达「没有这一段」。
type wireResources struct {
	CPUQuotaPercent int   `yaml:"cpuQuotaPercent"`
	MemoryMaxBytes  int64 `yaml:"memoryMaxBytes"`
}

// wireRelease 是发布策略。
//
// keepLast 用指针：**没写**要走默认值，而 `keepLast: 0` 是非法值（制品永远在制品库里，
// 「永不清理 release 目录」没有真实价值）。指针才能把这两件事分开说清楚。
type wireRelease struct {
	KeepLast *int `yaml:"keepLast"`
}

type wireArtifact struct {
	ID       string      `yaml:"id"`
	Digest   string      `yaml:"digest"`
	Version  string      `yaml:"version"`
	FileName string      `yaml:"fileName"`
	Unpack   *wireUnpack `yaml:"unpack"`
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

	// 解码流程（信封 → 迁移链 → 严格解码）与 BackupPolicy 共用一份实现，
	// 见 decode.go 的 decodeManifest。
	var wire wireSpec
	doc, err := decodeManifest(raw, domain.ManifestKind, domain.ManifestAPIVersion, appSpecMigrations, &wire)
	if err != nil {
		return nil, err
	}

	spec := convert(&wire, doc)
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return spec, nil
}

func convert(wire *wireSpec, doc map[string]any) *domain.ApplicationSpec {
	spec := &domain.ApplicationSpec{
		APIVersion:  wire.APIVersion,
		Kind:        wire.Kind,
		Application: strings.TrimSpace(wire.Application),
		Runtime:     domain.RuntimeKind(wire.Runtime),
		Artifact: domain.SpecArtifact{
			ID:       strings.TrimSpace(wire.Artifact.ID),
			Digest:   strings.TrimSpace(wire.Artifact.Digest),
			Version:  strings.TrimSpace(wire.Artifact.Version),
			FileName: strings.TrimSpace(wire.Artifact.FileName),
		},
		Release: domain.SpecRelease{
			KeepLast: keepLastOrZero(wire.Release),
		},
		Resources: domain.SpecResources{
			CPUQuotaPercent: resourcesOrZero(wire.Resources).CPUQuotaPercent,
			MemoryMaxBytes:  resourcesOrZero(wire.Resources).MemoryMaxBytes,
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

// resourcesOrZero 把可能缺失的 resources 段折叠成零值——零值就是「不限制」，与「没写」
// 语义相同，因此这里不需要指针一路传到领域模型。
func resourcesOrZero(wire *wireResources) wireResources {
	if wire == nil {
		return wireResources{}
	}
	return *wire
}

// keepLastOrZero 返回声明的 keepLast；没写时返回 0，交由领域层的校验补默认值
// （校验发生在 applyDefaults 之前，因此「没写」必须走默认值这条路径）。
func keepLastOrZero(wire *wireRelease) int {
	if wire == nil || wire.KeepLast == nil {
		return 0
	}
	return *wire.KeepLast
}

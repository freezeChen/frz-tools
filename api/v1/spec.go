package v1

// ApplicationSpec 是 manifest 的版本化 wire 形态。字段名与 manifest 的 YAML 键逐字
// 对应（见迭代 1c 第 4 节），这样 `opsctl spec show --json` 的输出可以与用户手上的
// manifest.yaml 直接对照，不必在两套命名之间做映射。
type ApplicationSpec struct {
	APIVersion  string       `json:"apiVersion"`
	Kind        string       `json:"kind"`
	Application string       `json:"application"`
	Runtime     string       `json:"runtime"`
	Artifact    SpecArtifact `json:"artifact"`
	Exec        SpecExec     `json:"exec"`
	Health      SpecHealth   `json:"health"`
	Logs        SpecLogs     `json:"logs"`
	Systemd     SpecSystemd  `json:"systemd"`
}

// SpecArtifact 中 id 与 digest 恰好有且只有一个非空，因此都不写 omitempty 之外的
// 变体；unpack.strategy 在写入时已经填好默认值或按 mediaType 推断出的取值。
type SpecArtifact struct {
	ID     string     `json:"id,omitempty"`
	Digest string     `json:"digest,omitempty"`
	Unpack SpecUnpack `json:"unpack"`
}

type SpecUnpack struct {
	Strategy        string `json:"strategy"`
	StripComponents int    `json:"stripComponents,omitempty"`
}

type SpecExec struct {
	Argv              []string             `json:"argv"`
	WorkingDirectory  string               `json:"workingDirectory"`
	RunUser           string               `json:"runUser"`
	Environment       map[string]string    `json:"environment,omitempty"`
	SecretEnvironment map[string]SecretRef `json:"secretEnvironment,omitempty"`
	Ports             []int                `json:"ports,omitempty"`
}

type SpecHealth struct {
	Readiness           SpecReadiness `json:"readiness"`
	StartTimeoutSeconds int           `json:"startTimeoutSeconds"`
	StopTimeoutSeconds  int           `json:"stopTimeoutSeconds"`
}

type SpecReadiness struct {
	Type                 string `json:"type"`
	Target               string `json:"target"`
	ConsecutiveSuccesses int    `json:"consecutiveSuccesses"`
}

type SpecLogs struct {
	Directory string `json:"directory"`
}

type SpecSystemd struct {
	UnitName      string `json:"unitName"`
	RestartPolicy string `json:"restartPolicy"`
}

// PutApplicationSpecRequest 只收 manifest 的原始文本，不收已经结构化的 spec。
// 这样提交路径上就只有一个解码入口（internal/adapters/manifest.Parse），客户端
// 无法绕过严格解码与迁移链，也不会出现「同一份规格两种字段名」的第二套协议。
type PutApplicationSpecRequest struct {
	Manifest  string `json:"manifest"`
	UpdatedBy string `json:"updatedBy,omitempty"`
}

type ApplicationSpecResponse struct {
	APIVersion string          `json:"apiVersion"`
	Spec       ApplicationSpec `json:"spec"`
}

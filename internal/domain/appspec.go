package domain

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// ManifestAPIVersion 与 Kind 是 manifest 的版本信封，与配置版本化的做法一致：
// 先非严格解码读出版本，再按版本走迁移链，最后严格解码。
const (
	ManifestAPIVersion = "ops.frz.io/v1alpha1"
	ManifestKind       = "ApplicationSpec"
)

type RuntimeKind string

const (
	RuntimeKindGo   RuntimeKind = "go"
	RuntimeKindJava RuntimeKind = "java"
)

type UnpackStrategy string

const (
	UnpackNone  UnpackStrategy = "none"
	UnpackTar   UnpackStrategy = "tar"
	UnpackTarGz UnpackStrategy = "tar-gz"
	UnpackZip   UnpackStrategy = "zip"
)

// ReadinessType 刻意只有 tcp 与 http 两种：1c 的规格提到过 exec，但从未定义
// 它的 target 与参数语义，凭空实现一套未被冻结的语义比不支持更危险。
const (
	ReadinessTCP  ReadinessType = "tcp"
	ReadinessHTTP ReadinessType = "http"
)

type ReadinessType string

type RestartPolicy string

const (
	RestartAlways    RestartPolicy = "always"
	RestartOnFailure RestartPolicy = "on-failure"
	RestartNo        RestartPolicy = "no"
)

// applicationNamePattern 与 runUserPattern 是安全边界，不只是格式偏好：
// application 会进入 /etc/opsd/apps/<application>.env 与解包目录，
// runUser 会进入 useradd 的参数。放开字符集就等于放开路径穿越与参数注入。
var (
	applicationNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	runUserPattern         = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	unitNamePattern        = regexp.MustCompile(`^[A-Za-z0-9_.@-]+\.service$`)
)

type ApplicationSpec struct {
	APIVersion  string
	Kind        string
	Application string
	Runtime     RuntimeKind
	Artifact    SpecArtifact
	Exec        SpecExec
	Health      SpecHealth
	Logs        SpecLogs
	Systemd     SpecSystemd
}

type SpecArtifact struct {
	ID     string
	Digest string
	Unpack SpecUnpack
}

type SpecUnpack struct {
	Strategy UnpackStrategy
	// StrategyExplicit 记录 manifest 是否显式写了 strategy。区分这一点是必需的：
	// 只有显式声明才可能与制品的 mediaType 冲突（MANIFEST_CONFLICT），
	// 默认值不构成冲突。
	StrategyExplicit bool
	StripComponents  int
}

type SpecExec struct {
	Argv              []string
	WorkingDirectory  string
	RunUser           string
	Environment       map[string]string
	SecretEnvironment map[string]SecretRef
	Ports             []int
}

type SpecHealth struct {
	Readiness    SpecReadiness
	StartTimeout time.Duration
	StopTimeout  time.Duration
}

type SpecReadiness struct {
	Type                 ReadinessType
	Target               string
	ConsecutiveSuccesses int
}

type SpecLogs struct {
	Directory string
}

type SpecSystemd struct {
	UnitName      string
	RestartPolicy RestartPolicy
}

const (
	DefaultStartTimeout  = 60 * time.Second
	DefaultStopTimeout   = 30 * time.Second
	DefaultRestartPolicy = RestartOnFailure
)

func (s *ApplicationSpec) Validate() error {
	if !applicationNamePattern.MatchString(s.Application) {
		return NewError(v1.CodeManifestInvalid,
			"application 只允许字母、数字、点、下划线与连字符，且必须以字母或数字开头（got %q）", s.Application)
	}

	switch s.Runtime {
	case RuntimeKindGo, RuntimeKindJava:
	default:
		return NewError(v1.CodeManifestInvalid, "runtime 取值非法: %q", s.Runtime)
	}

	if err := s.Artifact.validate(); err != nil {
		return err
	}
	if err := s.Exec.validate(); err != nil {
		return err
	}
	if err := s.Health.validate(); err != nil {
		return err
	}
	if err := s.Logs.validate(); err != nil {
		return err
	}
	if err := s.Systemd.validate(); err != nil {
		return err
	}

	s.applyDefaults()
	return nil
}

func (a *SpecArtifact) validate() error {
	hasID := strings.TrimSpace(a.ID) != ""
	hasDigest := strings.TrimSpace(a.Digest) != ""
	if hasID == hasDigest {
		return NewError(v1.CodeManifestInvalid, "artifact 必须且只能提供 id 或 digest 之一")
	}
	if hasDigest && !strings.HasPrefix(a.Digest, "sha256:") {
		return NewError(v1.CodeManifestInvalid, "artifact.digest 只支持 sha256: 前缀（got %q）", a.Digest)
	}

	switch a.Unpack.Strategy {
	case UnpackNone, UnpackTar, UnpackTarGz, UnpackZip:
	case "":
		a.Unpack.Strategy = UnpackNone
	default:
		return NewError(v1.CodeManifestInvalid, "unpack.strategy 取值非法: %q", a.Unpack.Strategy)
	}
	if a.Unpack.StripComponents < 0 {
		return NewError(v1.CodeManifestInvalid, "unpack.stripComponents 不能为负数")
	}
	if a.Unpack.Strategy == UnpackNone && a.Unpack.StripComponents > 0 {
		return NewError(v1.CodeManifestInvalid,
			"unpack.strategy=none 时不能提供 stripComponents")
	}
	return nil
}

func (e *SpecExec) validate() error {
	if len(e.Argv) == 0 {
		return NewError(v1.CodeManifestInvalid, "exec.argv 不能为空")
	}
	for i, arg := range e.Argv {
		if arg == "" {
			return NewError(v1.CodeManifestInvalid, "exec.argv[%d] 不能为空字符串", i)
		}
	}
	if !path.IsAbs(e.Argv[0]) {
		return NewError(v1.CodeManifestInvalid, "exec.argv[0] 必须是绝对路径（got %q）", e.Argv[0])
	}
	if !runUserPattern.MatchString(e.RunUser) {
		return NewError(v1.CodeManifestInvalid,
			"exec.runUser 只允许小写字母、数字、下划线与连字符，且必须以小写字母或下划线开头（got %q）", e.RunUser)
	}
	if !path.IsAbs(e.WorkingDirectory) {
		return NewError(v1.CodeManifestInvalid,
			"exec.workingDirectory 必须是绝对路径（got %q）", e.WorkingDirectory)
	}

	for _, port := range e.Ports {
		if port < 1 || port > 65535 {
			return NewError(v1.CodeManifestInvalid, "exec.ports 中的端口越界: %d", port)
		}
	}

	// 同名键会让「这个变量到底是敏感的还是非敏感的」变成未定义行为，
	// 因此不允许在 environment 与 secretEnvironment 之间撞键。
	for name := range e.SecretEnvironment {
		if !validEnvName(name) {
			return NewError(v1.CodeManifestInvalid, "secretEnvironment 的键 %q 不是合法的环境变量名", name)
		}
		if _, clash := e.Environment[name]; clash {
			return NewError(v1.CodeManifestConflict,
				"环境变量 %q 同时出现在 environment 与 secretEnvironment 中", name)
		}
	}
	for name := range e.Environment {
		if !validEnvName(name) {
			return NewError(v1.CodeManifestInvalid, "environment 的键 %q 不是合法的环境变量名", name)
		}
	}
	for name, ref := range e.SecretEnvironment {
		// 刻意固定为 MANIFEST_INVALID 而不是透传 SecretRef 自己的错误码：
		// 用户提交的是 manifest，出错的也是 manifest，透传内部错误码会让
		// 调用方看到一个与它提交的东西不对应的错误类型。
		if err := ref.Validate(); err != nil {
			return NewError(v1.CodeManifestInvalid, "secretEnvironment[%q]: %v", name, err)
		}
	}
	return nil
}

func (h *SpecHealth) validate() error {
	switch h.Readiness.Type {
	case ReadinessTCP:
		if _, _, err := net.SplitHostPort(h.Readiness.Target); err != nil {
			return NewError(v1.CodeManifestInvalid,
				"readiness.type=tcp 时 target 必须是 host:port（got %q）", h.Readiness.Target)
		}
	case ReadinessHTTP:
		parsed, err := url.Parse(h.Readiness.Target)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return NewError(v1.CodeManifestInvalid,
				"readiness.type=http 时 target 必须是完整 URL（got %q）", h.Readiness.Target)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return NewError(v1.CodeManifestInvalid,
				"readiness.target 的协议只支持 http/https（got %q）", parsed.Scheme)
		}
	case "":
		return NewError(v1.CodeManifestInvalid, "health.readiness.type 不能为空")
	default:
		return NewError(v1.CodeManifestInvalid, "health.readiness.type 取值非法: %q", h.Readiness.Type)
	}

	if h.Readiness.ConsecutiveSuccesses < 0 {
		return NewError(v1.CodeManifestInvalid, "health.readiness.consecutiveSuccesses 不能为负数")
	}
	if h.StartTimeout < 0 || h.StopTimeout < 0 {
		return NewError(v1.CodeManifestInvalid, "健康检查超时不能为负数")
	}
	return nil
}

func (l *SpecLogs) validate() error {
	if !path.IsAbs(l.Directory) {
		return NewError(v1.CodeManifestInvalid, "logs.directory 必须是绝对路径（got %q）", l.Directory)
	}
	return nil
}

func (s *SpecSystemd) validate() error {
	if s.UnitName == "" {
		return nil
	}
	if !unitNamePattern.MatchString(s.UnitName) {
		return NewError(v1.CodeManifestInvalid,
			"systemd.unitName 必须以 .service 结尾且只允许字母、数字、下划线、点、@ 与连字符（got %q）", s.UnitName)
	}
	switch s.RestartPolicy {
	case RestartAlways, RestartOnFailure, RestartNo, "":
	default:
		return NewError(v1.CodeManifestInvalid, "systemd.restartPolicy 取值非法: %q", s.RestartPolicy)
	}
	return nil
}

func (s *ApplicationSpec) applyDefaults() {
	if s.Systemd.UnitName == "" {
		s.Systemd.UnitName = s.Application + ".service"
	}
	if s.Systemd.RestartPolicy == "" {
		s.Systemd.RestartPolicy = DefaultRestartPolicy
	}
	if s.Health.StartTimeout == 0 {
		s.Health.StartTimeout = DefaultStartTimeout
	}
	if s.Health.StopTimeout == 0 {
		s.Health.StopTimeout = DefaultStopTimeout
	}
	if s.Health.Readiness.ConsecutiveSuccesses == 0 {
		s.Health.Readiness.ConsecutiveSuccesses = 1
	}
}

// SecretEnvNames 按字典序返回 kind=env 的变量名，保证生成的敏感环境文件字节稳定，
// 便于比对与测试。
func (s *ApplicationSpec) SecretEnvNames() []string {
	names := make([]string, 0, len(s.Exec.SecretEnvironment))
	for name, ref := range s.Exec.SecretEnvironment {
		if ref.Kind == SecretKindEnv {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// SecretFileNames 按字典序返回 kind=file 的变量名（其值是指向落盘文件的路径）。
func (s *ApplicationSpec) SecretFileNames() []string {
	names := make([]string, 0, len(s.Exec.SecretEnvironment))
	for name, ref := range s.Exec.SecretEnvironment {
		if ref.Kind == SecretKindFile {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// MediaTypeUnpackStrategy 把制品的 mediaType 映射为默认解包方式。
func MediaTypeUnpackStrategy(mediaType string) UnpackStrategy {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "application/x-tar":
		return UnpackTar
	case "application/gzip", "application/x-gzip":
		return UnpackTarGz
	case "application/zip":
		return UnpackZip
	default:
		return UnpackNone
	}
}

// CheckUnpackMediaType 处理 manifest 的解包策略与制品 mediaType 的关系：
// 未显式声明时以 mediaType 推断的为准；显式声明且与 mediaType 推断出的具体策略
// 不同则报 MANIFEST_CONFLICT。mediaType 推断不出具体策略（裸二进制）时不算冲突——
// 此时用户比 mediaType 更清楚内容是什么。
func (s *ApplicationSpec) CheckUnpackMediaType(mediaType string) error {
	implied := MediaTypeUnpackStrategy(mediaType)
	if !s.Artifact.Unpack.StrategyExplicit {
		s.Artifact.Unpack.Strategy = implied
		return nil
	}
	if implied != UnpackNone && s.Artifact.Unpack.Strategy != implied {
		return NewError(v1.CodeManifestConflict,
			"manifest 声明 unpack.strategy=%s，但制品 mediaType %q 对应 %s",
			s.Artifact.Unpack.Strategy, mediaType, implied)
	}
	return nil
}

// 下面是环境文件、凭据目录与 release 目录的约定路径。集中在一处是为了让适配器、
// 测试与 harness 不会各自拼出不同的路径。
func EnvFilePath(application string) string {
	return path.Join("/etc/opsd/apps", application+".env")
}

func SecretsEnvFilePath(application string) string {
	return path.Join("/etc/opsd/apps", application+".secrets.env")
}

func SecretsDir(application string) string {
	return path.Join("/etc/opsd/apps", application+".secrets")
}

func ReleaseDir(application, releaseID string) string {
	return path.Join("/opt/opsd/apps", application, "releases", releaseID)
}

func UnitPath(unitName string) string {
	return path.Join("/etc/systemd/system", unitName)
}

// SecretFileVarValue 是 kind=file 的凭据在环境变量里承载的值：文件路径本身。
func SecretFileVarValue(application, name string) string {
	return path.Join(SecretsDir(application), name)
}

func (s *ApplicationSpec) String() string {
	return fmt.Sprintf("ApplicationSpec(%s/%s)", s.Application, s.Systemd.UnitName)
}

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
	// versionPattern 描述**版本标签**：允许 semver（1.2.3-rc1+build.5），不允许斜杠
	// ——它是标签不是路径，而带斜杠的版本号进日志与回滚引用都没有意义。
	versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
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
	Resources   SpecResources
	Release     SpecRelease
}

// SpecResources 是资源限制。字段是**运行时的意图**而不是 systemd 指令：翻译成
// CPUQuota= / MemoryMax= 是适配器的事（同一套意图将来要能落到别的运行时上）。
type SpecResources struct {
	// CPUQuotaPercent 是 CPU 配额，按百分比（100 = 一个核）。0 表示不限制。
	CPUQuotaPercent int
	// MemoryMaxBytes 是内存上限。0 表示不限制。
	MemoryMaxBytes int64
}

// SpecRelease 是发布策略。
type SpecRelease struct {
	// KeepLast 是保留最近多少个 release 目录（0 = 不自动清理）。
	KeepLast int
}

// 资源限制与发布保留的边界。放在这里而不是各调用点，是因为「多大算不合理」只有一处判断。
const (
	MaxCPUQuotaPercent = 10000
	MinMemoryMaxBytes  = 16 << 20
	MaxMemoryMaxBytes  = 1 << 40
	DefaultKeepLast    = 5
	MaxKeepReleases    = 100
)

type SpecArtifact struct {
	ID     string
	Digest string
	// Version 是**人类可读的版本号**，成为 Release.Version 的来源。可选：省略时由制品
	// digest 派生（见 DeriveVersion），因此本字段是在同一 apiVersion 内**新增的可选
	// 字段**（兼容）。把它做成必填会改变必填性，那要提升 apiVersion——而派生值同样唯一，
	// 不值得为它付一次破坏性变更。
	Version string
	// FileName 是 **unpack.strategy=none 时**制品字节在 release 目录里的文件名。
	//
	// 为什么需要它：单文件制品（Go 二进制、单个 JAR）没有归档自带的名字，而「叫什么」
	// 决定了 argv 里怎么写它。用 `argv[0]` 去猜文件名看着省事，但 Java 的 `argv[0]` 是
	// 解释器（绝对路径），猜不出 jar 该叫什么——而「单个 JAR」正是路线图点名要支持的形态。
	FileName string
	Unpack   SpecUnpack
}

// DeriveVersion 在 manifest 没写 artifact.version 时给出一个稳定的版本号。
//
// 用 digest 的前 12 位：同一个制品永远得到同一个版本号（这正是「版本号不可复用」想要
// 的性质），而不同制品几乎不可能撞上前 12 位十六进制（48 bit）。
func DeriveVersion(artifact SpecArtifact) string {
	source := artifact.Digest
	if source == "" {
		source = artifact.ID
	}
	source = strings.TrimPrefix(source, "sha256:")
	if len(source) > 12 {
		source = source[:12]
	}
	if source == "" {
		return "unversioned"
	}
	return "sha256-" + source
}

// VersionOrDerived 返回 manifest 声明的版本号，没写时用派生值。
func (a SpecArtifact) VersionOrDerived() string {
	if strings.TrimSpace(a.Version) != "" {
		return a.Version
	}
	return DeriveVersion(a)
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
	if err := s.Resources.validate(); err != nil {
		return err
	}
	if err := s.Release.validate(); err != nil {
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
	if hasDigest {
		// 声明了摘要就要是**合法**的摘要：前缀对、长度不对的写法今天能过校验、
		// 到部署时才以「找不到制品」失败，那是个把人带偏的报错。
		if _, err := ParseDigest(a.Digest); err != nil {
			return NewError(v1.CodeManifestInvalid, "artifact.digest 不是合法的摘要（got %q）", a.Digest)
		}
	}
	if a.Version != "" {
		// 版本号会进日志、审计与回滚引用；它**不是路径**，因此不允许斜杠。
		if !versionPattern.MatchString(a.Version) {
			return NewError(v1.CodeManifestInvalid,
				"artifact.version 只允许字母、数字、点、下划线、加号与连字符，且必须以字母或数字开头（got %q）", a.Version)
		}
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

	// fileName 与解包方式是**互斥的一对**：归档自带名字，用不着它；单文件制品没有名字，
	// 只能靠它。这条检查必须放在**补齐默认 strategy 之后**——否则「给了 fileName、没写
	// unpack」这种最常见的写法会被误判成「strategy 不是 none」。
	if a.FileName != "" && a.Unpack.Strategy != UnpackNone {
		return NewError(v1.CodeManifestInvalid,
			"artifact.fileName 只用于 unpack.strategy=none（归档自带条目名，写了它不会生效）")
	}
	return validateArtifactFileName(a.FileName)
}

// validate 只判「这个值是不是一个合理的限制」。0 一律表示不限制，因此不在这里翻译成
// 运行时的指令——那是适配器的事（同一套意图将来要能落到别的运行时上）。
func (r *SpecResources) validate() error {
	if r.CPUQuotaPercent < 0 || r.CPUQuotaPercent > MaxCPUQuotaPercent {
		return NewError(v1.CodeManifestInvalid,
			"resources.cpuQuotaPercent 取值范围是 0（不限制）到 %d（got %d）", MaxCPUQuotaPercent, r.CPUQuotaPercent)
	}
	if r.MemoryMaxBytes == 0 {
		return nil
	}
	if r.MemoryMaxBytes < MinMemoryMaxBytes || r.MemoryMaxBytes > MaxMemoryMaxBytes {
		// 比 16 MiB 还小的上限只会让进程一起来就被 OOM 杀掉，那是配置写错而不是意图。
		return NewError(v1.CodeManifestInvalid,
			"resources.memoryMaxBytes 要么是 0（不限制），要么不小于 %d 且不大于 %d（got %d）",
			MinMemoryMaxBytes, MaxMemoryMaxBytes, r.MemoryMaxBytes)
	}
	return nil
}

// validate 不接受 0（不清理）：制品永远在制品库里、重新解包即可，因此「永不清理 release
// 目录」没有真实价值，只会把盘慢慢占满。少一个取值，也少一处「没写 vs 显式写了 0」的歧义。
func (r *SpecRelease) validate() error {
	// 默认值在这里补，与 SpecUnpack 补默认 strategy 同一个位置与理由：校验发生在
	// applyDefaults 之前，而「没写」必须走默认值、不能走报错。
	if r.KeepLast == 0 {
		r.KeepLast = DefaultKeepLast
	}
	if r.KeepLast < 1 || r.KeepLast > MaxKeepReleases {
		return NewError(v1.CodeManifestInvalid,
			"release.keepLast 取值范围是 1 到 %d（got %d）", MaxKeepReleases, r.KeepLast)
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
	// argv 的元素遵循同一条解析规则（规格 D4）：**绝对路径原样、相对路径相对
	// release 的 current 目录**。
	//
	// 这里刻意**不**按 runtime 强制 argv[0] 的形态（「go 必须相对、java 必须绝对」是
	// 规格里原本的写法）：把 1c 起就被允许的「绝对 argv[0]」收紧成非法，属于**改变
	// 必填性/形态**的破坏性变更，要提升 apiVersion——而为一条风格约束付这个代价不值。
	// 「编译产物住在 release 里」作为**推荐用法**写在文档里，不在这里拦。
	// 只有 argv[0] 会被解析成路径（见 ResolveArgv），因此 `..` 规则也只对它生效：
	// 它是**逃出 release 目录**的直接手段，而它会变成 ExecStart=。
	// 其余元素一个字节都不动（它们可能是参数，见 ResolveArgv 的注释），因此不去管。
	for _, segment := range strings.Split(e.Argv[0], "/") {
		if segment == ".." {
			return NewError(v1.CodeManifestInvalid,
				"exec.argv[0] 不得包含 ..（got %q）", e.Argv[0])
		}
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

// ReleaseRootDir 返回制品解包目录的父目录（`/opt/opsd/apps/<application>/releases`）。
//
// 它是 release 目录与 current 指针的共同父目录，因此 **domain 是它唯一的定义处**：
// unit 渲染要把它写进 ReadWritePaths、runtime 适配器要建它、release 适配器要在它下面
// 解包、argv 解析要拼到 current 之下——四处各拼一次就等于把「约定」变成四个可能漂移的
// 实现。（1c 时它临时住在 runtime/unitfile 里，迭代 3 因为用的人多了才归位。）
func ReleaseRootDir(application string) string {
	const releaseIDPlaceholder = "unreleased"
	return path.Dir(ReleaseDir(application, releaseIDPlaceholder))
}

// CurrentReleaseDir 是 current 指针的位置：一个指向当前激活 release 的符号链接
// （迭代 3 规格 D3）。
func CurrentReleaseDir(application string) string {
	return path.Join(ReleaseRootDir(application), "current")
}

// InReleaseTree 报告 candidate 是否落在该应用的 releases 子树**内部**（子树根自身不算）。
//
// 它存在是因为一条容易踩到的分工：releases 子树归 ReleaseAdapter 所有——根由物化建、
// release 目录由解包建、`current` 由切换建。运行时适配器若「顺手把目录建出来」，就会在
// **第一次部署**时把 `exec.workingDirectory` 指向的 `current` 建成一个**实体目录**，
// 之后 Activate 的 `rename(current.tmp, current)` 会因为目标是目录而失败
// ——而那时报出来的错是「改名失败」，与真实原因（有人在建我该建的东西）毫无关系。
//
// 判据用 path.Clean：manifest 只保证「是绝对路径」，尾随斜杠会让前缀比较凭空失败。
func InReleaseTree(application, candidate string) bool {
	root := ReleaseRootDir(application)
	cleaned := path.Clean(candidate)
	if cleaned == root {
		return false
	}
	return strings.HasPrefix(cleaned, root+"/")
}

// ResolveArgv 把 manifest 里的 argv 解析成最终要执行的 argv（迭代 3 规格 D4）。
//
// **只解析 argv[0]**：绝对路径原样，相对路径拼到 release 的 current 之下。
// 其余元素**一个字节都不动**。
//
// 为什么只动 argv[0]（这是实现时对规格的修正，原写法是「每个元素都按同一条规则解析」）：
// 除了 argv[0]，没有任何办法判断一个参数**是不是路径**。`-Xmx512m`、`-Dlogging.file=/var/log/app.log`
// 看起来都像相对路径（后者还含斜杠），一旦被拼成 `<current>/-Xmx512m`，进程收到的是一个
// 被**静默改写**的参数——不是报错，是行为变了。而 argv[0] 非解析不可：systemd 要求它是
// 绝对路径（219 那一档尤其如此），而「相对路径」正是「制品里的那个可执行文件」的表达。
//
// 其余元素里若有相对路径（例如 `java -jar app.jar`），由**进程按自己的 workingDirectory**
// 解释——那是它本来的语义，我们不该抢。
func ResolveArgv(application string, argv []string) ([]string, error) {
	resolved := append([]string(nil), argv...)
	if len(resolved) == 0 || path.IsAbs(resolved[0]) {
		return resolved, nil
	}

	base := CurrentReleaseDir(application)
	joined := path.Join(base, resolved[0])
	if joined != base && !strings.HasPrefix(joined, base+"/") {
		// 相对元素里的 .. 在 SpecExec.validate 里已经被拒（那道防线更早），
		// 这里再确认一次解析结果仍落在 current 之下：**只有这里的检查真的在拼路径**。
		return nil, NewError(v1.CodeManifestInvalid,
			"exec.argv[0] 解析后逃出了 release 目录（%q → %q）", resolved[0], joined)
	}
	resolved[0] = joined
	return resolved, nil
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

// validateArtifactFileName 校验单文件制品的落点名字（空串表示没写，直接通过）。
//
// 它是**安全边界**：这个名字会被拼进 release 目录，因此必须是干净的相对路径。
// 与备份适配器拒绝路径穿越是同一条理由，只是方向相反——那边是往归档里放，这边是从
// 归档里取出来放。
func validateArtifactFileName(name string) error {
	if name == "" {
		return nil
	}
	if path.IsAbs(name) || strings.HasPrefix(name, "/") {
		return NewError(v1.CodeManifestInvalid, "artifact.fileName 必须是相对路径（got %q）", name)
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return NewError(v1.CodeManifestInvalid, "artifact.fileName 不得逃出 release 目录（got %q）", name)
	}
	if cleaned != name {
		// 归一化之后与原文不同，说明写了 `a/./b` 或 `a//b` 这类写法；它们在不同工具里
		// 可能得到不同结果，不如要求写干净的那一种。
		return NewError(v1.CodeManifestInvalid,
			"artifact.fileName 必须是已经归一化的路径（got %q，应为 %q）", name, cleaned)
	}
	return nil
}

// MaterializedFileName 返回 strategy=none 时制品字节在 release 目录里的落点；
// 返回空串表示**这份 manifest 不需要物化任何东西**。
//
// 三条规则，都是为了让「迭代 3 的物化」与「1c 时代的手工部署」共存：
//
//  1. 显式写了 artifact.fileName → 用它（单个 JAR 就是这种情况：argv[0] 是解释器，
//     猜不出 jar 该叫什么）；
//  2. 没写、而 argv[0] 是**相对路径** → 用 argv[0]（Go 的典型形态：制品就是那个二进制，
//     名字与 argv[0] 本来就是同一个）；
//  3. 都没写、argv[0] 又是**绝对路径** → 空串：这份 manifest 声明的是「跑一个住在别处的
//     程序」（1c 时期唯一的形态），物化没有意义，release 目录会是空的。
//
// 第 3 条是**兼容性**要求，不是设计偏好：把它判成非法会让 1c 起就合法的 manifest 全部
// 提交不了，而那是破坏性变更。
func (s *ApplicationSpec) MaterializedFileName() string {
	if s.Artifact.Unpack.Strategy != UnpackNone {
		return ""
	}
	if s.Artifact.FileName != "" {
		return s.Artifact.FileName
	}
	if len(s.Exec.Argv) > 0 && !path.IsAbs(s.Exec.Argv[0]) {
		return s.Exec.Argv[0]
	}
	return ""
}

// Materializes 报告这份 manifest 是否要求把制品解出内容来。
//
// 部署流程用它在「没有东西可物化」时跳过物化与切换：那种 manifest 的 argv[0] 指向的是
// 发布目录之外的既有程序（1c 的形态），给它建一个空 release 目录、再把 current 指过去，
// 只会让「现在跑的是哪个版本」这句话变成假的。
func (s *ApplicationSpec) Materializes() bool {
	if s.Artifact.Unpack.Strategy != UnpackNone {
		return true
	}
	return s.MaterializedFileName() != ""
}

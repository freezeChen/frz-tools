package unitfile

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// Tier 是 systemd unit 模板的兼容档。档位由目标主机上探测到的 systemd 版本决定，
// 不能由调用方随意挑选：规格 §6 里的指令在旧版本上要么不存在、要么取值不被接受，
// 取值错误会让 unit 直接加载失败（不是被忽略），应用根本起不来。
type Tier string

const (
	// TierStrict 对应 systemd ≥ 240：ProtectSystem=strict + ReadWritePaths= + append: 日志。
	TierStrict Tier = "strict"
	// TierLegacy 覆盖 219～239：只能降级为 ProtectSystem=yes + ReadWriteDirectories= + journal 日志。
	TierLegacy Tier = "legacy"
)

const (
	// MinSupportedSystemdVersion 是承诺支持的最低版本（CentOS 7 起为 219）。
	MinSupportedSystemdVersion = 219
	// TierStrictMinVersion 由 StandardOutput=append: 的最低版本决定（240），
	// 它是两档里门槛最高的那条指令。
	TierStrictMinVersion = 240
)

// RestartSec 是重启间隔。systemd 默认 100ms 对「崩溃即重启」的循环来说太快，
// 会把日志刷爆，规格 §6 因此固定为 5 秒，这里保持同一取值。
const RestartSec = 5 * time.Second

// 未验证声明：legacy 档没有在任何真实 Linux 主机或 Linux 容器上跑过。
// 开发机是 macOS，没有 systemd；仓库的容器验证（make verify-linux）也未覆盖该档。
// 它的取值依据只有规格 §6 的兼容矩阵与 systemd 官方文档，所以这里不得写成「已支持 219」，
// 需要真机验证后才能在文档里升级这句话。
const tierLegacyUnverifiedNote = "legacy 档未在真实主机/容器验证"

// TierFor 把探测到的 systemd 主版本映射为档位。
// 低于最低支持版本时必须报错，不能挑一个「最接近的档」硬塞：
// 那会生成一个在目标主机上必定加载失败的 unit，而失败现场在几百公里外的机器上。
func TierFor(systemdVersion int) (Tier, error) {
	switch {
	case systemdVersion >= TierStrictMinVersion:
		return TierStrict, nil
	case systemdVersion >= MinSupportedSystemdVersion:
		return TierLegacy, nil
	}
	return "", domain.NewError(v1.CodeRuntimeUnsupport,
		"探测到的 systemd 版本为 %d，低于最低支持版本 %d，拒绝生成 unit", systemdVersion, MinSupportedSystemdVersion)
}

// Degradations 列出该档位相对 strict 档的语义损失。审计必须记录它们，
// 否则「同一份 manifest 在不同主机上生成的 unit 不同」这件事无迹可查。
func (t Tier) Degradations() []string {
	switch t {
	case TierStrict:
		return nil
	case TierLegacy:
		return []string{
			"日志改投 journal：legacy 档不支持 StandardOutput=append:，无法实现「每次启动不截断」的追加语义；opsd 当前按文件读日志，按 Operation 查日志需要改走 journalctl。",
			"文件系统防护降级为 ProtectSystem=yes：写权限不再被限制在显式列出的路径上，隔离强度低于 strict 档。",
			tierLegacyUnverifiedNote,
		}
	}
	return []string{"未知档位 " + string(t) + "：" + tierLegacyUnverifiedNote}
}

// Valid 报告 t 是否是本包认识的档位。
func (t Tier) Valid() bool {
	return t == TierStrict || t == TierLegacy
}

// Rendered 是 unit 渲染结果。除正文外还带上档位与探测到的版本：
// 调用方（systemd 适配器）必须把这两项写进审计，因为它们决定了这台主机上的 unit 长什么样。
type Rendered struct {
	Content        string
	Tier           Tier
	SystemdVersion int
	Degradations   []string
	// Argv 是**解析之后**的 argv（相对路径已拼到 release 的 current 之下）。
	//
	// 它随渲染结果一起交出去，是为了让「实际会执行什么」只有一个来源：proc 适配器要
	// 记录它、审计想看它、断言要钉它。让调用方各自再解析一遍，就是把这件既有安全含义
	// （argv 会变成 ExecStart=）又容易写错的事复制成多份。
	Argv []string
}

// RenderUnit 按 tier 渲染 spec 对应的 unit 文件正文。
//
// systemdVersion 是探测到的版本，与 tier 必须自洽（TierFor 的结果），
// 否则头部注释会记录一个与正文不符的档位——可追溯信息一旦不真实就毫无价值。
func RenderUnit(spec *domain.ApplicationSpec, tier Tier, systemdVersion int) (Rendered, error) {
	if spec == nil {
		return Rendered{}, domain.NewError(v1.CodeManifestInvalid, "渲染 unit 需要非空的应用规格")
	}
	selected, err := TierFor(systemdVersion)
	if err != nil {
		return Rendered{}, err
	}
	if tier != selected {
		return Rendered{}, domain.NewError(v1.CodeManifestInvalid,
			"systemd %d 应使用 %s 档，不能按 %s 档渲染", systemdVersion, selected, tier)
	}

	// 渲染前先校验，而不是假定调用方校验过：Restart=、Timeout*Sec= 的取值来自
	// applyDefaults，未校验的规格会把它们渲染成空值，而空值的 systemd 指令会让 unit 加载失败。
	// 注意 spec.Validate 会就地把缺省值写回规格（UnitName、RestartPolicy、两个超时），
	// 这是它既有的语义，渲染层复用它而不是自己再推一遍默认值。
	if err := spec.Validate(); err != nil {
		return Rendered{}, err
	}

	// argv 在这里解析（迭代 3 规格 D4）：绝对路径原样、相对路径相对 release 的
	// current 目录。**这是 argv 最后一次被使用的地方**，把解析放在这里而不是散在调用点，
	// 是因为「怎么解析」与「谁来执行」是同一条信息——分开就会漂移，而漂移的后果是
	// 启动一个不该启动的东西。
	resolvedArgv, err := domain.ResolveArgv(spec.Application, spec.Exec.Argv)
	if err != nil {
		return Rendered{}, err
	}

	writable := []string{
		spec.Exec.WorkingDirectory,
		spec.Logs.Directory,
		domain.ReleaseRootDir(spec.Application),
	}
	for _, p := range writable {
		if err := checkRenderablePath("unit 中的路径", p); err != nil {
			return Rendered{}, err
		}
	}

	startSeconds, err := wholeSeconds("health.startTimeoutSeconds", spec.Health.StartTimeout)
	if err != nil {
		return Rendered{}, err
	}
	stopSeconds, err := wholeSeconds("health.stopTimeoutSeconds", spec.Health.StopTimeout)
	if err != nil {
		return Rendered{}, err
	}

	var b strings.Builder
	writeHeader(&b, tier, systemdVersion)

	b.WriteString("\n[Unit]\n")
	fmt.Fprintf(&b, "Description=%s\n", spec.Application)
	b.WriteString("After=network.target\n")
	b.WriteString("Wants=network.target\n")

	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "User=%s\n", spec.Exec.RunUser)
	// Group 与 User 同名：1c 不为应用建组，useradd --system 会创建同名主组。
	fmt.Fprintf(&b, "Group=%s\n", spec.Exec.RunUser)
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", spec.Exec.WorkingDirectory)
	// 两个 EnvironmentFile 的 `-` 前缀是有意区别对待的：非敏感文件缺失可以继续，
	// 敏感文件缺失必须让 unit 启动失败，否则应用会在缺凭据的状态下起来。
	fmt.Fprintf(&b, "EnvironmentFile=-%s\n", domain.EnvFilePath(spec.Application))
	fmt.Fprintf(&b, "EnvironmentFile=%s\n", domain.SecretsEnvFilePath(spec.Application))
	fmt.Fprintf(&b, "ExecStart=%s\n", EscapeArgs(resolvedArgv))
	fmt.Fprintf(&b, "Restart=%s\n", spec.Systemd.RestartPolicy)
	fmt.Fprintf(&b, "RestartSec=%d\n", int(RestartSec/time.Second))
	fmt.Fprintf(&b, "TimeoutStartSec=%d\n", startSeconds)
	fmt.Fprintf(&b, "TimeoutStopSec=%d\n", stopSeconds)
	b.WriteString("NoNewPrivileges=true\n")
	b.WriteString("PrivateTmp=true\n")

	// 加固指令按规格 §6 模板的顺序排列（ProtectSystem → ProtectHome → 可写路径 → 日志）。
	// ProtectHome=true 自 214 起可用，两档一致；取值 tmpfs 需要 232，规格明确禁止使用。
	currentLog := path.Join(spec.Logs.Directory, "current.log")
	var hardening []string
	switch tier {
	case TierStrict:
		hardening = []string{
			"ProtectSystem=strict",
			"ProtectHome=true",
			"ReadWritePaths=" + strings.Join(writable, " "),
			"StandardOutput=append:" + currentLog,
			"StandardError=append:" + currentLog,
		}
	case TierLegacy:
		// 旧档的 ProtectSystem 只能用 yes/full：strict 在 240 之前取值不被接受，unit 会加载失败；
		// ReadWritePaths= 在 231 之前被忽略，与 ProtectSystem=yes 组合会让应用写不了自己的工作目录，
		// 因此必须退回 ReadWriteDirectories=。
		hardening = []string{
			"ProtectSystem=yes",
			"ProtectHome=true",
			"ReadWriteDirectories=" + strings.Join(writable, " "),
			"StandardOutput=journal",
			"StandardError=journal",
		}
	}
	for _, line := range hardening {
		b.WriteString(line + "\n")
	}

	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")

	return Rendered{
		Content:        b.String(),
		Tier:           tier,
		SystemdVersion: systemdVersion,
		Degradations:   tier.Degradations(),
		Argv:           resolvedArgv,
	}, nil
}

// RenderForVersion 是给定 systemd 版本时的入口：先选档再渲染。
// 需要先把档位展示给用户或写进审计的调用方可以直接用 RenderUnit。
func RenderForVersion(spec *domain.ApplicationSpec, systemdVersion int) (Rendered, error) {
	tier, err := TierFor(systemdVersion)
	if err != nil {
		return Rendered{}, err
	}
	return RenderUnit(spec, tier, systemdVersion)
}

// RenderForHost 把探测与渲染串起来，是 systemd 适配器最常用的入口。
func RenderForHost(ctx context.Context, spec *domain.ApplicationSpec, prober *Prober) (Rendered, error) {
	version, err := prober.Detect(ctx)
	if err != nil {
		return Rendered{}, err
	}
	return RenderForVersion(spec, version)
}

// writeHeader 写可追溯头注释：探测到的版本与所选档位。
// 规格要求「同一份 manifest 在不同主机上生成的 unit 可能不同」这件事必须留痕。
func writeHeader(b *strings.Builder, tier Tier, systemdVersion int) {
	b.WriteString("# 本文件由 opsd 生成，请勿手工编辑；下次 Prepare 会按规格 1c 第 6 节重写。\n")
	fmt.Fprintf(b, "# systemd 版本=%d 档位=%s\n", systemdVersion, tier)
	if tier != TierLegacy {
		return
	}
	b.WriteString("# 档位 legacy（systemd 219～239）：本机无法使用 ProtectSystem=strict、\n")
	b.WriteString("# ReadWritePaths= 与 StandardOutput=append:，因此降级为 ProtectSystem=yes、\n")
	b.WriteString("# ReadWriteDirectories= 与 journal。语义损失：日志不再追加到 <logs.directory>/current.log，\n")
	b.WriteString("# 按 Operation 查日志需要走 journalctl（opsd 当前按文件读日志）。\n")
	b.WriteString("# " + tierLegacyUnverifiedNote + "。\n")
}

// ReleaseRootDir 返回制品解包目录的父目录。1c 只固定了 domain.ReleaseDir 的路径约定
// （/opt/opsd/apps/<application>/releases/<releaseID>），release 目录本身属于迭代 3。
// 把整个 releases 根纳入可写路径，后续无论解包到哪个 releaseID 都在授权范围内，
// 不必每发一个版本就重写 unit；占位 releaseID 只是取出父目录的手段，
// 路径约定的唯一来源仍然是 domain.ReleaseDir。
//
// 导出它是因为 systemd 适配器既要把它写进 ReadWritePaths，又要按同样的路径建目录：
// 两处各拼一次路径就等于把「约定」变成了两个可能漂移的实现。
func ReleaseRootDir(application string) string {
	const releaseIDPlaceholder = "unreleased"
	return path.Dir(domain.ReleaseDir(application, releaseIDPlaceholder))
}

// checkRenderablePath 拒绝会在 unit 里被解释成别的东西的路径。
//
// 这是安全性检查，不是格式偏好：unit 是按行解析的文本，而 manifest 只校验了路径是绝对路径，
// 因此路径里的换行会直接变成一条新指令（等于让 manifest 作者写任意 unit 配置）；
// ReadWritePaths= 是按空白切分的列表，路径里的空白会把一个路径拆成两个不存在的路径，
// 让 unit 加载失败；引号与反斜杠会被 systemd 按 C 转义解析，把路径悄悄改写。
// 这些指令的引号规则没有在真实主机上验证过，所以宁可显式拒绝，也不发明一种写法。
func checkRenderablePath(field, value string) error {
	if value == "" {
		return domain.NewError(v1.CodeManifestInvalid, "%s 不能为空", field)
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c <= 0x20 || c == 0x7f || c == '"' || c == '\'' || c == '\\' {
			return domain.NewError(v1.CodeManifestInvalid,
				"%s 含有不能在 unit 里原样书写的字节 0x%02x（%q）：换行会变成新指令，空白会切开路径列表，引号与反斜杠会被 systemd 转义改写",
				field, c, value)
		}
	}
	return nil
}

// wholeSeconds 把时长取整成 systemd 要的秒数。manifest 的字段本就是整秒
// （见 manifest 适配器的 startTimeoutSeconds），出现亚秒说明上游变了语义，
// 这时宁可报错，也不要悄悄截断成一个比用户声明更短的超时。
func wholeSeconds(field string, d time.Duration) (int64, error) {
	if d < 0 {
		return 0, domain.NewError(v1.CodeManifestInvalid, "%s 不能为负数（got %s）", field, d)
	}
	if d%time.Second != 0 {
		return 0, domain.NewError(v1.CodeManifestInvalid,
			"%s 必须是整秒（got %s）：渲染层不做亚秒截断", field, d)
	}
	return int64(d / time.Second), nil
}

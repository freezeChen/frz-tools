// Package systemd 是 RuntimeAdapter 在真实 systemd 上的实现：把一份 ApplicationSpec
// 落成专用用户、目录、环境文件、凭据与 systemd unit，并用 systemctl 完成启停、
// 状态查询与就绪检查。
//
// 三个刻意的设计点：
//
//   - 所有外部命令都走可注入的 Runner（argv-only、无 shell）。macOS 上没有 systemctl，
//     单元测试整段替换掉「怎么执行命令」，断言的是命令序列本身。
//   - 所有绝对路径都经过 RootPath 前缀重定向（生产为 "/"）。这样无特权环境也能断言
//     目录、环境文件、凭据文件与 unit 的真实权限与内容，这些断言才可能在本地跑。
//   - 传给 systemd 与应用的路径永远是规格里的真实路径；RootPath 只重定向 opsd 自己的
//     文件操作，不参与 unit 文本生成。
//
// 验证状态（仓库约定，未验证必须显式标注）：
//
//   - legacy unit 档**已在真实的 systemd 219（CentOS 7 / cgroup v1）上验证**（2026-09-24，
//     118 项通过 / 0 项失败，含重启验证）；232～239 那一段仍无对应版本的主机。
//   - 真实主机上的重启后 unit 持久化**已验证**；SELinux/AppArmor 与 sudoers/PAM 语义未验证。
//   - 本包的单元测试跑在 macOS 上：断言的是命令序列、文件内容与权限，不是 systemd 的
//     实际行为——真机与容器上的行为由 test/host 与 test/linux 覆盖。
package systemd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/runtime/readiness"
	"github.com/freezeChen/frz-tools/internal/adapters/runtime/unitfile"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/sysuser"
)

const (
	systemctlBin = "systemctl"
	useraddBin   = "useradd"
	idBin        = "id"

	// nologinShell 是规格 §7 指定的登录壳：运行用户不应该能登录。
	nologinShell = "/usr/sbin/nologin"

	// unitMode 是 unit 文件的模式（规格 §6）：它必须能被 systemd 读到，
	// 也必须是只读的——可写等于谁能改 unit 谁就能以运行用户的身份执行任意命令。
	unitMode = 0o644
	// appDirMode 是工作目录、日志目录与解包目录的模式（规格 §7）。
	appDirMode = 0o750
	// secretDirMode 是凭据目录的模式（规格 §7）。
	secretDirMode = 0o700
	// secretFileMode 是两个环境文件与每个凭据文件的模式（规格 §7）。
	secretFileMode = 0o600
)

// UnitDecision 是一次 unit 渲染的可追溯记录：档位、探测到的 systemd 版本与降级说明。
// 规格明确要求探测到的版本与所选档位可追溯——同一份 manifest 在不同主机上会生成不同的
// unit，没有这份记录就无从解释「为什么这台机器上的 unit 长得不一样」。
type UnitDecision struct {
	Application    string
	UnitName       string
	UnitPath       string
	Tier           unitfile.Tier
	SystemdVersion int
	Degradations   []string
	DecidedAt      time.Time
}

// Option 是适配器的可注入项。生产不传任何 Option，测试用它们替换掉平台判定、
// 命令执行、属主解析与时钟。
type Option func(*Adapter)

// WithRunner 替换命令执行方式。
func WithRunner(runner Runner) Option {
	return func(a *Adapter) { a.runner = runner }
}

// WithGOOS 替换平台判定。测试用它把 macOS 伪装成 Linux——否则整个适配器在开发机上
// 一步都走不下去；这也让「非 Linux 必须报 RUNTIME_UNSUPPORTED」这条能被断言。
func WithGOOS(goos string) Option {
	return func(a *Adapter) { a.goos = goos }
}

// WithOwnerResolver 替换属主解析。
func WithOwnerResolver(resolver sysuser.Resolver) Option {
	return func(a *Adapter) { a.owner = resolver }
}

// WithClock 替换时钟：启动超时判定必须能被测试推到未来，否则那条路径只能靠等。
func WithClock(now func() time.Time) Option {
	return func(a *Adapter) { a.now = now }
}

// WithProbeTimeout 替换单次就绪探测超时。
func WithProbeTimeout(d time.Duration) Option {
	return func(a *Adapter) {
		if d > 0 {
			a.probe = d
		}
	}
}

// WithEUID 替换「本进程的有效用户」判定：它决定哪些目录属于我们、因而允许被修复
// （见 ensureCredentialReachable 的「只补穿越位」那一类）。
func WithEUID(euid int) Option {
	return func(a *Adapter) { a.euid = func() int { return euid } }
}

// WithLogger 注入日志出口。档位与探测到的 systemd 版本必须能被追溯，日志是它
// 最直接的落点；不注入时用丢弃式 logger（与 httpapi 适配器的做法一致），
// 决策本身仍留在内存里，可通过 Adapter.UnitDecision 查询。
func WithLogger(logger *slog.Logger) Option {
	return func(a *Adapter) { a.log = logger }
}

// Adapter 是 systemd 运行时适配器。
type Adapter struct {
	root     string
	runner   Runner
	resolver application.SecretResolver
	owner    sysuser.Resolver
	goos     string
	now      func() time.Time
	probe    time.Duration
	euid     func() int
	log      *slog.Logger

	mu             sync.Mutex
	systemdVersion int
	startedAt      map[string]time.Time
	becameReady    map[string]bool
	decisions      map[string]UnitDecision

	// successes 与 proc 适配器共用同一份「连续成功」语义，见 readiness 包。
	successes *readiness.Tracker
}

// New 构造适配器。root 是路径前缀，生产传 "/"；传空字符串等同于 "/"。
// resolver 在使用时刻解析凭据，解析结果只用于落盘，不进日志、不进审计。
func New(root string, resolver application.SecretResolver, opts ...Option) *Adapter {
	adapter := &Adapter{
		root:        root,
		runner:      CommandRunner,
		resolver:    resolver,
		owner:       sysuser.OSLookup,
		goos:        runtime.GOOS,
		now:         func() time.Time { return time.Now().UTC() },
		probe:       readiness.DefaultTimeout,
		euid:        os.Geteuid,
		successes:   readiness.NewTracker(),
		startedAt:   map[string]time.Time{},
		becameReady: map[string]bool{},
		decisions:   map[string]UnitDecision{},
	}
	if adapter.root == "" {
		adapter.root = string(filepath.Separator)
	}
	for _, opt := range opts {
		opt(adapter)
	}
	return adapter
}

// RootPath 返回规格里的绝对路径在本适配器下的实际位置。
// 导出它是为了让测试断言真实产物，而不必复制一遍前缀规则——复制规则意味着
// 规则一旦调整，测试会继续「通过」却指向错误的文件。
func (a *Adapter) RootPath(absolute string) string {
	return filepath.Join(a.root, absolute)
}

// UnitDecision 返回最近一次 Prepare 记录的决策，供审计与排障使用。
func (a *Adapter) UnitDecision(unitName string) (UnitDecision, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	decision, ok := a.decisions[unitName]
	return decision, ok
}

// Validate 在 validateSpec 之上再加一条**主机事实**的检查：java 解释器在不在
// （迭代 3c）。它不进 validateSpec，是因为 Stop / Status / Health 走 validateSpec
// ——见那个方法的注释。
func (a *Adapter) Validate(ctx context.Context, spec *domain.ApplicationSpec) error {
	if err := a.validateSpec(ctx, spec); err != nil {
		return err
	}
	return unitfile.PreflightInterpreter(spec)
}

// validateSpec 是「平台 + 规格 + 这台主机能不能被管起来」那一层，**不碰 argv[0]**。
//
// Stop / Status / Health 刻意只走这一层：解释器或可执行文件从盘上消失（JDK 被卸载、
// 制品目录被人手工删了）时，服务仍然必须**能停下来、状态仍然必须问得出来**。
// 「工具在最需要它的时候拒绝工作」是最坏的一种失败——那时运维手边只有 systemctl，
// 而他用这个工具正是为了不必手工敲那些命令。
//
// 平台检查放在最前：在一台没有 systemd 的机器上讨论 manifest 字段是否合法没有意义，
// 而「不支持」才是调用方需要立刻知道的事。
func (a *Adapter) validateSpec(ctx context.Context, spec *domain.ApplicationSpec) error {
	if a.goos != "linux" {
		return domain.NewError(v1.CodeRuntimeUnsupport,
			"systemd 适配器只支持 Linux（当前平台 %s）", a.goos)
	}
	if spec == nil {
		return domain.NewError(v1.CodeManifestInvalid, "systemd 适配器需要非空的应用规格")
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	// 版本探测放最后：它要 exec systemctl，是这里唯一有代价的一步。
	// 探测失败或版本低于 219 时在「还没建用户、没建目录」的阶段就失败，
	// 而不是等到写文件之后留下半套状态。
	if _, err := a.detectVersion(ctx); err != nil {
		return err
	}
	return nil
}

// Prepare 创建运行用户、目录、环境文件、凭据与 unit，并让 systemd 重新加载与启用。
// 幂等：重复调用不会重复 useradd、不会重写内容与权限已经正确的文件，
// 因此重复调用也不会白白触发 daemon-reload。
func (a *Adapter) Prepare(ctx context.Context, spec *domain.ApplicationSpec, slot domain.Slot) error {
	if err := a.Validate(ctx, spec); err != nil {
		return err
	}
	unitName := spec.UnitNameFor(slot)

	// 先解析凭据，再落任何文件：凭据解析失败（SECRET_UNRESOLVED）时不该留下半套产物，
	// 否则下一次 Prepare 面对的是一个「看起来已经准备好」的目录树。
	secrets, err := a.resolveSecrets(ctx, spec)
	if err != nil {
		return err
	}
	if err := a.ensureUser(ctx, spec.Exec.RunUser, spec.Exec.WorkingDirectory); err != nil {
		return err
	}
	owner, err := a.ownerOf(spec.Exec.RunUser)
	if err != nil {
		return err
	}
	if err := a.ensureDirectories(spec, slot, owner); err != nil {
		return err
	}
	if len(spec.SecretFileNames()) > 0 {
		// 只有 kind=file 的凭据需要运行用户自己去读。没有它就不要碰共享目录的权限位。
		if err := a.ensureCredentialReachable(spec, slot, owner); err != nil {
			return err
		}
	}
	if err := a.writeEnvFiles(spec, slot, secrets, owner); err != nil {
		return err
	}

	rendered, unitChanged, err := a.writeUnit(ctx, spec, slot)
	if err != nil {
		return err
	}
	if unitChanged {
		if _, err := a.mustRun(ctx, v1.CodeInternal, systemctlBin, "daemon-reload"); err != nil {
			return err
		}
	}
	// enable 每次都执行，不做「已启用就跳过」的判断：它本身是幂等的，而
	// 「上一次写完 unit 但 enable 失败」这种半成品状态用条件跳过是修不回来的，
	// 重跑 Prepare 必须能把它修好。
	if _, err := a.mustRun(ctx, v1.CodeInternal, systemctlBin, "enable", unitName); err != nil {
		return err
	}

	a.recordDecision(spec, slot, rendered)
	return nil
}

// Start 启动 unit。已在运行时是空操作（幂等）。
func (a *Adapter) Start(ctx context.Context, spec *domain.ApplicationSpec, slot domain.Slot) error {
	if err := a.Validate(ctx, spec); err != nil {
		return err
	}
	unitName := spec.UnitNameFor(slot)
	if err := a.prepared(spec, slot); err != nil {
		return err
	}

	status, err := a.unitStatus(ctx, spec, slot)
	if err != nil {
		return err
	}
	if status == domain.RuntimeActive {
		return nil
	}
	if _, err := a.mustRun(ctx, v1.CodeRuntimeNotReady, systemctlBin, "start", unitName); err != nil {
		return err
	}
	a.markStarted(unitName)
	return nil
}

// Stop 停止 unit。未运行时 systemctl stop 本身成功，所以重复调用是幂等的。
func (a *Adapter) Stop(ctx context.Context, spec *domain.ApplicationSpec, slot domain.Slot) error {
	if err := a.validateSpec(ctx, spec); err != nil {
		return err
	}
	if err := a.prepared(spec, slot); err != nil {
		return err
	}
	unitName := spec.UnitNameFor(slot)
	if _, err := a.mustRun(ctx, v1.CodeInternal, systemctlBin, "stop", unitName); err != nil {
		return err
	}
	a.clearTracking(unitName)
	return nil
}

// Status 把 systemd 的 ActiveState 映射成 domain.RuntimeStatus。
// 认不出来的取值一律落到 unknown，绝不落到 active——把未知状态当成「在运行」会让
// 上层在应用其实没起来的时候继续往下走。
func (a *Adapter) Status(ctx context.Context, spec *domain.ApplicationSpec, slot domain.Slot) (domain.RuntimeStatus, error) {
	if err := a.validateSpec(ctx, spec); err != nil {
		return domain.RuntimeUnknown, err
	}
	return a.unitStatus(ctx, spec, slot)
}

// Health 返回「能否接流量」。它与 Status 刻意不合并：进程活着不等于已就绪。
//
// 返回值分三类，契约测试要求前两类必须能共存：
//   - unit 未运行、或已运行但探活未通过：返回快照（Ready=false，err=nil）——
//     调用方据此轮询，不是错误；
//   - unit 处于 failed、或从本次启动起已超过 startTimeoutSeconds 仍未就绪：
//     返回 RUNTIME_NOT_READY 硬错误，这是确定性失败，再给快照只会让调用方一直等；
//   - 探活连续通过 consecutiveSuccesses 次：Ready=true。
func (a *Adapter) Health(ctx context.Context, spec *domain.ApplicationSpec, slot domain.Slot) (domain.RuntimeHealth, error) {
	if err := a.validateSpec(ctx, spec); err != nil {
		return domain.RuntimeHealth{}, err
	}
	unitName := spec.UnitNameFor(slot)
	now := a.now()

	status, err := a.unitStatus(ctx, spec, slot)
	if err != nil {
		return domain.RuntimeHealth{}, err
	}
	if status == domain.RuntimeFailed {
		return domain.RuntimeHealth{}, domain.NewError(v1.CodeRuntimeNotReady,
			"unit %s 处于 failed 状态，不能接流量；用 journalctl -u %s 定位原因", unitName, unitName)
	}

	if status != domain.RuntimeActive {
		a.successes.Reset(unitName)
		if err := a.startDeadlineExceeded(spec, slot, now, status); err != nil {
			return domain.RuntimeHealth{}, err
		}
		return domain.RuntimeHealth{CheckedAt: now, Detail: "unit 未在运行（" + string(status) + "）"}, nil
	}

	// 就绪目标按槽位取：蓝绿的两个槽位听不同端口，探错目标的后果是**把没起来的那一侧
	// 当成已就绪**——而那正是切流时最不该发生的事。
	ready, detail := readiness.Check(ctx, spec.ReadinessFor(slot), a.probe)
	if !ready {
		a.successes.Reset(unitName)
		if err := a.startDeadlineExceeded(spec, slot, now, status); err != nil {
			return domain.RuntimeHealth{}, err
		}
		return domain.RuntimeHealth{CheckedAt: now, Detail: detail}, nil
	}

	required := spec.ReadinessFor(slot).ConsecutiveSuccesses
	count := a.successes.Record(unitName)
	if count < required {
		return domain.RuntimeHealth{
			CheckedAt: now,
			Detail:    fmt.Sprintf("就绪检查连续通过 %d/%d 次", count, required),
		}, nil
	}
	a.markReady(unitName)
	return domain.RuntimeHealth{Ready: true, CheckedAt: now, Detail: detail}, nil
}

// unitStatus 是 Status 的实现体，供 Start/Health 复用，避免为了拿状态再走一遍 Validate。
//
// 刻意不用 `systemctl show --value`：那个开关是 systemd 230 才加的（已核对 v219/v228/v229
// 的 systemctl.c 里没有它，v230 起才有），而本适配器承诺支持到 219。在 219～229 的主机上
// 带 --value 会直接失败，等于把 legacy 档的 Status/Start/Health 一起废掉。
// 代价是输出多一个 `ActiveState=` 前缀，下面按「有前缀就去掉」解析。
func (a *Adapter) unitStatus(ctx context.Context, spec *domain.ApplicationSpec, slot domain.Slot) (domain.RuntimeStatus, error) {
	unitName := spec.UnitNameFor(slot)
	result, err := a.runner(ctx, []string{systemctlBin, "show", "-p", "ActiveState", unitName})
	if err != nil {
		return domain.RuntimeUnknown, domain.NewError(v1.CodeInternal,
			"执行 systemctl show 失败: %v", err)
	}
	if result.ExitCode != 0 {
		// systemctl 对「没有这个 unit」的报错里带本地化文案，靠 stderr 判断语言相关、
		// 不可靠；这里用我们自己掌握的证据：unit 文件不存在，就没有在运行的东西。
		if exists, statErr := a.unitFileExists(spec, slot); statErr == nil && !exists {
			return domain.RuntimeInactive, nil
		}
		return domain.RuntimeUnknown, domain.NewError(v1.CodeInternal,
			"systemctl show %s 退出码 %d: %s", unitName, result.ExitCode, strings.TrimSpace(result.Stderr))
	}

	state := parseActiveState(result.Stdout)
	if state == "" {
		return domain.RuntimeUnknown, domain.NewError(v1.CodeInternal,
			"systemctl show %s 未返回 ActiveState（输出 %q）", unitName, strings.TrimSpace(result.Stdout))
	}
	switch domain.RuntimeStatus(state) {
	case domain.RuntimeActive, domain.RuntimeInactive, domain.RuntimeFailed,
		domain.RuntimeActivating, domain.RuntimeDeactivating, domain.RuntimeUnknown:
		return domain.RuntimeStatus(state), nil
	}
	// systemd 新增状态（如 maintenance/refreshing）时落到 unknown 而不是报错：
	// 报错会让 opsd 在一台升级过 systemd 的主机上彻底查不了应用状态。
	return domain.RuntimeUnknown, nil
}

// parseActiveState 从 `show -p ActiveState` 的输出里取出状态值，同时容忍只打印值的形态。
// 只取第一行：多出来的内容一定是别的属性或提示，把它们拼进状态里会让映射彻底失控。
func parseActiveState(stdout string) string {
	line, _, _ := strings.Cut(stdout, "\n")
	line = strings.TrimSpace(line)
	if _, value, found := strings.Cut(line, "="); found {
		return strings.TrimSpace(value)
	}
	return line
}

// startDeadlineExceeded 判定「启动超时」。它只对「本次进程发起过 Start、且从未就绪过」
// 的 unit 生效：一个已经就绪过的应用偶发探活失败，不该被误判成启动失败。
// opsd 重启后内存里没有启动时刻，这条判定就不再触发——这是刻意留下的缺口，
// 因为唯一可靠的替代来源（systemd 的 ActiveEnterTimestamp）解析起来不可测且与本地化有关。
func (a *Adapter) startDeadlineExceeded(spec *domain.ApplicationSpec, slot domain.Slot, now time.Time, status domain.RuntimeStatus) error {
	unitName := spec.UnitNameFor(slot)
	a.mu.Lock()
	startedAt, started := a.startedAt[unitName]
	readyOnce := a.becameReady[unitName]
	a.mu.Unlock()

	if !started || readyOnce {
		return nil
	}
	budget := spec.Health.StartTimeout
	if budget <= 0 || now.Sub(startedAt) <= budget {
		return nil
	}
	return domain.NewError(v1.CodeRuntimeNotReady,
		"unit %s 在 startTimeoutSeconds=%s 内未就绪（当前状态 %s）", unitName, budget, status)
}

func (a *Adapter) markStarted(unitName string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.startedAt[unitName] = a.now()
	a.becameReady[unitName] = false
	a.successes.Reset(unitName)
}

func (a *Adapter) markReady(unitName string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.becameReady[unitName] = true
}

// clearTracking 在 Stop 之后清掉进程内的跟踪状态：unit 已经停下，
// 残留的成功计数会让下一次启动少探一次就报告就绪。
func (a *Adapter) clearTracking(unitName string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.startedAt, unitName)
	delete(a.becameReady, unitName)
	a.successes.Forget(unitName)
}

// detectVersion 探测并缓存 systemd 版本。缓存是安全的：systemd 升级需要重启，
// 进程生命周期内版本不会变；而不缓存意味着每次 Status 轮询都要 fork 一次 systemctl。
func (a *Adapter) detectVersion(ctx context.Context) (int, error) {
	a.mu.Lock()
	cached := a.systemdVersion
	a.mu.Unlock()
	if cached != 0 {
		return cached, nil
	}

	// 复用 unitfile 的探测器：它已经处理了 `systemd 255` 的解析、不可解析输出与
	// 低于 219 三种情形，档位下限也只在 TierFor 里定义一次。
	prober := unitfile.NewProber(func(ctx context.Context, argv []string) ([]byte, error) {
		result, err := a.runner(ctx, argv)
		if err != nil {
			return nil, err
		}
		if result.ExitCode != 0 {
			return nil, fmt.Errorf("退出码 %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
		}
		return []byte(result.Stdout), nil
	})
	version, err := prober.Detect(ctx)
	if err != nil {
		return 0, err
	}

	a.mu.Lock()
	a.systemdVersion = version
	a.mu.Unlock()
	return version, nil
}

func (a *Adapter) recordDecision(spec *domain.ApplicationSpec, slot domain.Slot, rendered unitfile.Rendered) {
	unitName := spec.UnitNameFor(slot)
	decision := UnitDecision{
		Application:    spec.Application,
		UnitName:       unitName,
		UnitPath:       domain.UnitPath(unitName),
		Tier:           rendered.Tier,
		SystemdVersion: rendered.SystemdVersion,
		Degradations:   rendered.Degradations,
		DecidedAt:      a.now(),
	}
	a.mu.Lock()
	a.decisions[unitName] = decision
	a.mu.Unlock()

	// 日志里只放档位、版本、unit 路径与降级说明，没有任何凭据值。
	// 这是「同一份 manifest 在不同主机上生成的 unit 为什么不同」的第一手证据。
	a.logger().Info("systemd unit 已渲染",
		"application", decision.Application,
		"unit", decision.UnitName,
		"unitPath", decision.UnitPath,
		"systemdVersion", decision.SystemdVersion,
		"tier", string(decision.Tier),
		"degradations", decision.Degradations,
	)
}

// logger 与 httpapi 适配器保持同一约定：没注入日志时就丢弃，不 panic、不刷屏。
func (a *Adapter) logger() *slog.Logger {
	if a.log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return a.log
}

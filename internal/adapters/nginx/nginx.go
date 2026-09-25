// Package nginx 是 NginxAdapter 的实现：把蓝绿的对外入口落成一份**受管配置**，
// 并用 `nginx -t` 与 `nginx -s reload` 完成校验与切流（迭代 4 规格 D3/D4）。
//
// 两条硬约束：
//
//   - **只写自己的目录**（`<confDir>/frz-managed/<app>.conf`），不改用户的主配置。
//     主配置要不要 include 这个目录由运维决定，而「有没有真的被加载」由 CheckLoaded 检查
//     ——「配置写对了但没生效」是最危险的中间态。
//   - 切换是**原子替换**（写 `.tmp` 再 rename）之后 reload。文件名与内容都不做原地修改：
//     nginx 读配置的瞬间可能正在 reload，半份文件会让它读到一段语法错误的东西。
package nginx

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/execcmd"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// 受管文件与目录的模式：配置里没有任何秘密，0644 足够；目录 0755 让 nginx 的 worker
// （可能以非 root 身份跑）也能读。
const (
	managedFileMode = 0o644
	managedDirMode  = 0o755
)

// Config 是适配器的部署参数（来自 opsd 的配置）。
type Config struct {
	// ConfDir 是 Nginx 的配置目录（生产通常是 /etc/nginx）。受管目录是它的子目录。
	ConfDir string
	// Binary 是 nginx 可执行文件（默认 "nginx"，即走 PATH）。
	Binary string
	// MainConfig 是主配置（默认 <ConfDir>/nginx.conf）。`nginx -t/-T` 都用它。
	MainConfig string
}

// Option 是适配器的可注入项（生产不传；测试用它替换路径前缀、命令执行与平台判定）。
type Option func(*Adapter)

// WithRoot 覆盖路径前缀（测试用；生产是 "/"）。与 systemd 适配器同一个理由：
// 无特权环境也要能断言真实产物的内容与权限。
func WithRoot(root string) Option { return func(a *Adapter) { a.root = root } }

// WithRunner 替换命令执行方式。
func WithRunner(runner execcmd.Runner) Option { return func(a *Adapter) { a.runner = runner } }

// WithGOOS 替换平台判定。Nginx 适配器与 systemd 适配器同为 Linux 专有：
// 非 Linux 上必须明确返回 RUNTIME_UNSUPPORTED，而不是让命令在运行时全失败。
func WithGOOS(goos string) Option { return func(a *Adapter) { a.goos = goos } }

// WithLogger 注入日志出口。
func WithLogger(logger *slog.Logger) Option { return func(a *Adapter) { a.log = logger } }

// Adapter 实现 application.NginxAdapter。
type Adapter struct {
	confDir    string
	binary     string
	mainConfig string

	root   string
	runner execcmd.Runner
	goos   string
	log    *slog.Logger
}

func New(cfg Config, opts ...Option) *Adapter {
	confDir := strings.TrimSpace(cfg.ConfDir)
	if confDir == "" {
		confDir = "/etc/nginx"
	}
	binary := strings.TrimSpace(cfg.Binary)
	if binary == "" {
		binary = "nginx"
	}
	mainConfig := strings.TrimSpace(cfg.MainConfig)
	if mainConfig == "" {
		mainConfig = filepath.Join(confDir, "nginx.conf")
	}
	adapter := &Adapter{
		confDir:    confDir,
		binary:     binary,
		mainConfig: mainConfig,
		root:       string(filepath.Separator),
		runner:     execcmd.Command,
		goos:       runtime.GOOS,
		log:        slog.New(slog.DiscardHandler),
	}
	for _, opt := range opts {
		opt(adapter)
	}
	if adapter.root == "" {
		adapter.root = string(filepath.Separator)
	}
	return adapter
}

// ManagedDir 是受管目录（`<confDir>/frz-managed`）。导出它是因为运维要在主配置里
// include 它——这条路径是运维与工具之间的接口，不能藏在实现里。
func (a *Adapter) ManagedDir() string { return domain.ManagedNginxDir(a.confDir) }

// ManagedFile 是某个应用的受管文件。它同时是「现在指向哪个槽位」的**线上事实**
// （迭代 4 规格 D2）：对账以它为准。
func (a *Adapter) ManagedFile(application string) string {
	return domain.ManagedNginxFile(a.confDir, application)
}

// RootPath 返回绝对路径在本适配器下的实际位置（测试用前缀重定向）。
func (a *Adapter) RootPath(absolute string) string {
	return filepath.Join(a.root, absolute)
}

// Validate 检查这台主机能不能承担蓝绿的入口：平台、规格形态、nginx 可执行、配置目录存在。
//
// 它必须能在 **Prepare 阶段**就跑：蓝绿的新槽位起来之后才发现 nginx 不可用，等于白折腾
// 一趟（而且那时流量还在旧槽位，用户看到的是「部署失败但不知道为什么」）。
func (a *Adapter) Validate(ctx context.Context, spec *domain.ApplicationSpec) error {
	if a.goos != "linux" {
		return domain.NewError(v1.CodeRuntimeUnsupport,
			"Nginx 适配器只支持 Linux（当前平台 %s）", a.goos)
	}
	if spec == nil {
		return domain.NewError(v1.CodeManifestInvalid, "Nginx 适配器需要非空的应用规格")
	}
	if !spec.BlueGreen() {
		return domain.NewError(v1.CodeManifestInvalid,
			"应用 %s 不是蓝绿形态（没有 exec.slots）：它不需要对外入口这一层", spec.Application)
	}
	if !spec.Nginx.Configured() {
		return domain.NewError(v1.CodeManifestInvalid, "蓝绿应用 %s 必须声明 nginx 段", spec.Application)
	}
	// 探测 nginx 本身：`-v` 是只读的，且是唯一能证明「这个二进制真的能跑」的方式。
	if _, err := a.runner(ctx, []string{a.binary, "-v"}); err != nil {
		return domain.NewError(v1.CodeRuntimeUnsupport,
			"执行 %s -v 失败（这台主机上没有可用的 Nginx）: %v", a.binary, err)
	}
	if _, err := os.Stat(a.RootPath(a.confDir)); err != nil {
		return domain.NewError(v1.CodeRuntimeUnsupport,
			"Nginx 配置目录 %s 不可用: %v", a.confDir, err)
	}
	return nil
}

// CheckLoaded 断言受管文件**真的被主配置加载**。
//
// 判据是 `nginx -T` 的输出：它把生效的配置连同**每个文件的名字**一起 dump 出来
// （`# configuration file <path>:` 这一行）。找不到就说明主配置没有 include 我们的目录
// ——此时切流看起来会成功，而流量根本不经过我们改的 upstream。宁可在这里失败。
func (a *Adapter) CheckLoaded(ctx context.Context, spec *domain.ApplicationSpec) error {
	if err := a.Validate(ctx, spec); err != nil {
		return err
	}
	return a.checkLoaded(ctx, spec)
}

// checkLoaded 是不带前置校验的那一半：Apply 已经校验过，再探一次 nginx 只是白起一个进程。
func (a *Adapter) checkLoaded(ctx context.Context, spec *domain.ApplicationSpec) error {
	result, err := a.runner(ctx, []string{a.binary, "-T", "-c", a.mainConfig})
	if err != nil {
		return domain.NewError(v1.CodeNginxConfigInvalid, "执行 %s -T 失败: %v", a.binary, err)
	}
	// nginx 把 dump 写到 stderr（老版本）或 stdout（新版本），两个都找一遍：
	// 不为了「哪个流」去猜版本，反正内容是一样的。
	dump := result.Stdout + result.Stderr
	if result.ExitCode != 0 {
		return domain.NewError(v1.CodeNginxConfigInvalid,
			"%s -T 退出码 %d（主配置本身就有问题）: %s",
			a.binary, result.ExitCode, strings.TrimSpace(result.Stderr))
	}

	file := a.ManagedFile(spec.Application)
	marker := "# configuration file " + file + ":"
	if !strings.Contains(dump, marker) {
		return domain.NewError(v1.CodeNginxConfigInvalid,
			"受管文件 %s 没有被主配置 %s 加载（nginx -T 的输出里没有这一行）。"+
				"请在主配置里加一行 include %s/*.conf; 之后重试——在那之前切流不会有任何效果",
			file, a.mainConfig, a.ManagedDir())
	}
	return nil
}

// Apply 把 upstream 指向给定槽位：写候选 → 放到位 → 校验 → reload。
//
// 顺序上有一处与规格初稿不同、且是刻意的：**先放文件、再校验**。原因是 nginx 的 `-t`
// 只能校验**已安装**的配置树，没有「校验一个候选文件」的写法；而 nginx 只在 reload 时读
// 配置，因此放文件的瞬间对流量没有任何影响——真正决定流量的是最后那一次 reload。
// 校验失败时把原内容换回（也不 reload），流量同样一点没动。
func (a *Adapter) Apply(ctx context.Context, spec *domain.ApplicationSpec, target domain.Slot) error {
	if err := a.Validate(ctx, spec); err != nil {
		return err
	}
	if err := a.checkLoaded(ctx, spec); err != nil {
		return err
	}

	// 头注释只写槽位：这才是这份文件承载的那个事实（「现在指向谁」）。具体的 release 与
	// 版本由 `app slot list` 回答——那份数据在库里，这里多写一遍只会多一处会过期的副本。
	candidate, err := RenderManaged(spec, target, "", "")
	if err != nil {
		return err
	}

	file := a.ManagedFile(spec.Application)
	previous, hadPrevious, err := a.readManaged(spec.Application)
	if err != nil {
		return err
	}
	if err := a.writeManaged(spec.Application, candidate); err != nil {
		return err
	}

	// 校验**已安装**的配置树。失败：换回原来那份（没有 reload，因此流量未动）。
	if err := a.testConfig(ctx); err != nil {
		if restoreErr := a.restore(spec.Application, previous, hadPrevious); restoreErr != nil {
			// 换回也失败：磁盘上的配置现在是一份没通过校验的东西，而下一次有人 reload
			// nginx（手工的、或别的应用的流程）就会把坏配置带上线。
			return domain.NewError(v1.CodeNginxConfigInvalid,
				"%s；**且换回原配置也失败了**（%s）：%s 现在是一份没通过校验的配置，需要人工介入",
				domain.MessageOf(err), domain.MessageOf(restoreErr), file)
		}
		return err
	}

	// 校验通过 → reload。失败：换回原内容并再 reload 一次。
	if err := a.reload(ctx); err != nil {
		restoreErr := a.restore(spec.Application, previous, hadPrevious)
		var second error
		if restoreErr == nil {
			second = a.reload(ctx)
		}
		if restoreErr != nil || second != nil {
			detail := domain.MessageOf(restoreErr)
			if restoreErr == nil {
				detail = domain.MessageOf(second)
			}
			// **不报「已回滚」**：现在没有任何东西能保证流量在哪一侧。
			return domain.NewError(v1.CodeNginxReloadFailed,
				"reload 失败（%s），换回原配置后再次 reload 也失败（%s）：流量现在处于不确定状态，需要人工介入",
				domain.MessageOf(err), detail)
		}
		return domain.NewError(v1.CodeNginxReloadFailed,
			"reload 失败（%s）：已换回原配置并重新 reload，流量仍在原来的槽位", domain.MessageOf(err))
	}

	a.log.Info("Nginx 已切流", "application", spec.Application, "slot", string(target), "file", file)
	return nil
}

// Current 读回受管文件现在指向哪个槽位。文件不存在时返回空槽位（「还没部署过」不是错误）。
func (a *Adapter) Current(_ context.Context, spec *domain.ApplicationSpec) (domain.Slot, error) {
	if spec == nil {
		return "", domain.NewError(v1.CodeManifestInvalid, "Nginx 适配器需要非空的应用规格")
	}
	content, present, err := a.readManaged(spec.Application)
	if err != nil {
		return "", err
	}
	if !present {
		return "", nil
	}
	slot, ok := ParseManaged(content, spec)
	if !ok {
		return "", domain.NewError(v1.CodeNginxConfigInvalid,
			"受管文件 %s 读不出它指向哪个槽位（可能被手工改过）", a.ManagedFile(spec.Application))
	}
	return slot, nil
}

// readManaged 读受管文件的当前内容。第二个返回值表示文件是否存在。
func (a *Adapter) readManaged(application string) (string, bool, error) {
	raw, err := os.ReadFile(a.RootPath(a.ManagedFile(application)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, domain.NewError(v1.CodeInternal,
			"读取受管配置 %s 失败: %v", a.ManagedFile(application), err)
	}
	return string(raw), true, nil
}

// writeManaged 原子地写入受管文件：先写 `.tmp` 再 rename。
func (a *Adapter) writeManaged(application, content string) error {
	file := a.RootPath(a.ManagedFile(application))
	if err := os.MkdirAll(filepath.Dir(file), managedDirMode); err != nil {
		return domain.NewError(v1.CodeInternal, "创建受管目录 %s 失败: %v", filepath.Dir(file), err)
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), managedFileMode); err != nil {
		return domain.NewError(v1.CodeInternal, "写入候选配置 %s 失败: %v", tmp, err)
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return domain.NewError(v1.CodeInternal, "把候选配置改名为 %s 失败: %v", file, err)
	}
	return nil
}

// restore 把受管文件恢复成之前那份；之前没有文件就把它删掉。
func (a *Adapter) restore(application, previous string, hadPrevious bool) error {
	file := a.RootPath(a.ManagedFile(application))
	if !hadPrevious {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return domain.NewError(v1.CodeInternal, "移除 %s 失败: %v", file, err)
		}
		return nil
	}
	return a.writeManaged(application, previous)
}

// testConfig 校验当前已安装的配置树。
func (a *Adapter) testConfig(ctx context.Context) error {
	result, err := a.runner(ctx, []string{a.binary, "-t", "-c", a.mainConfig})
	if err != nil {
		return domain.NewError(v1.CodeNginxConfigInvalid, "执行 %s -t 失败: %v", a.binary, err)
	}
	if result.ExitCode != 0 {
		return domain.NewError(v1.CodeNginxConfigInvalid,
			"配置没通过 %s -t（退出码 %d）: %s",
			a.binary, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// reload 让 nginx 重新加载配置。它不给 nginx 发信号之外的东西：`-s reload` 是 graceful 的，
// 旧 worker 会处理完在途请求再退出。
func (a *Adapter) reload(ctx context.Context) error {
	result, err := a.runner(ctx, []string{a.binary, "-s", "reload"})
	if err != nil {
		return domain.NewError(v1.CodeNginxReloadFailed, "执行 %s -s reload 失败: %v", a.binary, err)
	}
	if result.ExitCode != 0 {
		return domain.NewError(v1.CodeNginxReloadFailed,
			"%s -s reload 退出码 %d: %s", a.binary, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// Package proc 是 RuntimeAdapter 在本机进程上的实现，同时充当 macOS 与 CI 上的
// 假适配器：它不做任何需要 root 的事，却能跑通同一套合约测试。
//
// 它把所有绝对路径映射到自己的沙箱根之下（`/var/lib/app` 变成
// `<root>/<application>/fs/var/lib/app`）。这不是取巧，而是必需的：合约测试要能在
// 无特权环境里断言目录、环境文件与属主语义，否则这些断言只能在 Linux 上跑，
// 本地就失去了反馈速度。
package proc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/runtime/readiness"
	"github.com/freezeChen/frz-tools/internal/adapters/runtime/unitfile"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

const (
	stateFileName    = "state.json"
	nonSecretEnvFile = "app.env"
	secretEnvFile    = "app.secrets.env"
	pidFileName      = "app.pid"
)

// state 是 Prepare 的产物，Start 只读它。把它落盘而不是留在内存里，
// 是为了让合约测试能够断言「Prepare 的结果」本身。
type state struct {
	Application      string   `json:"application"`
	UnitName         string   `json:"unitName"`
	Argv             []string `json:"argv"`
	WorkingDirectory string   `json:"workingDirectory"`
	LogFile          string   `json:"logFile"`
	UnpackDir        string   `json:"unpackDir"`
}

type Adapter struct {
	root     string
	resolver application.SecretResolver
	now      func() time.Time
	probe    time.Duration

	mu      sync.Mutex
	running map[string]*exec.Cmd

	// successes 与 systemd 适配器共用同一份「连续成功」语义，见 readiness 包。
	successes *readiness.Tracker
}

func New(root string, resolver application.SecretResolver) *Adapter {
	return &Adapter{
		root:      root,
		resolver:  resolver,
		now:       func() time.Time { return time.Now().UTC() },
		probe:     readiness.DefaultTimeout,
		running:   map[string]*exec.Cmd{},
		successes: readiness.NewTracker(),
	}
}

// Validate 做与 systemd 适配器同一条规则的检查：规格本身，加上 java 解释器在不在
// （迭代 3c，见 unitfile.PreflightInterpreter）。
//
// 它**曾经**在这里 stat 任意 argv[0]（含 go），而 systemd 那边没有——同一个端口上两个
// 实现对「什么算合法规格」的判断不同，正是合约包要防的那种漂移。现在两边共用同一个
// 判据、同一个函数。不再检查 go 的绝对 argv[0] 是有意的：那属于**收紧** 1c 起就存在的
// 形态（绝对路径在夹具里被广泛使用），需要单独判断兼容性影响，已记入停放区。
func (a *Adapter) Validate(_ context.Context, spec *domain.ApplicationSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	return unitfile.PreflightInterpreter(spec)
}

func (a *Adapter) Prepare(ctx context.Context, spec *domain.ApplicationSpec) error {
	if err := a.Validate(ctx, spec); err != nil {
		return err
	}

	// 判据用**规格里的原始路径**（而不是 mapPath 之后的沙箱路径）：releases 子树是
	// 「我们管理的生产路径」，沙箱前缀只是测试手段，拿沙箱路径去比对必然匹配不上。
	for _, absolute := range []string{spec.Exec.WorkingDirectory, spec.Logs.Directory} {
		// releases 子树的内部由 ReleaseAdapter 建，与 systemd 适配器同一条分工：
		// Prepare 若把 `current` 建成实体目录，之后的符号链接切换就会失败
		// （见 domain.InReleaseTree）。
		if domain.InReleaseTree(spec.Application, absolute) {
			continue
		}
		if err := os.MkdirAll(a.mapPath(spec, absolute), 0o750); err != nil {
			return domain.NewError(v1.CodeInternal, "创建目录 %q 失败: %v", absolute, err)
		}
	}
	if err := os.MkdirAll(a.appRoot(spec), 0o750); err != nil {
		return domain.NewError(v1.CodeInternal, "创建应用目录 %q 失败: %v", a.appRoot(spec), err)
	}

	// 凭据目录单独处理并要求 0700：MkdirAll 对已存在的目录不改权限，
	// 因此必须显式 Chmod，否则目录一旦先被上面的 0750 循环建出来，
	// 后面再按 0700 创建就是无效的——同组用户将能穿过凭据目录。
	secretsDir := a.mapPath(spec, domain.SecretsDir(spec.Application))
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		return domain.NewError(v1.CodeInternal, "创建凭据目录 %q 失败: %v", secretsDir, err)
	}
	if err := os.Chmod(secretsDir, 0o700); err != nil {
		return domain.NewError(v1.CodeInternal, "收敛凭据目录权限失败: %v", err)
	}

	if err := a.writeEnvFiles(ctx, spec); err != nil {
		return err
	}
	return a.writeState(spec)
}

func (a *Adapter) Start(ctx context.Context, spec *domain.ApplicationSpec) error {
	if err := a.Validate(ctx, spec); err != nil {
		return err
	}
	current, err := a.Status(ctx, spec)
	if err != nil {
		return err
	}
	if current == domain.RuntimeActive {
		return nil
	}

	stored, err := a.loadState(spec)
	if err != nil {
		return err
	}
	env, err := a.environment(ctx, spec)
	if err != nil {
		return err
	}

	logPath := a.mapPath(spec, spec.Logs.Directory)
	if err := os.MkdirAll(logPath, 0o750); err != nil {
		return domain.NewError(v1.CodeInternal, "创建日志目录失败: %v", err)
	}
	logFile, err := os.OpenFile(filepath.Join(logPath, "current.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return domain.NewError(v1.CodeInternal, "打开日志文件失败: %v", err)
	}
	defer logFile.Close()

	cmd := exec.Command(stored.Argv[0], stored.Argv[1:]...)
	cmd.Dir = stored.WorkingDirectory
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return domain.NewError(v1.CodeInternal, "启动 %q 失败: %v", stored.Argv[0], err)
	}

	a.mu.Lock()
	a.running[spec.Systemd.UnitName] = cmd
	a.successes.Reset(spec.Systemd.UnitName)
	a.mu.Unlock()

	if err := os.WriteFile(filepath.Join(a.appRoot(spec), pidFileName),
		[]byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o640); err != nil {
		return domain.NewError(v1.CodeInternal, "写入 pid 文件失败: %v", err)
	}

	// 必须回收子进程，否则它会变成僵尸进程，而僵尸在 kill(pid,0) 下仍算「存在」。
	go func() {
		_ = cmd.Wait()
		a.mu.Lock()
		delete(a.running, spec.Systemd.UnitName)
		a.mu.Unlock()
	}()
	return nil
}

func (a *Adapter) Stop(ctx context.Context, spec *domain.ApplicationSpec) error {
	// 与 systemd 适配器同一条：Stop 只校验规格本身，**不校验 argv[0] 还在不在**
	// （见 systemd.Adapter.validateSpec 的注释）。解释器被卸载、制品被人删掉时，
	// 服务仍然必须能停下来——那正是最需要这个工具的时刻。
	if err := spec.Validate(); err != nil {
		return err
	}

	a.mu.Lock()
	cmd, ok := a.running[spec.Systemd.UnitName]
	a.mu.Unlock()
	if !ok {
		_ = os.Remove(filepath.Join(a.appRoot(spec), pidFileName))
		return nil
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return domain.NewError(v1.CodeInternal, "发送 SIGTERM 失败: %v", err)
	}

	deadline := a.now().Add(5 * time.Second)
	for a.now().Before(deadline) {
		if status, err := a.Status(ctx, spec); err == nil && status != domain.RuntimeActive {
			_ = os.Remove(filepath.Join(a.appRoot(spec), pidFileName))
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return domain.NewError(v1.CodeInternal, "强制结束失败: %v", err)
	}
	_ = os.Remove(filepath.Join(a.appRoot(spec), pidFileName))
	return nil
}

func (a *Adapter) Status(ctx context.Context, spec *domain.ApplicationSpec) (domain.RuntimeStatus, error) {
	if err := spec.Validate(); err != nil {
		return domain.RuntimeUnknown, err
	}

	a.mu.Lock()
	_, tracked := a.running[spec.Systemd.UnitName]
	a.mu.Unlock()
	if tracked {
		return domain.RuntimeActive, nil
	}

	raw, err := os.ReadFile(filepath.Join(a.appRoot(spec), pidFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.RuntimeInactive, nil
		}
		return domain.RuntimeUnknown, domain.NewError(v1.CodeInternal, "读取 pid 文件失败: %v", err)
	}

	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err != nil || pid <= 0 {
		return domain.RuntimeUnknown, domain.NewError(v1.CodeInternal, "pid 文件内容非法")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return domain.RuntimeInactive, nil
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		return domain.RuntimeInactive, nil
	}
	return domain.RuntimeActive, nil
}

func (a *Adapter) Health(ctx context.Context, spec *domain.ApplicationSpec) (domain.RuntimeHealth, error) {
	now := a.now()
	status, err := a.Status(ctx, spec)
	if err != nil {
		return domain.RuntimeHealth{}, err
	}
	if status != domain.RuntimeActive {
		a.successes.Reset(spec.Systemd.UnitName)
		return domain.RuntimeHealth{CheckedAt: now, Detail: "进程未在运行"}, nil
	}

	ready, detail := readiness.Check(ctx, spec.Health.Readiness, a.probe)
	if !ready {
		a.successes.Reset(spec.Systemd.UnitName)
		return domain.RuntimeHealth{CheckedAt: now, Detail: detail}, nil
	}

	count := a.successes.Record(spec.Systemd.UnitName)
	required := spec.Health.Readiness.ConsecutiveSuccesses
	if count < required {
		return domain.RuntimeHealth{
			CheckedAt: now,
			Detail:    fmt.Sprintf("就绪检查连续通过 %d/%d 次", count, required),
		}, nil
	}
	return domain.RuntimeHealth{Ready: true, CheckedAt: now, Detail: detail}, nil
}

func (a *Adapter) appRoot(spec *domain.ApplicationSpec) string {
	return filepath.Join(a.root, spec.Application)
}

// mapPath 把规格里的绝对路径搬到沙箱内，这样无特权也能跑通目录与环境文件断言。
func (a *Adapter) mapPath(spec *domain.ApplicationSpec, absolute string) string {
	return a.SandboxPath(spec.Application, absolute)
}

// SandboxPath 返回规格里的绝对路径在本适配器沙箱内的实际位置。
// 导出它是为了让测试与 harness 能断言真实产物，而不必复制一遍沙箱规则——
// 复制规则意味着沙箱一旦调整，测试会继续「通过」却指向错误的文件。
func (a *Adapter) SandboxPath(application, absolute string) string {
	return filepath.Join(a.root, application, "fs", strings.TrimPrefix(absolute, "/"))
}

func (a *Adapter) writeEnvFiles(ctx context.Context, spec *domain.ApplicationSpec) error {
	nonSecret := map[string]string{}
	for name, value := range spec.Exec.Environment {
		nonSecret[name] = value
	}
	if err := writeFile(a.mapPath(spec, domain.EnvFilePath(spec.Application)),
		unitfile.RenderEnvFile(nonSecret), 0o600); err != nil {
		return err
	}

	secrets := map[string]string{}
	secretsDir := a.mapPath(spec, domain.SecretsDir(spec.Application))
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		return domain.NewError(v1.CodeInternal, "创建凭据目录失败: %v", err)
	}
	for name, ref := range spec.Exec.SecretEnvironment {
		value, err := a.resolver.Resolve(ctx, ref)
		if err != nil {
			return err
		}
		if ref.Kind == domain.SecretKindEnv {
			// environment 文件承载不了含真实换行的值，这里必须显式拒绝，
			// 而不是悄悄写进去让进程收到被吃掉换行的凭据。
			if unitfile.HasNewline(value) {
				return domain.NewError(v1.CodeSecretUnresolved,
					"secretEnvironment[%q] 的值含换行，必须以 kind: file 交付", name)
			}
			secrets[name] = value
			continue
		}
		path := filepath.Join(secretsDir, name)
		if err := writeFile(path, []byte(value), 0o600); err != nil {
			return err
		}
		secrets[name] = path
	}
	return writeFile(a.mapPath(spec, domain.SecretsEnvFilePath(spec.Application)),
		unitfile.RenderEnvFile(secrets), 0o600)
}

// environment 组装传给子进程的环境变量。非敏感值与环境型凭据直接注入，
// 文件型凭据注入的是文件路径——与 1c 规格第 6 节的约定一致。
func (a *Adapter) environment(ctx context.Context, spec *domain.ApplicationSpec) ([]string, error) {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + a.mapPath(spec, spec.Exec.WorkingDirectory),
	}
	for name, value := range spec.Exec.Environment {
		env = append(env, name+"="+value)
	}

	secretsDir := a.mapPath(spec, domain.SecretsDir(spec.Application))
	for name, ref := range spec.Exec.SecretEnvironment {
		value, err := a.resolver.Resolve(ctx, ref)
		if err != nil {
			return nil, err
		}
		if ref.Kind == domain.SecretKindFile {
			value = filepath.Join(secretsDir, name)
		} else if unitfile.HasNewline(value) {
			return nil, domain.NewError(v1.CodeSecretUnresolved,
				"secretEnvironment[%q] 的值含换行，必须以 kind: file 交付", name)
		}
		env = append(env, name+"="+value)
	}
	return env, nil
}

func (a *Adapter) writeState(spec *domain.ApplicationSpec) error {
	stored := state{
		Application:      spec.Application,
		UnitName:         spec.Systemd.UnitName,
		Argv:             spec.Exec.Argv,
		WorkingDirectory: a.mapPath(spec, spec.Exec.WorkingDirectory),
		LogFile:          filepath.Join(a.mapPath(spec, spec.Logs.Directory), "current.log"),
		UnpackDir:        a.mapPath(spec, "/opt/opsd/apps/"+spec.Application+"/releases/current"),
	}
	encoded, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return domain.NewError(v1.CodeInternal, "序列化运行时状态失败: %v", err)
	}
	return writeFile(filepath.Join(a.appRoot(spec), stateFileName), append(encoded, '\n'), 0o640)
}

func (a *Adapter) loadState(spec *domain.ApplicationSpec) (*state, error) {
	raw, err := os.ReadFile(filepath.Join(a.appRoot(spec), stateFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, domain.NewError(v1.CodeRuntimeNotReady,
				"应用 %s 尚未 Prepare，不能直接启动", spec.Application)
		}
		return nil, domain.NewError(v1.CodeInternal, "读取运行时状态失败: %v", err)
	}
	var stored state
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, domain.NewError(v1.CodeInternal, "解析运行时状态失败: %v", err)
	}
	return &stored, nil
}

func writeFile(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return domain.NewError(v1.CodeInternal, "创建目录 %q 失败: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		return domain.NewError(v1.CodeInternal, "写入 %q 失败: %v", path, err)
	}
	// os.WriteFile 对已存在的文件不会改权限，显式 Chmod 才能保证幂等 Prepare 后
	// 权限仍是我们要求的那个值。
	return os.Chmod(path, mode)
}

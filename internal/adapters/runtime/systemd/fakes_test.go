package systemd_test

import (
	"context"
	"errors"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/runtime/systemd"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// stubResolver 按 SecretRef 的字符串形式返回值。
type stubResolver struct {
	values map[string]string
}

func (s *stubResolver) Resolve(_ context.Context, ref domain.SecretRef) (string, error) {
	value, ok := s.values[ref.String()]
	if !ok {
		return "", domain.NewError(v1.CodeSecretUnresolved, "no secret %s", ref)
	}
	return value, nil
}

// secretValues 是测试用的凭据：一个含反斜杠与双引号的单行值（压环境文件转义），
// 一个多行值（只能走 kind=file）。
func secretValues() map[string]string {
	return map[string]string{
		"env:APP_TOKEN": `tok\en"v"`,
		// SecretRef.String() 是 "kind:name"，kind=file 的 name 是凭据来源的路径。
		"file:/etc/opsd/secrets/app.key": "line1\nline2\n",
	}
}

// fakeHost 模拟一台装了 systemd 的 Linux 主机：命令、unit 状态与用户库都在内存里。
// 适配器只能通过注入的 Runner 触达它，因此测试断言的是「命令序列」，不是副作用。
type fakeHost struct {
	mu            sync.Mutex
	root          string
	versionOutput string
	runnerErr     error
	showOverride  *systemd.Result
	users         map[string]bool
	states        map[string]string
	commands      [][]string
}

func newFakeHost(root string) *fakeHost {
	return &fakeHost{
		root:          root,
		versionOutput: "systemd 255\n+PAM +AUDIT +SELINUX +APPARMOR +IMA\n",
		users:         map[string]bool{},
		states:        map[string]string{},
	}
}

// run 实现 systemd.Runner。
func (f *fakeHost) run(_ context.Context, argv []string) (systemd.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, append([]string(nil), argv...))

	if f.runnerErr != nil {
		return systemd.Result{}, f.runnerErr
	}

	switch argv[0] {
	case "systemctl":
		return f.systemctl(argv)
	case "id":
		if f.users[argv[2]] {
			return systemd.Result{Stdout: "1000\n"}, nil
		}
		return systemd.Result{ExitCode: 1, Stderr: "no such user"}, nil
	case "useradd":
		f.users[argv[len(argv)-1]] = true
		return systemd.Result{}, nil
	default:
		return systemd.Result{ExitCode: 127, Stderr: "command not found"}, nil
	}
}

func (f *fakeHost) systemctl(argv []string) (systemd.Result, error) {
	if len(argv) < 2 {
		return systemd.Result{ExitCode: 1, Stderr: "missing verb"}, nil
	}
	verb := argv[1]
	if verb == "--version" {
		if f.versionOutput == "" {
			return systemd.Result{ExitCode: 1, Stderr: "systemctl: command not found"}, nil
		}
		return systemd.Result{Stdout: f.versionOutput}, nil
	}
	unit := ""
	if len(argv) > 2 {
		unit = argv[len(argv)-1]
	}

	switch verb {
	case "daemon-reload", "enable":
		return systemd.Result{}, nil
	case "start":
		if !f.unitFileExists(unit) {
			return systemd.Result{ExitCode: 1, Stderr: "Failed to start " + unit + ": Unit not found."}, nil
		}
		f.states[unit] = string(domain.RuntimeActive)
		return systemd.Result{}, nil
	case "stop":
		f.states[unit] = string(domain.RuntimeInactive)
		return systemd.Result{}, nil
	case "show":
		if f.showOverride != nil {
			return *f.showOverride, nil
		}
		if !f.unitFileExists(unit) {
			return systemd.Result{ExitCode: 4, Stderr: "Unit " + unit + " could not be found."}, nil
		}
		// 真机命令是 `show -p ActiveState <unit>`（不用 --value，见 unitStatus 的说明），
		// 输出因此带属性名前缀。
		return systemd.Result{Stdout: "ActiveState=" + f.state(unit) + "\n"}, nil
	}
	return systemd.Result{ExitCode: 1, Stderr: "unknown verb " + verb}, nil
}

// unitFileExists 用真实文件系统回答「unit 装没装」——真机上的 systemd 也会去看 unit 路径，
// 因此这里不是取巧，而是让假主机与真主机在同一处证据上保持一致。
func (f *fakeHost) unitFileExists(unitName string) bool {
	if unitName == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(f.root, "etc/systemd/system", unitName))
	return err == nil
}

func (f *fakeHost) state(unitName string) string {
	if state, ok := f.states[unitName]; ok {
		return state
	}
	return string(domain.RuntimeInactive)
}

func (f *fakeHost) setState(unitName string, state domain.RuntimeStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[unitName] = string(state)
}

func (f *fakeHost) takeCommands() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	taken := f.commands
	f.commands = nil
	return taken
}

// currentOwner 是可注入的 OwnerResolver：忽略用户名，返回当前进程的 uid/gid。
// 测试进程通常不是 root，无法真的 chown 给别的用户——注入自己才能让 chown 成功，
// 从而断言「属主确实按解析结果被设置过」而不是只有模式对了。
func currentOwner(string) (int, int, error) { return currentIDs() }

// currentIDs 返回当前进程的 uid/gid，供断言使用。
func currentIDs() (int, int, error) {
	account, err := user.Current()
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// specFixture 返回一份合法规格；name 用作应用名，保证子测试之间不共享任何路径。
func specFixture(t *testing.T, name string) *domain.ApplicationSpec {
	t.Helper()
	spec := &domain.ApplicationSpec{
		APIVersion:  domain.ManifestAPIVersion,
		Kind:        domain.ManifestKind,
		Application: name,
		Runtime:     domain.RuntimeKindGo,
		Artifact:    domain.SpecArtifact{Digest: "sha256:" + name},
		Exec: domain.SpecExec{
			Argv:             []string{"/opt/opsd/apps/" + name + "/bin/run"},
			WorkingDirectory: "/var/lib/" + name,
			RunUser:          "appuser",
			Environment:      map[string]string{"GOMEMLIMIT": "40MiB"},
			SecretEnvironment: map[string]domain.SecretRef{
				"APP_TOKEN": {Kind: domain.SecretKindEnv, Name: "APP_TOKEN"},
				"APP_KEY":   {Kind: domain.SecretKindFile, Name: "/etc/opsd/secrets/app.key"},
			},
			Ports: []int{8080},
		},
		Health: domain.SpecHealth{
			Readiness: domain.SpecReadiness{
				Type:                 domain.ReadinessTCP,
				Target:               "127.0.0.1:1", // 由 Serve/测试覆盖
				ConsecutiveSuccesses: 1,
			},
			StartTimeout: 30 * time.Second,
			StopTimeout:  30 * time.Second,
		},
		Logs: domain.SpecLogs{Directory: "/var/log/" + name},
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("夹具规格本身必须合法: %v", err)
	}
	return spec
}

// serveTCP 在随机端口上监听，并把规格的就绪目标指向它——返回销毁函数。
func serveTCP(t *testing.T, spec *domain.ApplicationSpec) func() {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	spec.Health.Readiness.Target = listener.Addr().String()
	return func() { _ = listener.Close() }
}

func mustRun(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("非预期错误: %v", err)
	}
}

func formatCommands(commands [][]string) string {
	lines := make([]string, 0, len(commands))
	for _, argv := range commands {
		lines = append(lines, strings.Join(argv, " "))
	}
	return strings.Join(lines, "\n")
}

var errBoom = errors.New("boom")

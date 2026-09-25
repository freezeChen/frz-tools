package nginx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/execcmd"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// 这一组用例护的是**切流那条路径上的每一个岔口**：校验失败不能动流量、reload 失败要换回去、
// 换不回去要说清「现在不确定」。命令执行被整段替换成可编程的假实现，因此这些岔口都能在
// 没有 nginx 的机器上逐条走一遍——而真实 nginx 上的行为由容器 harness 覆盖（真 `nginx -t`、
// 真 reload、真 curl）。

// fakeNginx 是可编程的 nginx 替身：它记录命令序列，并按脚本决定每条命令的结果。
type fakeNginx struct {
	commands [][]string
	// failTest 让 -t 失败；failReload 让**每一次** reload 都失败；failReloadOnce 让
	// **下一次** reload 失败一次（用于「换回原配置后再次 reload 成功」那条岔口）。
	failTest       bool
	failReload     bool
	failReloadOnce bool
	reloadCount    int
	// dump 是 -T 的输出（CheckLoaded 找的是里面的「# configuration file …」那一行）。
	dump string
	// binaries 里没有的二进制算「命令跑不起来」。
	missingBinary bool
}

func (f *fakeNginx) run(_ context.Context, argv []string) (execcmd.Result, error) {
	f.commands = append(f.commands, argv)
	if f.missingBinary {
		return execcmd.Result{ExitCode: -1}, os.ErrNotExist
	}
	switch {
	case len(argv) > 1 && argv[1] == "-v":
		return execcmd.Result{Stderr: "nginx version: nginx/1.18.0"}, nil
	case len(argv) > 1 && argv[1] == "-T":
		return execcmd.Result{Stdout: f.dump}, nil
	case len(argv) > 1 && argv[1] == "-t":
		if f.failTest {
			return execcmd.Result{ExitCode: 1, Stderr: "nginx: configuration file test failed"}, nil
		}
		return execcmd.Result{Stderr: "syntax is ok"}, nil
	case len(argv) > 2 && argv[1] == "-s" && argv[2] == "reload":
		f.reloadCount++
		if f.failReload {
			return execcmd.Result{ExitCode: 1, Stderr: "reload failed"}, nil
		}
		if f.failReloadOnce {
			// 只失败这一次：下一次（换回原配置之后的那个）必须成功，否则测的就是
			// 「两次都失败」那条岔口了。
			f.failReloadOnce = false
			return execcmd.Result{ExitCode: 1, Stderr: "reload failed"}, nil
		}
		return execcmd.Result{}, nil
	}
	return execcmd.Result{}, nil
}

type fixture struct {
	adapter *Adapter
	fake    *fakeNginx
	root    string
	spec    *domain.ApplicationSpec
	ctx     context.Context
}

// newFixture 造一个蓝绿应用的规格与一个受管目录已被加载的假 nginx。
func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	spec := &domain.ApplicationSpec{
		APIVersion:  domain.ManifestAPIVersion,
		Kind:        domain.ManifestKind,
		Application: "orders-api",
		Runtime:     domain.RuntimeKindGo,
		Artifact:    domain.SpecArtifact{ID: "art_1"},
		Exec: domain.SpecExec{
			Argv:             []string{"bin/server"},
			WorkingDirectory: "/var/lib/orders-api",
			RunUser:          "orders-api",
			Slots: map[domain.Slot]domain.SpecSlot{
				domain.SlotBlue: {
					Ports:     []int{18081},
					Readiness: domain.SpecReadiness{Type: domain.ReadinessTCP, Target: "127.0.0.1:18081"},
				},
				domain.SlotGreen: {
					Ports:     []int{18082},
					Readiness: domain.SpecReadiness{Type: domain.ReadinessTCP, Target: "127.0.0.1:18082"},
				},
			},
		},
		Health: domain.SpecHealth{},
		Logs:   domain.SpecLogs{Directory: "/var/log/orders-api"},
		Nginx:  domain.SpecNginx{Listen: 8080, ServerName: "orders.example.com"},
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("夹具规格本身必须合法: %v", err)
	}

	// confDir 必须在 root 前缀下真实存在（Validate 会 stat 它）。
	confDir := "/etc/nginx"
	if err := os.MkdirAll(filepath.Join(root, confDir), 0o755); err != nil {
		t.Fatalf("建夹具配置目录: %v", err)
	}
	fake := &fakeNginx{}
	// 默认：主配置 include 了我们的目录（`-T` 的输出里带上受管文件那一行）。
	fake.dump = "# configuration file " + filepath.Join(confDir, "nginx.conf") + ":\n" +
		"# configuration file " + domain.ManagedNginxFile(confDir, spec.Application) + ":\n"

	adapter := New(Config{ConfDir: confDir},
		WithRoot(root), WithRunner(fake.run), WithGOOS("linux"))
	return &fixture{adapter: adapter, fake: fake, root: root, spec: spec, ctx: context.Background()}
}

// managed 读回受管文件的内容与是否存在。
func (f *fixture) managed(t *testing.T) (string, bool) {
	t.Helper()
	raw, err := os.ReadFile(f.adapter.RootPath(f.adapter.ManagedFile(f.spec.Application)))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false
		}
		t.Fatalf("读受管文件: %v", err)
	}
	return string(raw), true
}

// ==== 渲染 ====

func TestRenderManagedPointsAtTargetSlot(t *testing.T) {
	spec := newFixture(t).spec
	content, err := RenderManaged(spec, domain.SlotGreen, "rel_1", "1.2.3")
	if err != nil {
		t.Fatalf("RenderManaged: %v", err)
	}
	for _, want := range []string{
		"upstream frz_orders-api {",
		"server 127.0.0.1:18082;", // green 的端口
		"listen 8080;",
		"server_name orders.example.com;",
		"proxy_pass http://frz_orders-api;",
		"# 现在指向：green（版本 1.2.3）（release rel_1）",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("受管配置缺少 %q：\n%s", want, content)
		}
	}
	// 另一个槽位的端口绝不能出现在 upstream 里——那正是「切流没生效」的样子。
	if strings.Contains(content, "18081") {
		t.Fatalf("green 的配置里不该出现 blue 的端口：\n%s", content)
	}
}

func TestRenderManagedRejectsNonBlueGreen(t *testing.T) {
	spec := newFixture(t).spec
	single := *spec
	single.Exec.Slots = nil
	single.Nginx = domain.SpecNginx{}
	if _, err := RenderManaged(&single, domain.SlotBlue, "", ""); domain.CodeOf(err) != v1.CodeManifestInvalid {
		t.Fatalf("非蓝绿应用应当被拒，got %v", err)
	}
	if _, err := RenderManaged(spec, domain.Slot("purple"), "", ""); err == nil {
		t.Fatal("不存在的槽位应当被拒")
	}
}

// 渲染与解析必须是一对：写出去的那份文件，读回来必须指向同一个槽位。
func TestParseManagedRoundTrip(t *testing.T) {
	spec := newFixture(t).spec
	for _, slot := range domain.Slots {
		content, err := RenderManaged(spec, slot, "", "")
		if err != nil {
			t.Fatalf("RenderManaged(%s): %v", slot, err)
		}
		got, ok := ParseManaged(content, spec)
		if !ok || got != slot {
			t.Fatalf("want %s, got %q（ok=%v）", slot, got, ok)
		}
	}
	if _, ok := ParseManaged("这是别人手写的配置\n", spec); ok {
		t.Fatal("读不出槽位时不该猜一个")
	}
}

// ==== 校验与「有没有被加载」 ====

func TestValidateRejectsNonLinuxAndMissingNginx(t *testing.T) {
	f := newFixture(t)

	darwin := New(Config{ConfDir: "/etc/nginx"}, WithRoot(f.root), WithRunner(f.fake.run), WithGOOS("darwin"))
	if code := domain.CodeOf(darwin.Validate(f.ctx, f.spec)); code != v1.CodeRuntimeUnsupport {
		t.Fatalf("非 Linux 上 want RUNTIME_UNSUPPORTED, got %s", code)
	}

	f.fake.missingBinary = true
	if code := domain.CodeOf(f.adapter.Validate(f.ctx, f.spec)); code != v1.CodeRuntimeUnsupport {
		t.Fatalf("没有 nginx 时 want RUNTIME_UNSUPPORTED, got %s", code)
	}
}

// 「配置写对了但没生效」是最危险的中间态：主配置没 include 我们的目录时，切流看起来会成功，
// 而流量根本不经过我们改的 upstream。这条断言必须给出**可操作**的下一步。
func TestCheckLoadedRejectsUnincludedManagedDir(t *testing.T) {
	f := newFixture(t)
	f.fake.dump = "# configuration file /etc/nginx/nginx.conf:\n"
	err := f.adapter.CheckLoaded(f.ctx, f.spec)
	if domain.CodeOf(err) != v1.CodeNginxConfigInvalid {
		t.Fatalf("want NGINX_CONFIG_INVALID, got %v", err)
	}
	if !strings.Contains(err.Error(), "include") {
		t.Fatalf("报错要给出可操作的下一步（include 那一行）：%v", err)
	}

	f.fake.dump = "# configuration file " + domain.ManagedNginxFile("/etc/nginx", "orders-api") + ":\n"
	if err := f.adapter.CheckLoaded(f.ctx, f.spec); err != nil {
		t.Fatalf("被加载时应当通过: %v", err)
	}
}

// ==== Apply ====

func TestApplyWritesConfigAndReloads(t *testing.T) {
	f := newFixture(t)
	if err := f.adapter.Apply(f.ctx, f.spec, domain.SlotGreen); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	content, present := f.managed(t)
	if !present {
		t.Fatal("受管文件应当被写出来")
	}
	if !strings.Contains(content, "server 127.0.0.1:18082;") {
		t.Fatalf("受管文件没有指向 green：\n%s", content)
	}
	info, err := os.Stat(f.adapter.RootPath(f.adapter.ManagedFile(f.spec.Application)))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("受管文件模式 want 0644, got %04o", info.Mode().Perm())
	}
	// 命令序列：T（有没有被加载）→ t（校验）→ s reload（切流）。顺序是语义的一部分：
	// 校验必须在 reload 之前，否则一次坏配置就会被带上线。
	var names []string
	for _, argv := range f.fake.commands {
		if len(argv) > 1 {
			names = append(names, strings.Join(argv[1:], " "))
		}
	}
	joined := strings.Join(names, " | ")
	tIndex := strings.Index(joined, "-t -c /etc/nginx/nginx.conf")
	reloadIndex := strings.Index(joined, "-s reload")
	dumpIndex := strings.Index(joined, "-T -c /etc/nginx/nginx.conf")
	if tIndex < 0 || reloadIndex < 0 || dumpIndex < 0 || !(dumpIndex < tIndex && tIndex < reloadIndex) {
		t.Fatalf("命令序列应当是 T → t → s reload，got %v", names)
	}
	// 当前指向也得能读回来。
	slot, err := f.adapter.Current(f.ctx, f.spec)
	if err != nil || slot != domain.SlotGreen {
		t.Fatalf("Current want green, got %q err=%v", slot, err)
	}
}

// 校验失败：**换回原内容，且不 reload**（流量一点没动）。
func TestApplyRestoresConfigWhenValidationFails(t *testing.T) {
	f := newFixture(t)
	// 先成功切到 blue，作为「原内容」。
	if err := f.adapter.Apply(f.ctx, f.spec, domain.SlotBlue); err != nil {
		t.Fatalf("第一次 Apply: %v", err)
	}
	before, _ := f.managed(t)
	beforeReloads := countReloads(f.fake.commands)

	f.fake.failTest = true
	err := f.adapter.Apply(f.ctx, f.spec, domain.SlotGreen)
	if domain.CodeOf(err) != v1.CodeNginxConfigInvalid {
		t.Fatalf("want NGINX_CONFIG_INVALID, got %v", err)
	}
	after, present := f.managed(t)
	if !present || after != before {
		t.Fatalf("校验失败必须换回原内容：\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
	if countReloads(f.fake.commands) != beforeReloads {
		t.Fatal("校验失败时**不该** reload：流量必须一点没动")
	}
}

// 第一次部署（没有原内容）时校验失败：文件要被删掉，而不是留一份坏配置在盘上。
func TestApplyRemovesConfigWhenFirstValidationFails(t *testing.T) {
	f := newFixture(t)
	f.fake.failTest = true
	if err := f.adapter.Apply(f.ctx, f.spec, domain.SlotGreen); domain.CodeOf(err) != v1.CodeNginxConfigInvalid {
		t.Fatalf("want NGINX_CONFIG_INVALID, got %v", err)
	}
	if _, present := f.managed(t); present {
		t.Fatal("第一次部署校验失败时不该留下受管文件")
	}
}

// reload 失败但换回成功：报 NGINX_RELOAD_FAILED，并**说清流量还在原来那一侧**。
func TestApplyRestoresAndReloadsWhenReloadFails(t *testing.T) {
	f := newFixture(t)
	if err := f.adapter.Apply(f.ctx, f.spec, domain.SlotBlue); err != nil {
		t.Fatalf("第一次 Apply: %v", err)
	}
	before, _ := f.managed(t)

	// 只让第一次 reload 失败：换回原配置之后的第二次 reload 会成功。
	f.fake.failReloadOnce = true
	err := f.adapter.Apply(f.ctx, f.spec, domain.SlotGreen)
	if domain.CodeOf(err) != v1.CodeNginxReloadFailed {
		t.Fatalf("want NGINX_RELOAD_FAILED, got %v", err)
	}
	if !strings.Contains(err.Error(), "原来的槽位") {
		t.Fatalf("换回成功时要说清流量仍在原处：%v", err)
	}
	after, present := f.managed(t)
	if !present || after != before {
		t.Fatalf("reload 失败后应当换回原内容：\n%s", after)
	}
}

// reload 失败且换回之后再次 reload 也失败：**不能报「已回滚」**——现在没有任何东西能保证
// 流量在哪一侧，报错必须这么说。
func TestApplyReportsUncertainStateWhenSecondReloadFails(t *testing.T) {
	f := newFixture(t)
	if err := f.adapter.Apply(f.ctx, f.spec, domain.SlotBlue); err != nil {
		t.Fatalf("第一次 Apply: %v", err)
	}
	f.fake.failReload = true
	// 第二次 reload 也失败：让 runner 从第二次开始永远失败（failReload 已经是这样）。
	err := f.adapter.Apply(f.ctx, f.spec, domain.SlotGreen)
	if domain.CodeOf(err) != v1.CodeNginxReloadFailed {
		t.Fatalf("want NGINX_RELOAD_FAILED, got %v", err)
	}
	if !strings.Contains(err.Error(), "不确定") {
		t.Fatalf("两次 reload 都失败时必须说「流量状态不确定」：%v", err)
	}
	if strings.Contains(err.Error(), "已回到") {
		t.Fatalf("不该声称已经回到旧版本：%v", err)
	}
}

// Current 在还没有受管文件时返回空槽位（「还没部署过」不是错误）。
func TestCurrentWithoutManagedFile(t *testing.T) {
	f := newFixture(t)
	slot, err := f.adapter.Current(f.ctx, f.spec)
	if err != nil || slot != "" {
		t.Fatalf("want 空槽位与 nil，got %q err=%v", slot, err)
	}
}

func countReloads(commands [][]string) int {
	count := 0
	for _, argv := range commands {
		if len(argv) > 2 && argv[1] == "-s" && argv[2] == "reload" {
			count++
		}
	}
	return count
}

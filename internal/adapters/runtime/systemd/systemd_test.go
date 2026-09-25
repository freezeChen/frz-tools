package systemd_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/release/local"
	"github.com/freezeChen/frz-tools/internal/adapters/runtime/systemd"
	"github.com/freezeChen/frz-tools/internal/adapters/runtime/unitfile"
	"github.com/freezeChen/frz-tools/internal/domain"
)

type harness struct {
	adapter  *systemd.Adapter
	host     *fakeHost
	root     string
	logs     *bytes.Buffer
	now      *time.Time
	uid, gid int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	host := newFakeHost(root)
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	uid, gid, err := currentIDs()
	if err != nil {
		t.Fatalf("解析当前用户失败: %v", err)
	}

	h := &harness{root: root, host: host, logs: &bytes.Buffer{}, now: &now, uid: uid, gid: gid}
	h.adapter = systemd.New(root, &stubResolver{values: secretValues()},
		systemd.WithRunner(host.run),
		systemd.WithGOOS("linux"),
		systemd.WithOwnerResolver(currentOwner),
		systemd.WithClock(func() time.Time { return *h.now }),
		systemd.WithLogger(slog.New(slog.NewTextHandler(h.logs, nil))),
	)
	return h
}

func (h *harness) advance(d time.Duration) { *h.now = h.now.Add(d) }

func assertCommands(t *testing.T, got, want [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("命令序列不符：\n--- got ---\n%s\n--- want ---\n%s", formatCommands(got), formatCommands(want))
	}
	for i := range want {
		if strings.Join(got[i], "\x00") != strings.Join(want[i], "\x00") {
			t.Fatalf("第 %d 条命令不符：got %v, want %v", i, got[i], want[i])
		}
	}
}

// Validate 在非 Linux 上必须明确说不支持，而不是假装支持然后让命令在运行时全失败。
func TestValidateRejectsNonLinux(t *testing.T) {
	root := t.TempDir()
	host := newFakeHost(root)
	spec := specFixture(t, "nonlinux")

	faked := systemd.New(root, &stubResolver{values: secretValues()},
		systemd.WithRunner(host.run), systemd.WithGOOS("darwin"))
	err := faked.Validate(context.Background(), spec)
	if domain.CodeOf(err) != v1.CodeRuntimeUnsupport {
		t.Fatalf("want RUNTIME_UNSUPPORTED, got %v", err)
	}

	// 平台判定默认取 runtime.GOOS：在非 Linux 开发机上，不注入也必须拒绝。
	if runtime.GOOS != "linux" {
		real := systemd.New(root, &stubResolver{values: secretValues()}, systemd.WithRunner(host.run))
		if code := domain.CodeOf(real.Validate(context.Background(), spec)); code != v1.CodeRuntimeUnsupport {
			t.Fatalf("真实平台判定下 want RUNTIME_UNSUPPORTED, got %s", code)
		}
	}
}

// 没有 systemd（或版本低于 219）时必须在「还没建用户、没建目录」的阶段失败。
func TestValidateReportsUnusableSystemd(t *testing.T) {
	t.Run("systemctl 不可执行", func(t *testing.T) {
		h := newHarness(t)
		h.host.runnerErr = errBoom
		err := h.adapter.Validate(context.Background(), specFixture(t, "nosystemd"))
		if domain.CodeOf(err) != v1.CodeRuntimeUnsupport {
			t.Fatalf("want RUNTIME_UNSUPPORTED, got %v", err)
		}
	})

	t.Run("输出不可解析", func(t *testing.T) {
		h := newHarness(t)
		h.host.versionOutput = ""
		err := h.adapter.Validate(context.Background(), specFixture(t, "garbage"))
		if domain.CodeOf(err) != v1.CodeRuntimeUnsupport {
			t.Fatalf("want RUNTIME_UNSUPPORTED, got %v", err)
		}
	})

	t.Run("版本低于 219", func(t *testing.T) {
		h := newHarness(t)
		h.host.versionOutput = "systemd 218\n"
		err := h.adapter.Validate(context.Background(), specFixture(t, "old"))
		if domain.CodeOf(err) != v1.CodeRuntimeUnsupport {
			t.Fatalf("want RUNTIME_UNSUPPORTED, got %v", err)
		}
	})
}

// Validate 无副作用：非法规格在探测 systemd 之前就被拒绝，且没有留下任何文件。
func TestValidateIsSideEffectFree(t *testing.T) {
	h := newHarness(t)
	spec := specFixture(t, "novalidate")
	spec.Exec.Argv = nil

	err := h.adapter.Validate(context.Background(), spec)
	if domain.CodeOf(err) != v1.CodeManifestInvalid {
		t.Fatalf("want MANIFEST_INVALID, got %v", err)
	}
	if commands := h.host.takeCommands(); len(commands) != 0 {
		t.Fatalf("非法规格不该触发任何命令: %s", formatCommands(commands))
	}
	assertTreeEmpty(t, h.root)
}

// Prepare 的命令序列是这一层最重要的证据：它说明 opsd 到底对主机做了什么。
func TestPrepareCommandSequenceIsIdempotent(t *testing.T) {
	h := newHarness(t)
	spec := specFixture(t, "orders")
	ctx := context.Background()

	mustRun(t, h.adapter.Prepare(ctx, spec, ""))
	assertCommands(t, h.host.takeCommands(), [][]string{
		{"systemctl", "--version"},
		{"id", "-u", "appuser"},
		{"useradd", "--system", "--no-create-home", "--home-dir", "/var/lib/orders", "--shell", "/usr/sbin/nologin", "appuser"},
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "orders.service"},
	})

	// 第二次：不重复 useradd；文件内容与权限已经正确，因此也不重写、不重新 daemon-reload。
	// enable 仍然执行：它幂等，且「上一次写完 unit 但 enable 失败」只能靠重跑修好。
	mustRun(t, h.adapter.Prepare(ctx, spec, ""))
	assertCommands(t, h.host.takeCommands(), [][]string{
		{"id", "-u", "appuser"},
		{"systemctl", "enable", "orders.service"},
	})
}

func TestPrepareDoesNotTouchExistingUser(t *testing.T) {
	h := newHarness(t)
	h.host.users["appuser"] = true

	mustRun(t, h.adapter.Prepare(context.Background(), specFixture(t, "existing"), ""))

	for _, argv := range h.host.takeCommands() {
		if argv[0] == "useradd" {
			t.Fatalf("用户已存在时不得再 useradd（会改已有用户的属性）: %v", argv)
		}
	}
}

func TestPrepareWritesFilesWithExpectedModesAndOwners(t *testing.T) {
	h := newHarness(t)
	spec := specFixture(t, "orders")
	mustRun(t, h.adapter.Prepare(context.Background(), spec, ""))

	for path, want := range map[string]struct {
		mode    os.FileMode
		content string
	}{
		domain.EnvFilePath("orders"): {
			mode:    0o600,
			content: "GOMEMLIMIT=\"40MiB\"\n",
		},
		domain.SecretsEnvFilePath("orders"): {
			mode: 0o600,
			// kind=file 的变量里是路径（真实路径，不是 root 前缀下的路径），
			// kind=env 的值按 systemd 的转义规则整体加引号并翻倍反斜杠。
			content: `APP_KEY="/etc/opsd/apps/orders.secrets/APP_KEY"
APP_TOKEN="tok\\en\"v\""
`,
		},
		filepath.Join(domain.SecretsDir("orders"), "APP_KEY"): {
			mode:    0o600,
			content: "line1\nline2\n",
		},
	} {
		assertFile(t, h.adapter.RootPath(path), want.mode, want.content, h.uid, h.gid)
	}

	for path, mode := range map[string]os.FileMode{
		spec.Exec.WorkingDirectory:       0o750,
		spec.Logs.Directory:              0o750,
		"/opt/opsd/apps/orders/releases": 0o750,
		domain.SecretsDir("orders"):      0o700,
	} {
		assertModeAndOwner(t, h.adapter.RootPath(path), mode, h.uid, h.gid)
	}

	// unit 文件由探测到的版本与规格共同决定：档位、路径与可写路径都在正文里。
	unitPath := h.adapter.RootPath(domain.UnitPath("orders.service"))
	assertModeAndOwner(t, unitPath, 0o644, h.uid, h.gid)
	content := readFile(t, unitPath)
	for _, want := range []string{
		"# systemd 版本=255 档位=strict",
		"WorkingDirectory=/var/lib/orders",
		"EnvironmentFile=-/etc/opsd/apps/orders.env",
		"EnvironmentFile=/etc/opsd/apps/orders.secrets.env",
		"ExecStart=/opt/opsd/apps/orders/bin/run",
		"ProtectSystem=strict",
		"ReadWritePaths=/var/lib/orders /var/log/orders /opt/opsd/apps/orders/releases",
		"StandardOutput=append:/var/log/orders/current.log",
	} {
		if !strings.Contains(content, want+"\n") {
			t.Fatalf("unit 缺少 %q:\n%s", want, content)
		}
	}
}

// 属主不是适配器硬编码的：解析不出 uid/gid 时必须整段失败，而不是悄悄写出 root 属主的文件。
func TestPrepareFailsWhenOwnerCannotBeApplied(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("以 root 运行时无法制造 EPERM，这条断言失去意义")
	}
	h := newHarness(t)
	h.adapter = systemd.New(h.root, &stubResolver{values: secretValues()},
		systemd.WithRunner(h.host.run),
		systemd.WithGOOS("linux"),
		// root 属主：非 root 进程 chown 到 0 必然失败。
		systemd.WithOwnerResolver(func(string) (int, int, error) { return 0, 0, nil }),
	)

	err := h.adapter.Prepare(context.Background(), specFixture(t, "owned"), "")
	if err == nil {
		t.Fatal("无法设置属主时必须报错，否则会留下一批 root 属主的应用文件")
	}
	if code := domain.CodeOf(err); code != v1.CodeInternal {
		t.Fatalf("want INTERNAL, got %s (%v)", code, err)
	}
}

// kind=env 的凭据含换行必须被拒绝，且错误信息要指向具体 manifest 字段。
func TestPrepareRejectsMultilineEnvSecret(t *testing.T) {
	h := newHarness(t)
	h.adapter = systemd.New(h.root, &stubResolver{values: map[string]string{
		"env:APP_TOKEN":                  "line1\nline2",
		"file:/etc/opsd/secrets/app.key": "line1\nline2\n",
	}}, systemd.WithRunner(h.host.run), systemd.WithGOOS("linux"),
		systemd.WithOwnerResolver(currentOwner))

	err := h.adapter.Prepare(context.Background(), specFixture(t, "multiline"), "")
	if domain.CodeOf(err) != v1.CodeSecretUnresolved {
		t.Fatalf("want SECRET_UNRESOLVED, got %v", err)
	}
	if !strings.Contains(err.Error(), `secretEnvironment["APP_TOKEN"]`) {
		t.Fatalf("错误信息必须指向具体字段，got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "kind: file") {
		t.Fatalf("错误信息必须给出可行的替代方案（kind: file），got %q", err.Error())
	}
	// 解析先于落盘：一条含换行的凭据不该留下半套文件。
	assertTreeEmpty(t, h.root)
}

// kind=file 的多行凭据必须能正常交付（这正是它存在的理由）。
func TestPrepareDeliversMultilineFileSecret(t *testing.T) {
	h := newHarness(t)
	spec := specFixture(t, "pem")
	spec.Exec.SecretEnvironment = map[string]domain.SecretRef{
		"TLS_KEY": {Kind: domain.SecretKindFile, Name: "/etc/opsd/secrets/tls.key"},
	}
	pem := "-----BEGIN KEY-----\nAAAA\nBBBB\n-----END KEY-----\n"
	h.adapter = systemd.New(h.root, &stubResolver{values: map[string]string{"file:/etc/opsd/secrets/tls.key": pem}},
		systemd.WithRunner(h.host.run), systemd.WithGOOS("linux"), systemd.WithOwnerResolver(currentOwner))

	mustRun(t, h.adapter.Prepare(context.Background(), spec, ""))

	assertFile(t, h.adapter.RootPath(filepath.Join(domain.SecretsDir("pem"), "TLS_KEY")), 0o600, pem, h.uid, h.gid)
	// 环境变量里传的是路径，不是内容。
	assertFile(t, h.adapter.RootPath(domain.SecretsEnvFilePath("pem")), 0o600,
		"TLS_KEY=\"/etc/opsd/apps/pem.secrets/TLS_KEY\"\n", h.uid, h.gid)
}

func TestStartStopIssueSystemctlCommands(t *testing.T) {
	h := newHarness(t)
	spec := specFixture(t, "orders")
	ctx := context.Background()
	mustRun(t, h.adapter.Prepare(ctx, spec, ""))
	h.host.takeCommands()

	mustRun(t, h.adapter.Start(ctx, spec, ""))
	// Start 先问状态再启动：已经 active 的 unit 不该被再 start 一次。
	assertCommands(t, h.host.takeCommands(), [][]string{
		{"systemctl", "show", "-p", "ActiveState", "orders.service"},
		{"systemctl", "start", "orders.service"},
	})

	status, err := h.adapter.Status(ctx, spec, "")
	mustRun(t, err)
	if status != domain.RuntimeActive {
		t.Fatalf("want active, got %s", status)
	}
	assertCommands(t, h.host.takeCommands(), [][]string{
		{"systemctl", "show", "-p", "ActiveState", "orders.service"},
	})

	// 重复 Start：只查询、不重复启动。
	mustRun(t, h.adapter.Start(ctx, spec, ""))
	assertCommands(t, h.host.takeCommands(), [][]string{
		{"systemctl", "show", "-p", "ActiveState", "orders.service"},
	})

	mustRun(t, h.adapter.Stop(ctx, spec, ""))
	assertCommands(t, h.host.takeCommands(), [][]string{
		{"systemctl", "stop", "orders.service"},
	})
	status, err = h.adapter.Status(ctx, spec, "")
	mustRun(t, err)
	if status != domain.RuntimeInactive {
		t.Fatalf("want inactive, got %s", status)
	}
}

func TestStartRequiresPrepare(t *testing.T) {
	h := newHarness(t)
	spec := specFixture(t, "unprepared")

	err := h.adapter.Start(context.Background(), spec, "")
	if domain.CodeOf(err) != v1.CodeRuntimeNotReady {
		t.Fatalf("want RUNTIME_NOT_READY, got %v", err)
	}
	for _, argv := range h.host.takeCommands() {
		if argv[1] == "start" {
			t.Fatalf("未 Prepare 不得启动任何东西: %v", argv)
		}
	}
}

// Status 的映射规则：认不出来的取值落到 unknown，绝不落到 active。
func TestStatusMapsActiveState(t *testing.T) {
	cases := []struct {
		name    string
		stdout  string
		want    domain.RuntimeStatus
		wantErr bool
	}{
		// `show -p ActiveState` 的真实输出形态。
		{name: "active", stdout: "ActiveState=active\n", want: domain.RuntimeActive},
		{name: "inactive", stdout: "ActiveState=inactive\n", want: domain.RuntimeInactive},
		{name: "failed", stdout: "ActiveState=failed\n", want: domain.RuntimeFailed},
		{name: "activating", stdout: "ActiveState=activating\n", want: domain.RuntimeActivating},
		{name: "deactivating", stdout: "ActiveState=deactivating\n", want: domain.RuntimeDeactivating},
		{name: "unknown", stdout: "ActiveState=unknown\n", want: domain.RuntimeUnknown},
		// 只打印值的形态也要认：不同档位的 systemctl 输出形态不同，而状态含义是一样的。
		{name: "只打印值", stdout: "active\n", want: domain.RuntimeActive},
		{name: "未来新增的状态", stdout: "ActiveState=maintenance\n", want: domain.RuntimeUnknown},
		// 问了 ActiveState 却拿到了别的属性：这说明输出形态变了，宁可 unknown 也不要
		// 把 "loaded" 之类的词硬套成某个状态。
		{name: "问到的不是 ActiveState", stdout: "LoadState=loaded\n", want: domain.RuntimeUnknown},
		{name: "值缺失", stdout: "ActiveState=\n", wantErr: true},
		{name: "空输出", stdout: "", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			spec := specFixture(t, "status")
			mustRun(t, h.adapter.Prepare(context.Background(), spec, ""))
			h.host.showOverride = &systemd.Result{Stdout: tc.stdout}

			status, err := h.adapter.Status(context.Background(), spec, "")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("必须报错，却得到 %s", status)
				}
				if code := domain.CodeOf(err); code != v1.CodeInternal {
					t.Fatalf("want INTERNAL, got %s (%v)", code, err)
				}
				return
			}
			mustRun(t, err)
			if status != tc.want {
				t.Fatalf("got %s, want %s", status, tc.want)
			}
		})
	}
}

func TestStatusFailurePaths(t *testing.T) {
	t.Run("unit 未安装时是不在运行", func(t *testing.T) {
		h := newHarness(t)
		// 假主机的 show 对不存在的 unit 返回「找不到」，与真机一致。
		status, err := h.adapter.Status(context.Background(), specFixture(t, "missing"), "")
		mustRun(t, err)
		if status != domain.RuntimeInactive {
			t.Fatalf("want inactive, got %s", status)
		}
	})

	t.Run("unit 已安装但查询失败是错误", func(t *testing.T) {
		h := newHarness(t)
		spec := specFixture(t, "broken")
		mustRun(t, h.adapter.Prepare(context.Background(), spec, ""))
		h.host.showOverride = &systemd.Result{ExitCode: 4, Stderr: "transport endpoint is not connected"}

		_, err := h.adapter.Status(context.Background(), spec, "")
		if domain.CodeOf(err) != v1.CodeInternal {
			t.Fatalf("want INTERNAL, got %v", err)
		}
		if !strings.Contains(err.Error(), "transport endpoint") {
			t.Fatalf("错误信息必须带 stderr，got %q", err.Error())
		}
	})

	t.Run("命令无法执行是错误", func(t *testing.T) {
		h := newHarness(t)
		spec := specFixture(t, "noexec")
		mustRun(t, h.adapter.Prepare(context.Background(), spec, ""))
		h.host.runnerErr = errBoom

		_, err := h.adapter.Status(context.Background(), spec, "")
		if domain.CodeOf(err) != v1.CodeInternal {
			t.Fatalf("want INTERNAL, got %v", err)
		}
	})
}

func TestHealthLifecycle(t *testing.T) {
	ctx := context.Background()

	t.Run("未启动时返回不就绪快照而不是错误", func(t *testing.T) {
		h := newHarness(t)
		spec := specFixture(t, "idle")
		mustRun(t, h.adapter.Prepare(ctx, spec, ""))

		health, err := h.adapter.Health(ctx, spec, "")
		mustRun(t, err)
		if health.Ready {
			t.Fatal("未启动不得报告就绪")
		}
		if !health.CheckedAt.Equal(*h.now) {
			t.Fatalf("CheckedAt 必须来自注入的时钟，got %s", health.CheckedAt)
		}
	})

	t.Run("failed 是硬错误", func(t *testing.T) {
		h := newHarness(t)
		spec := specFixture(t, "failed")
		mustRun(t, h.adapter.Prepare(ctx, spec, ""))
		h.host.setState(spec.Systemd.UnitName, domain.RuntimeFailed)

		_, err := h.adapter.Health(ctx, spec, "")
		if domain.CodeOf(err) != v1.CodeRuntimeNotReady {
			t.Fatalf("want RUNTIME_NOT_READY, got %v", err)
		}
	})

	t.Run("连续成功次数未满足时不报就绪", func(t *testing.T) {
		h := newHarness(t)
		spec := specFixture(t, "consecutive")
		spec.Health.Readiness.ConsecutiveSuccesses = 2
		stop := serveTCP(t, spec)
		defer stop()
		mustRun(t, h.adapter.Prepare(ctx, spec, ""))
		mustRun(t, h.adapter.Start(ctx, spec, ""))

		health, err := h.adapter.Health(ctx, spec, "")
		mustRun(t, err)
		if health.Ready {
			t.Fatalf("只通过 1 次就报就绪，detail=%q", health.Detail)
		}
		if !strings.Contains(health.Detail, "1/2") {
			t.Fatalf("detail 应当说明进度，got %q", health.Detail)
		}

		health, err = h.adapter.Health(ctx, spec, "")
		mustRun(t, err)
		if !health.Ready {
			t.Fatalf("连续通过 2 次后必须报就绪，detail=%q", health.Detail)
		}
	})

	t.Run("就绪目标不可达时不报就绪", func(t *testing.T) {
		h := newHarness(t)
		spec := specFixture(t, "unready") // 127.0.0.1:1 上没有人监听
		mustRun(t, h.adapter.Prepare(ctx, spec, ""))
		mustRun(t, h.adapter.Start(ctx, spec, ""))

		health, err := h.adapter.Health(ctx, spec, "")
		mustRun(t, err)
		if health.Ready {
			t.Fatal("就绪目标不可达时不得报告就绪")
		}
		if !strings.Contains(health.Detail, "TCP 探测失败") {
			t.Fatalf("detail 应当说明探测失败，got %q", health.Detail)
		}
	})

	t.Run("探测受超时约束，不会无限期挂住", func(t *testing.T) {
		// 本地起一个永不响应的 HTTP 服务：只有 probe 超时能结束这次探测。
		// 用一个真实但慢的目标，比挑一个「应该不可达」的地址可靠——后者在有
		// 透明代理或防火墙改写语义的环境里会连上，测试就变成了环境测试。
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		mustRun(t, err)
		release := make(chan struct{})
		server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			<-release
		})}
		go func() { _ = server.Serve(listener) }()
		defer func() {
			close(release)
			_ = server.Close()
		}()

		h := newHarness(t)
		h.adapter = systemd.New(h.root, &stubResolver{values: secretValues()},
			systemd.WithRunner(h.host.run),
			systemd.WithGOOS("linux"),
			systemd.WithOwnerResolver(currentOwner),
			systemd.WithProbeTimeout(50*time.Millisecond),
		)
		spec := specFixture(t, "slowprobe")
		spec.Health.Readiness = domain.SpecReadiness{
			Type:                 domain.ReadinessHTTP,
			Target:               "http://" + listener.Addr().String() + "/healthz",
			ConsecutiveSuccesses: 1,
		}
		mustRun(t, h.adapter.Prepare(ctx, spec, ""))
		mustRun(t, h.adapter.Start(ctx, spec, ""))

		started := time.Now()
		health, err := h.adapter.Health(ctx, spec, "")
		elapsed := time.Since(started)
		mustRun(t, err)
		if health.Ready {
			t.Fatal("对端不应答时不得报告就绪")
		}
		if elapsed < 50*time.Millisecond || elapsed > 2*time.Second {
			t.Fatalf("探测必须被 probe 超时界住（约 50ms），实际等了 %s", elapsed)
		}
		if !strings.Contains(health.Detail, "HTTP 探测失败") {
			t.Fatalf("detail 应当说明探测失败，got %q", health.Detail)
		}
	})

	t.Run("超过 startTimeoutSeconds 仍未就绪是硬错误", func(t *testing.T) {
		h := newHarness(t)
		spec := specFixture(t, "timeout")
		mustRun(t, h.adapter.Prepare(ctx, spec, ""))
		mustRun(t, h.adapter.Start(ctx, spec, ""))

		// 预算内：快照，不是错误。
		health, err := h.adapter.Health(ctx, spec, "")
		mustRun(t, err)
		if health.Ready {
			t.Fatal("探测不通过时不得报告就绪")
		}

		h.advance(spec.Health.StartTimeout + time.Second)
		_, err = h.adapter.Health(ctx, spec, "")
		if domain.CodeOf(err) != v1.CodeRuntimeNotReady {
			t.Fatalf("启动超时必须报 RUNTIME_NOT_READY, got %v", err)
		}
	})
}

// 档位与探测到的版本必须可追溯：同一份 manifest 在不同主机上生成的 unit 不同，
// 没有这条记录就无从解释。两个出口都要有——日志（落盘、可 grep）与内存里的决策
// （供 API 层组装审计事件），两者内容必须一致。
func TestDecisionIsRecordedInLogAndMemory(t *testing.T) {
	ctx := context.Background()

	t.Run("新档主机", func(t *testing.T) {
		h := newHarness(t)
		spec := specFixture(t, "audit-new")
		mustRun(t, h.adapter.Prepare(ctx, spec, ""))

		decision, ok := h.adapter.UnitDecision("audit-new.service")
		if !ok {
			t.Fatal("决策必须留在内存里供查询")
		}
		if decision.Tier != unitfile.TierStrict || decision.SystemdVersion != 255 {
			t.Fatalf("档位/版本不对: %+v", decision)
		}
		if len(decision.Degradations) != 0 {
			t.Fatalf("strict 档不该有降级说明: %v", decision.Degradations)
		}
		if decision.UnitPath != domain.UnitPath("audit-new.service") {
			t.Fatalf("UnitPath 不对: %s", decision.UnitPath)
		}
		if !decision.DecidedAt.Equal(*h.now) {
			t.Fatalf("决策时间必须来自注入的时钟: %s", decision.DecidedAt)
		}

		logged := h.logs.String()
		for _, want := range []string{"tier=strict", "systemdVersion=255", "unitPath=/etc/systemd/system/audit-new.service"} {
			if !strings.Contains(logged, want) {
				t.Fatalf("日志缺少 %q:\n%s", want, logged)
			}
		}
	})

	t.Run("旧档主机", func(t *testing.T) {
		h := newHarness(t)
		h.host.versionOutput = "systemd 219\n+PAM +AUDIT\n"
		spec := specFixture(t, "audit-old")
		mustRun(t, h.adapter.Prepare(ctx, spec, ""))

		decision, _ := h.adapter.UnitDecision("audit-old.service")
		if decision.SystemdVersion != 219 || decision.Tier != unitfile.TierLegacy {
			t.Fatalf("档位/版本不对: %+v", decision)
		}
		if len(decision.Degradations) == 0 {
			t.Fatal("legacy 档必须把降级说明交出去")
		}
		if logged := h.logs.String(); !strings.Contains(logged, "tier=legacy") || !strings.Contains(logged, "journal") {
			t.Fatalf("日志必须记录降级：\n%s", logged)
		}

		content := readFile(t, h.adapter.RootPath(domain.UnitPath("audit-old.service")))
		if !strings.Contains(content, "StandardOutput=journal\n") {
			t.Fatalf("219 主机必须渲染 legacy 档:\n%s", content)
		}
	})
}

// MkdirAll 与 WriteFile 都不会修正已存在对象的权限：上一次留下的 0755 目录、
// 0644 的环境文件必须在 Prepare 时被显式收敛回规格值。
func TestPrepareConvergesPreExistingPermissions(t *testing.T) {
	h := newHarness(t)
	spec := specFixture(t, "converge")
	ctx := context.Background()

	// 先按错误权限与内容把产物造出来，模拟「上一次运行留下的」或「运维手工建的」。
	for path, mode := range map[string]os.FileMode{
		spec.Exec.WorkingDirectory:         0o755,
		spec.Logs.Directory:                0o755,
		"/opt/opsd/apps/converge/releases": 0o755,
		domain.SecretsDir("converge"):      0o755,
	} {
		target := h.adapter.RootPath(path)
		mustRun(t, os.MkdirAll(target, mode))
		mustRun(t, os.Chmod(target, mode))
	}
	envPath := h.adapter.RootPath(domain.EnvFilePath("converge"))
	mustRun(t, os.MkdirAll(filepath.Dir(envPath), 0o755))
	mustRun(t, os.WriteFile(envPath, []byte("STALE=1\n"), 0o644))

	mustRun(t, h.adapter.Prepare(ctx, spec, ""))

	for path, want := range map[string]os.FileMode{
		spec.Exec.WorkingDirectory:         0o750,
		spec.Logs.Directory:                0o750,
		"/opt/opsd/apps/converge/releases": 0o750,
		domain.SecretsDir("converge"):      0o700,
	} {
		assertModeAndOwner(t, h.adapter.RootPath(path), want, h.uid, h.gid)
	}
	assertFile(t, envPath, 0o600, "GOMEMLIMIT=\"40MiB\"\n", h.uid, h.gid)
}

// 路径上的「过路目录」必须是可穿越的：运行用户拥有叶子目录也没用，
// 缺了任何一级父目录的 +x 就进不去。这里同时压 umask——只用 MkdirAll 的机器
// 在 umask 077 下会把这些目录建成 0700，运行用户当场失去自己的凭据与工作目录。
func TestPrepareCreatesTraversableAncestorsRegardlessOfUmask(t *testing.T) {
	previous := syscall.Umask(0o077)
	defer syscall.Umask(previous)

	h := newHarness(t)
	spec := specFixture(t, "umask")
	mustRun(t, h.adapter.Prepare(context.Background(), spec, ""))

	for _, path := range []string{
		"/etc/opsd",
		"/etc/opsd/apps",
		"/etc/systemd/system",
		"/opt/opsd",
		"/opt/opsd/apps",
		"/opt/opsd/apps/umask",
	} {
		info, err := os.Stat(h.adapter.RootPath(path))
		if err != nil {
			t.Fatalf("stat %s 失败: %v", path, err)
		}
		if got := info.Mode().Perm(); got != 0o751 {
			t.Fatalf("%s 权限 = %o, want 0751（可穿越、不可列目录）", path, got)
		}
	}
	// 叶子依然按规格收敛。
	assertModeAndOwner(t, h.adapter.RootPath(spec.Exec.WorkingDirectory), 0o750, h.uid, h.gid)
	assertModeAndOwner(t, h.adapter.RootPath(domain.SecretsDir("umask")), 0o700, h.uid, h.gid)
}

// Prepare 的幂等必须落到文件系统上：第二次不再重写内容一致的文件，
// 否则每次 Prepare 都会刷新 mtime，进而让 systemd 看到「unit 变了」。
func TestPrepareDoesNotRewriteUnchangedFiles(t *testing.T) {
	h := newHarness(t)
	spec := specFixture(t, "stable")
	ctx := context.Background()
	mustRun(t, h.adapter.Prepare(ctx, spec, ""))

	watched := []string{
		h.adapter.RootPath(domain.UnitPath(spec.Systemd.UnitName)),
		h.adapter.RootPath(domain.EnvFilePath("stable")),
		h.adapter.RootPath(domain.SecretsEnvFilePath("stable")),
		h.adapter.RootPath(filepath.Join(domain.SecretsDir("stable"), "APP_KEY")),
	}
	before := map[string]os.FileInfo{}
	for _, path := range watched {
		info, err := os.Stat(path)
		mustRun(t, err)
		before[path] = info
	}

	mustRun(t, h.adapter.Prepare(ctx, spec, ""))

	for _, path := range watched {
		info, err := os.Stat(path)
		mustRun(t, err)
		if !info.ModTime().Equal(before[path].ModTime()) {
			t.Fatalf("%s 被无谓地重写了（mtime %s → %s）", path, before[path].ModTime(), info.ModTime())
		}
	}
}

// kind=file 的凭据是应用以 runUser 身份自己按路径打开的：路径上每一级都必须让
// runUser 穿得进去。生产上 runUser 既不是 /etc/opsd 的属主（属主是 opsd 自己），
// 通常也不在它的组里，所以真正起作用的是 other 的 +x 位——这就是本测试断言的不变量。
func TestCredentialPathIsReachableByRunUser(t *testing.T) {
	h := newHarness(t)
	spec := specFixture(t, "creds")
	mustRun(t, h.adapter.Prepare(context.Background(), spec, ""))

	// 过路目录：必须对 other 开放 +x（可穿越），且不需要读位。
	for path, wantMode := range map[string]os.FileMode{
		"/etc":           0o751,
		"/etc/opsd":      0o751,
		"/etc/opsd/apps": 0o751,
	} {
		assertModeAndOwner(t, h.adapter.RootPath(path), wantMode, h.uid, h.gid)
	}

	// 凭据目录与文件：按 §7 只属 runUser，模式不被放大。
	credentialDir := h.adapter.RootPath(domain.SecretsDir("creds"))
	assertModeAndOwner(t, credentialDir, 0o700, h.uid, h.gid)
	assertModeAndOwner(t, filepath.Join(credentialDir, "APP_KEY"), 0o600, h.uid, h.gid)

	// 关键不变量：凭据目录**之上**的每一级都对 other 有 +x——生产上 runUser 既不是这些
	// 目录的属主也不在其组里，真正起作用的就是 other 位。少一级，应用在真机上就 EACCES，
	// 而这一点本地测试看不见：本地 runUser 就是测试进程自己，走的是属主分支。
	for _, path := range []string{"/etc", "/etc/opsd", "/etc/opsd/apps"} {
		info, err := os.Stat(h.adapter.RootPath(path))
		mustRun(t, err)
		if info.Mode().Perm()&0o001 == 0 {
			t.Fatalf("%s 对 other 没有 +x（模式 %04o），runUser 可能穿不过去", path, info.Mode().Perm())
		}
	}
	// 凭据目录本身由 runUser 拥有（0700），它靠属主的 +x 进入，不需要对 other 开放。
	info, err := os.Stat(credentialDir)
	mustRun(t, err)
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("凭据目录属主没有 +x（模式 %04o），应用进不去自己的凭据目录", info.Mode().Perm())
	}
}

// 迭代 0 §9 的安装约定把 /etc/opsd 建成 0750、属主是 opsd 自己
// （test/linux/verify.sh 也断言 750）。这个模式对 runUser 是 other `---`，
// 会挡住 kind=file 的凭据读取；Prepare 必须把它补成可穿越，且只补穿越位。
func TestPrepareRepairsInstallerOwnedEtcOpsd(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing os.FileMode
		want     os.FileMode
	}{
		{name: "安装约定的 0750", existing: 0o750, want: 0o751},
		{name: "更严格的 0700", existing: 0o700, want: 0o711},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			etcOpsd := h.adapter.RootPath("/etc/opsd")
			mustRun(t, os.MkdirAll(etcOpsd, tc.existing))
			mustRun(t, os.Chmod(etcOpsd, tc.existing))

			mustRun(t, h.adapter.Prepare(context.Background(), specFixture(t, "repair"), ""))

			info, err := os.Stat(etcOpsd)
			mustRun(t, err)
			if got := info.Mode().Perm(); got != tc.want {
				t.Fatalf("/etc/opsd 模式 = %04o, want %04o（只补穿越位）", got, tc.want)
			}
			// 只补 +x：other 依然不能读，opsd 自己的 config.yaml / secrets 保护不变。
			if got := info.Mode().Perm(); got&0o004 != 0 {
				t.Fatalf("不得给 other 放开读位，模式 = %04o", got)
			}
		})
	}
}

// 没有 kind=file 凭据时不该动任何共享目录的权限位：Prepare 的作用域越小越好。
func TestPrepareWithoutFileSecretsLeavesSharedDirsAlone(t *testing.T) {
	h := newHarness(t)
	etcOpsd := h.adapter.RootPath("/etc/opsd")
	mustRun(t, os.MkdirAll(etcOpsd, 0o750))
	mustRun(t, os.Chmod(etcOpsd, 0o750))

	spec := specFixture(t, "no-file-secret")
	spec.Exec.SecretEnvironment = map[string]domain.SecretRef{
		"APP_TOKEN": {Kind: domain.SecretKindEnv, Name: "APP_TOKEN"},
	}
	mustRun(t, h.adapter.Prepare(context.Background(), spec, ""))

	info, err := os.Stat(etcOpsd)
	mustRun(t, err)
	if got := info.Mode().Perm(); got != 0o750 {
		t.Fatalf("没有 kind=file 凭据时不得改 /etc/opsd 的模式，got %04o", got)
	}
}

func assertTreeEmpty(t *testing.T, root string) {
	t.Helper()
	entries := 0
	err := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path != root {
			entries++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s 失败: %v", root, err)
	}
	if entries != 0 {
		t.Fatalf("不该留下任何文件，实际有 %d 项", entries)
	}
}

func assertFile(t *testing.T, path string, mode os.FileMode, content string, uid, gid int) {
	t.Helper()
	assertModeAndOwner(t, path, mode, uid, gid)
	if got := readFile(t, path); got != content {
		t.Fatalf("%s 内容不符:\n--- got ---\n%q\n--- want ---\n%q", path, got, content)
	}
}

func assertModeAndOwner(t *testing.T, path string, mode os.FileMode, uid, gid int) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s 失败: %v", path, err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("%s 权限 = %o, want %o", path, info.Mode().Perm(), mode)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s 无法读取属主信息", path)
	}
	if int(stat.Uid) != uid || int(stat.Gid) != gid {
		t.Fatalf("%s 属主 = %d:%d, want %d:%d", path, stat.Uid, stat.Gid, uid, gid)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(raw)
}

// TestEnvironmentFilesAreSorted 断言环境文件的键按字典序写出：
// 内容随 map 迭代顺序抖动会让每次 Prepare 都重写文件。
func TestEnvironmentFilesAreSorted(t *testing.T) {
	h := newHarness(t)
	spec := specFixture(t, "sorted")
	spec.Exec.Environment = map[string]string{"ZED": "1", "ALPHA": "2", "MID": "3"}
	mustRun(t, h.adapter.Prepare(context.Background(), spec, ""))

	lines := strings.Split(strings.TrimSpace(readFile(t, h.adapter.RootPath(domain.EnvFilePath("sorted")))), "\n")
	names := make([]string, 0, len(lines))
	for _, line := range lines {
		names = append(names, strings.SplitN(line, "=", 2)[0])
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("环境文件的键必须有序，got %v", names)
	}
}

// Prepare 不得把 releases 子树**内部**的目录建出来（迭代 3c）。
//
// 这条测试的存在理由是一个真实的缺陷：`exec.workingDirectory` 的推荐写法就是 release 的
// `current`（那样 `java -jar app.jar` 才能按工作目录解析到制品里的 JAR），而部署流程是
// **先 Prepare、后物化/切换**。Prepare 若「顺手」把 `current` 建成实体目录，紧接着的
// Activate 就会失败——`rename(current.tmp, current)` 的目标是个目录，报出来的是
// 「改名失败」，与真实原因毫无关系。
func TestPrepareDoesNotCreateReleaseTreeInternals(t *testing.T) {
	h := newHarness(t)
	spec := specFixture(t, "releasepath")
	spec.Exec.WorkingDirectory = domain.CurrentReleaseDir(spec.Application)

	if err := h.adapter.Prepare(context.Background(), spec, ""); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	releasesRoot := filepath.Join(h.root, domain.ReleaseRootDir(spec.Application))
	if info, err := os.Stat(releasesRoot); err != nil || !info.IsDir() {
		t.Fatalf("releases 根必须被建出来（unit 的 ReadWritePaths= 指着它）: %v", err)
	}
	currentLink := filepath.Join(h.root, domain.CurrentReleaseDir(spec.Application))
	if _, err := os.Lstat(currentLink); !os.IsNotExist(err) {
		t.Fatalf("Prepare 不该建出 %s：那是 Activate 的产物，建成实体目录会让切换失败", currentLink)
	}

	// 真正的判据不是「目录没被建出来」，而是**切换能成功**——上面那条只是它的前提。
	// 用真实的 ReleaseAdapter 切换一次：这正是部署流程里紧接着 Prepare 的那一步。
	releases := local.New(local.WithRoot(h.root))
	releaseDir := filepath.Join(h.root, domain.ReleaseDir(spec.Application, "rel_1"))
	if err := os.MkdirAll(releaseDir, 0o750); err != nil {
		t.Fatalf("建 release 目录: %v", err)
	}
	if err := releases.Activate(context.Background(), spec, "", "rel_1"); err != nil {
		t.Fatalf("Prepare 之后 Activate 必须成功: %v", err)
	}
	if target, err := os.Readlink(currentLink); err != nil || target != "rel_1" {
		t.Fatalf("current 应当指向 rel_1: target=%q err=%v", target, err)
	}
}

// java 的解释器预检（迭代 3c）：它的错误必须**在部署之前**、且指向那个路径；
// 而解释器从盘上消失之后，stop 仍然必须能停下来。
func TestJavaInterpreterPreflightAndStopAfterRemoval(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	missing := filepath.Join(t.TempDir(), "jdk-17.0.1", "bin", "java")
	spec := specFixture(t, "javaapp")
	spec.Runtime = domain.RuntimeKindJava
	spec.Exec.Argv = []string{missing, "-Xmx512m", "-jar", "app.jar"}

	err := h.adapter.Validate(ctx, spec)
	if domain.CodeOf(err) != v1.CodeManifestInvalid {
		t.Fatalf("解释器不存在时 want MANIFEST_INVALID, got %v", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("报错必须带上路径，否则运维不知道该改什么: %v", err)
	}

	// 解释器就位之后，同一个适配器必须放行——否则这条检查只是「什么都拦」。
	interpreter := filepath.Join(t.TempDir(), "java")
	if err := os.WriteFile(interpreter, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("写夹具: %v", err)
	}
	spec.Exec.Argv[0] = interpreter
	if err := h.adapter.Validate(ctx, spec); err != nil {
		t.Fatalf("解释器到位时应当通过: %v", err)
	}
	if err := h.adapter.Prepare(ctx, spec, ""); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// 现在把解释器删掉（JDK 被卸载、路径被换掉的现实版本）。
	if err := os.Remove(interpreter); err != nil {
		t.Fatalf("删除解释器夹具: %v", err)
	}
	if err := h.adapter.Validate(ctx, spec); domain.CodeOf(err) != v1.CodeManifestInvalid {
		t.Fatalf("解释器消失后 Validate 应当报错, got %v", err)
	}
	// stop 走的是不碰 argv[0] 的那一层：**这正是最需要这个工具的时刻**，
	// 它不能因为解释器不在就拒绝停下来。
	if err := h.adapter.Stop(ctx, spec, ""); err != nil {
		t.Fatalf("解释器消失后 Stop 仍必须成功: %v", err)
	}
}

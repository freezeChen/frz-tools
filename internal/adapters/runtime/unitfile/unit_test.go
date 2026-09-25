package unitfile

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// validSpec 是与 manifest/domain 约束一致的规格；渲染测试必须从「合法输入」出发，
// 否则断言的是错误路径而不是模板。
func validSpec() *domain.ApplicationSpec {
	return &domain.ApplicationSpec{
		APIVersion:  "v1",
		Kind:        "ApplicationSpec",
		Application: "orders-api",
		Runtime:     domain.RuntimeKindGo,
		Artifact: domain.SpecArtifact{
			Digest: "sha256:" + strings.Repeat("a", 64),
			Unpack: domain.SpecUnpack{Strategy: domain.UnpackTarGz},
		},
		Exec: domain.SpecExec{
			Argv:             []string{"/opt/opsd/apps/orders-api/bin/start", "--config", "/opt/opsd/apps/orders-api/config.yaml"},
			WorkingDirectory: "/opt/opsd/apps/orders-api/current",
			RunUser:          "orders",
			Environment:      map[string]string{"LOG_LEVEL": "info"},
			Ports:            []int{8080},
		},
		Health: domain.SpecHealth{
			Readiness: domain.SpecReadiness{
				Type: domain.ReadinessTCP, Target: "127.0.0.1:8080", ConsecutiveSuccesses: 1,
			},
			StartTimeout: 60 * time.Second,
			StopTimeout:  30 * time.Second,
		},
		Logs:    domain.SpecLogs{Directory: "/var/log/opsd/orders-api"},
		Systemd: domain.SpecSystemd{UnitName: "orders-api.service", RestartPolicy: domain.RestartOnFailure},
	}
}

// strictUnit 是规格 §6 模板在 ≥240 主机上的期望产物，逐字比对。
const strictUnit = `# 本文件由 opsd 生成，请勿手工编辑；下次 Prepare 会按规格 1c 第 6 节重写。
# systemd 版本=255 档位=strict

[Unit]
Description=orders-api
After=network.target
Wants=network.target

[Service]
Type=simple
User=orders
Group=orders
WorkingDirectory=/opt/opsd/apps/orders-api/current
EnvironmentFile=-/etc/opsd/apps/orders-api.env
EnvironmentFile=/etc/opsd/apps/orders-api.secrets.env
ExecStart=/opt/opsd/apps/orders-api/bin/start --config /opt/opsd/apps/orders-api/config.yaml
Restart=on-failure
RestartSec=5
TimeoutStartSec=60
TimeoutStopSec=30
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/opt/opsd/apps/orders-api/current /var/log/opsd/orders-api /opt/opsd/apps/orders-api/releases
StandardOutput=append:/var/log/opsd/orders-api/current.log
StandardError=append:/var/log/opsd/orders-api/current.log

[Install]
WantedBy=multi-user.target
`

// legacyUnit 是同一份规格在 219～239 主机上的期望产物。
// 2026-09-24 起它在真实的 CentOS 7 / systemd 219 上验证过（见 unit.go 的 tierLegacyNote），
// 期望值同时来自规格 §6 的兼容矩阵与那台主机上的实测。
const legacyUnit = `# 本文件由 opsd 生成，请勿手工编辑；下次 Prepare 会按规格 1c 第 6 节重写。
# systemd 版本=239 档位=legacy
# 档位 legacy（systemd 219～239）：本机无法使用 ProtectSystem=strict、
# ReadWritePaths= 与 StandardOutput=append:，因此降级为 ProtectSystem=yes、
# ReadWriteDirectories= 与 journal。语义损失：日志不再追加到 <logs.directory>/current.log，
# 按 Operation 查日志需要走 journalctl（opsd 当前按文件读日志）；
# ProtectSystem=yes 只保护 /usr，未声明的 /var 路径仍然可写；
# 内存上限用 MemoryLimit= 表达（它与 MemoryMax= 同义，但后者 231 才出现、
# 在 219 上写出去等于没有效果）。
# legacy 档已在真实的 systemd 219 上验证；232～239 那一段仍未验证。

[Unit]
Description=orders-api
After=network.target
Wants=network.target

[Service]
Type=simple
User=orders
Group=orders
WorkingDirectory=/opt/opsd/apps/orders-api/current
EnvironmentFile=-/etc/opsd/apps/orders-api.env
EnvironmentFile=/etc/opsd/apps/orders-api.secrets.env
ExecStart=/opt/opsd/apps/orders-api/bin/start --config /opt/opsd/apps/orders-api/config.yaml
Restart=on-failure
RestartSec=5
TimeoutStartSec=60
TimeoutStopSec=30
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=yes
ProtectHome=true
ReadWriteDirectories=/opt/opsd/apps/orders-api/current /var/log/opsd/orders-api /opt/opsd/apps/orders-api/releases
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`

// directiveLines 去掉注释与空行，只留 systemd 真正解析的指令行。
// 注释里会引用被禁用的指令名（例如「本机无法使用 ProtectSystem=strict」），
// 断言「不得出现某指令」时必须只看指令行，否则断言会退化成文字游戏。
func directiveLines(content string) string {
	var kept []string
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func TestRenderUnitStrictTier(t *testing.T) {
	spec := validSpec()
	got, err := RenderUnit(spec, "", TierStrict, 255)
	if err != nil {
		t.Fatalf("RenderUnit(strict) 报错: %v", err)
	}
	if got.Content != strictUnit {
		t.Fatalf("strict 渲染结果不符规格 §6 模板:\n--- got ---\n%s\n--- want ---\n%s", got.Content, strictUnit)
	}
	if got.Tier != TierStrict || got.SystemdVersion != 255 {
		t.Fatalf("审计信息不对: tier=%q version=%d", got.Tier, got.SystemdVersion)
	}
	if len(got.Degradations) != 0 {
		t.Fatalf("strict 档不应有语义损失: %v", got.Degradations)
	}
	if strings.Contains(directiveLines(got.Content), "ReadWriteDirectories=") {
		t.Fatal("strict 档不得出现 ReadWriteDirectories=")
	}
	if !strings.Contains(directiveLines(got.Content), "StandardOutput=append:") {
		t.Fatal("strict 档必须用 append: 追加日志")
	}
}

func TestRenderUnitLegacyTier(t *testing.T) {
	spec := validSpec()
	got, err := RenderUnit(spec, "", TierLegacy, 239)
	if err != nil {
		t.Fatalf("RenderUnit(legacy) 报错: %v", err)
	}
	if got.Content != legacyUnit {
		t.Fatalf("legacy 渲染结果不符规格 §6 兼容矩阵:\n--- got ---\n%s\n--- want ---\n%s", got.Content, legacyUnit)
	}
	// 这些取值在 219 上要么不被接受（unit 加载失败）、要么被忽略，指令行里一个都不能出现。
	body := directiveLines(got.Content)
	for _, forbidden := range []string{"ProtectSystem=strict", "ReadWritePaths=", "StandardOutput=append:", "StandardError=append:"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("legacy 档不得出现 %q:\n%s", forbidden, got.Content)
		}
	}
	if !strings.Contains(body, "ProtectSystem=yes\n") {
		t.Fatal("legacy 档必须降级为 ProtectSystem=yes")
	}
	if !strings.Contains(body, "ReadWriteDirectories=") {
		t.Fatal("legacy 档必须退回 ReadWriteDirectories=")
	}
	if !strings.Contains(body, "StandardOutput=journal\n") || !strings.Contains(body, "StandardError=journal\n") {
		t.Fatal("legacy 档日志必须改投 journal")
	}
	// 语义损失必须留在产物里：运维看到 unit 就知道日志去哪了。
	if !strings.Contains(got.Content, "journalctl") {
		t.Fatal("legacy 档必须在注释里写明日志语义损失")
	}
	if len(got.Degradations) == 0 {
		t.Fatal("legacy 档必须把语义损失交给审计")
	}
}

func TestRenderUnitHeaderRecordsProbedVersionAndTier(t *testing.T) {
	for _, tc := range []struct {
		version  int
		tier     Tier
		wantLine string
	}{
		{version: 255, tier: TierStrict, wantLine: "# systemd 版本=255 档位=strict\n"},
		{version: 240, tier: TierStrict, wantLine: "# systemd 版本=240 档位=strict\n"},
		{version: 239, tier: TierLegacy, wantLine: "# systemd 版本=239 档位=legacy\n"},
		{version: 219, tier: TierLegacy, wantLine: "# systemd 版本=219 档位=legacy\n"},
	} {
		got, err := RenderUnit(validSpec(), "", tc.tier, tc.version)
		if err != nil {
			t.Fatalf("RenderUnit(%s, %d) 报错: %v", tc.tier, tc.version, err)
		}
		if !strings.Contains(got.Content, tc.wantLine) {
			t.Fatalf("头部注释缺少版本与档位 %q:\n%s", tc.wantLine, got.Content)
		}
		if !strings.HasPrefix(got.Content, "# ") {
			t.Fatalf("unit 必须以注释开头:\n%s", got.Content)
		}
	}
}

// ExecStart 是转义出错就会「服务起不来或参数错乱」的地方，这里把空白、双引号、
// 反斜杠、单引号四种边界放进同一个 argv 一起断言。
func TestRenderUnitEscapesExecStartArguments(t *testing.T) {
	spec := validSpec()
	spec.Exec.Argv = []string{
		"/opt/opsd/apps/orders-api/bin/start",
		"--name", "orders api",
		`--label=他说"你好"`,
		`--path=C:\Program Files\opsd`,
		"--mode='prod'",
	}

	got, err := RenderUnit(spec, "", TierStrict, 255)
	if err != nil {
		t.Fatalf("RenderUnit 报错: %v", err)
	}

	want := `/opt/opsd/apps/orders-api/bin/start --name "orders api" "--label=他说\"你好\"" "--path=C:\\Program Files\\opsd" "--mode='prod'"`
	execLine := "ExecStart=" + want + "\n"
	if !strings.Contains(got.Content, execLine) {
		t.Fatalf("ExecStart 转义不符:\n--- got ---\n%s\n--- want 行 ---\n%s", got.Content, execLine)
	}
	// 渲染必须与既有的转义实现逐字节一致：两处各写一份转义就是 bug 温床。
	if EscapeArgs(spec.Exec.Argv) != want {
		t.Fatalf("EscapeArgs 与期望不一致: %q", EscapeArgs(spec.Exec.Argv))
	}
	// 转义后的 ExecStart 只能出现一次：多出来的行等于让参数变成新指令。
	if strings.Count(got.Content, "ExecStart=") != 1 {
		t.Fatalf("ExecStart 行数不为 1:\n%s", got.Content)
	}

	// 空参数在 domain 层被拒绝（exec.argv 不允许空字符串），
	// 因此渲染层不会静默产出 `""`；这个边界由 escape_test.go 的 EscapeArgs 用例钉住。
	spec.Exec.Argv = []string{"/opt/opsd/apps/orders-api/bin/start", ""}
	if _, err := RenderUnit(spec, "", TierStrict, 255); err == nil {
		t.Fatal("含空参数的非法规格必须被拒绝")
	} else if code := domain.CodeOf(err); code != v1.CodeManifestInvalid {
		t.Fatalf("错误码不对: %s", code)
	}
}

// 同一输入必须字节一致：Prepare 是幂等的，unit 内容抖动会导致无意义的 daemon-reload 与重启。
func TestRenderUnitIsDeterministic(t *testing.T) {
	first, err := RenderUnit(validSpec(), "", TierStrict, 255)
	if err != nil {
		t.Fatalf("RenderUnit 报错: %v", err)
	}
	for i := 0; i < 8; i++ {
		again, err := RenderUnit(validSpec(), "", TierLegacy, 239)
		if err != nil {
			t.Fatalf("RenderUnit 报错: %v", err)
		}
		if again.Content != legacyUnit {
			t.Fatalf("第 %d 次渲染不稳定:\n%s", i, again.Content)
		}
		same, err := RenderUnit(validSpec(), "", TierStrict, 255)
		if err != nil {
			t.Fatalf("RenderUnit 报错: %v", err)
		}
		if same.Content != first.Content {
			t.Fatalf("第 %d 次渲染字节不稳定:\n%s", i, same.Content)
		}
	}
}

func TestRenderUnitRejectsTierVersionMismatch(t *testing.T) {
	if _, err := RenderUnit(validSpec(), "", TierLegacy, 255); err == nil {
		t.Fatal("版本 255 却按 legacy 渲染必须被拒绝")
	} else if code := domain.CodeOf(err); code != v1.CodeManifestInvalid {
		t.Fatalf("错误码不对: %s", code)
	}
	if _, err := RenderUnit(validSpec(), "", TierStrict, 219); err == nil {
		t.Fatal("版本 219 却按 strict 渲染必须被拒绝")
	}
	if _, err := RenderUnit(validSpec(), "", Tier("unknown"), 255); err == nil {
		t.Fatal("未知档位必须被拒绝")
	}
	if _, err := RenderUnit(nil, "", TierStrict, 255); err == nil {
		t.Fatal("空规格必须被拒绝")
	}
}

// 路径里的换行会让 unit 多出一条指令（manifest 只校验了「绝对路径」），
// 空白会把 ReadWritePaths= 的列表切开，两者都必须在渲染层被拦住。
func TestRenderUnitRejectsPathsThatBreakUnitSyntax(t *testing.T) {
	cases := map[string]func(*domain.ApplicationSpec){
		"工作目录含换行": func(s *domain.ApplicationSpec) {
			s.Exec.WorkingDirectory = "/opt/opsd/apps/orders-api/current\nUser=root"
		},
		"工作目录含空白": func(s *domain.ApplicationSpec) {
			s.Exec.WorkingDirectory = "/opt/opsd/apps/orders api"
		},
		"日志目录含双引号": func(s *domain.ApplicationSpec) {
			s.Logs.Directory = `/var/log/opsd/"orders-api"`
		},
		"日志目录含反斜杠": func(s *domain.ApplicationSpec) {
			s.Logs.Directory = `/var/log/opsd/orders\api`
		},
	}
	for name, mutate := range cases {
		spec := validSpec()
		mutate(spec)
		_, err := RenderUnit(spec, "", TierStrict, 255)
		if err == nil {
			t.Fatalf("%s: 必须被拒绝", name)
		}
		if code := domain.CodeOf(err); code != v1.CodeManifestInvalid {
			t.Fatalf("%s: 错误码不对: %s", name, code)
		}
	}
}

func TestRenderUnitRejectsSubSecondTimeouts(t *testing.T) {
	spec := validSpec()
	spec.Health.StartTimeout = 1500 * time.Millisecond
	if _, err := RenderUnit(spec, "", TierStrict, 255); err == nil {
		t.Fatal("亚秒超时必须报错而不是截断")
	}
}

func TestTierForBoundaries(t *testing.T) {
	cases := []struct {
		version int
		want    Tier
	}{
		{version: 219, want: TierLegacy},
		{version: 231, want: TierLegacy},
		{version: 232, want: TierLegacy},
		{version: 239, want: TierLegacy},
		{version: 240, want: TierStrict},
		{version: 255, want: TierStrict},
	}
	for _, tc := range cases {
		got, err := TierFor(tc.version)
		if err != nil {
			t.Fatalf("TierFor(%d) 报错: %v", tc.version, err)
		}
		if got != tc.want {
			t.Fatalf("TierFor(%d) = %q, want %q", tc.version, got, tc.want)
		}
	}

	for _, version := range []int{0, -1, 1, 218} {
		got, err := TierFor(version)
		if err == nil {
			t.Fatalf("TierFor(%d) 必须报错，却返回 %q", version, got)
		}
		if code := domain.CodeOf(err); code != v1.CodeRuntimeUnsupport {
			t.Fatalf("TierFor(%d) 错误码 = %s, want RUNTIME_UNSUPPORTED", version, code)
		}
		if got != "" {
			t.Fatalf("TierFor(%d) 报错时不得返回档位: %q", version, got)
		}
	}
}

func TestTierValid(t *testing.T) {
	for _, tier := range []Tier{TierStrict, TierLegacy} {
		if !tier.Valid() {
			t.Fatalf("%q 应当是合法档位", tier)
		}
	}
	for _, tier := range []Tier{"", "modern", "Strict"} {
		if tier.Valid() {
			t.Fatalf("%q 不应当是合法档位", tier)
		}
	}
}

func TestRenderForVersionPicksTierFromVersion(t *testing.T) {
	strict, err := RenderForVersion(validSpec(), "", 255)
	if err != nil {
		t.Fatalf("RenderForVersion(255) 报错: %v", err)
	}
	if strict.Content != strictUnit {
		t.Fatalf("255 应走 strict 档:\n%s", strict.Content)
	}
	legacy, err := RenderForVersion(validSpec(), "", 219)
	if err != nil {
		t.Fatalf("RenderForVersion(219) 报错: %v", err)
	}
	if !strings.Contains(legacy.Content, "ProtectSystem=yes\n") {
		t.Fatalf("219 应走 legacy 档:\n%s", legacy.Content)
	}
	if _, err := RenderForVersion(validSpec(), "", 218); err == nil {
		t.Fatal("218 必须报错")
	}
}

// ==== 资源限制（迭代 3c）====

// 断言的是**字节级**的形式：MemoryMax= 写原始字节数，不写 `512M` 这种后缀——
// 后缀要过一道单位换算，而换算写错就是一个「差一点点生效」的限制。
func TestRenderUnitResources(t *testing.T) {
	spec := validSpec()
	spec.Resources = domain.SpecResources{CPUQuotaPercent: 200, MemoryMaxBytes: 536870912}

	got, err := RenderUnit(spec, "", TierStrict, 255)
	if err != nil {
		t.Fatalf("RenderUnit: %v", err)
	}
	body := directiveLines(got.Content)
	for _, want := range []string{"CPUQuota=200%\n", "MemoryMax=536870912\n"} {
		if !strings.Contains(body, want) {
			t.Fatalf("渲染结果缺少 %q:\n%s", want, got.Content)
		}
	}
	// 声明了限制的 unit 必须真的能被 systemd 读到：两条指令都在 [Service] 段里。
	service := got.Content[strings.Index(got.Content, "[Service]"):strings.Index(got.Content, "[Install]")]
	for _, want := range []string{"CPUQuota=200%\n", "MemoryMax=536870912\n"} {
		if !strings.Contains(service, want) {
			t.Fatalf("%q 不在 [Service] 段里:\n%s", want, got.Content)
		}
	}
}

// 0 = 不限制：**那一行不写**，而不是写 `CPUQuota=0`。`0` 会被 systemd 解释成什么
// 我们没有在真机上验证过，而「不写」的语义是确定的。
func TestRenderUnitOmitsResourcesWhenUnlimited(t *testing.T) {
	spec := validSpec() // Resources 全 0
	got, err := RenderUnit(spec, "", TierStrict, 255)
	if err != nil {
		t.Fatalf("RenderUnit: %v", err)
	}
	if strings.Contains(directiveLines(got.Content), "CPUQuota=") ||
		strings.Contains(directiveLines(got.Content), "MemoryMax=") {
		t.Fatalf("不限制时不该写出资源指令:\n%s", got.Content)
	}
}

// legacy 档（219～239）用**那一档的拼法**表达内存上限：`MemoryLimit=` 而不是 `MemoryMax=`。
//
// 这条是 2026-09-24 在真实的 systemd 219 上实测之后改的（原来是「这一档直接拒绝」）。
// 实测结果是：同一份 unit 里 `MemoryMax=536870912` **毫无效果**（unit 照常加载并启动、
// `systemctl show` 里没有它、cgroup 里也没有它），而 `MemoryLimit=` 落到了 cgroup 的
// `memory.limit_in_bytes`。也就是说这一档表达得了内存上限——用不着让用户为了「机器老」
// 而放弃这条限制，只是指令名随档位不同。
func TestRenderUnitLegacyUsesMemoryLimit(t *testing.T) {
	spec := validSpec()
	spec.Resources = domain.SpecResources{CPUQuotaPercent: 200, MemoryMaxBytes: 536870912}

	got, err := RenderUnit(spec, "", TierLegacy, 219)
	if err != nil {
		t.Fatalf("legacy 档表达内存上限不应当报错: %v", err)
	}
	body := directiveLines(got.Content)
	// 值仍然是原始字节数（与 strict 档同一条理由：不做单位换算）。
	if !strings.Contains(body, "MemoryLimit=536870912\n") {
		t.Fatalf("legacy 档应当渲染 MemoryLimit=:\n%s", got.Content)
	}
	// 231 才有的那个拼法在这一档上会出现**静默失效**，绝不能一起写出去。
	if strings.Contains(body, "MemoryMax=") {
		t.Fatalf("legacy 档不得出现 MemoryMax=（219 上它没有效果）:\n%s", got.Content)
	}
	// CPUQuota= 需要 213，两档都满足（219 上实测生效）。
	if !strings.Contains(body, "CPUQuota=200%\n") {
		t.Fatalf("legacy 档应当渲染 CPUQuota=:\n%s", got.Content)
	}
	// strict 档用现代的拼法，不受影响。
	strict, err := RenderUnit(spec, "", TierStrict, 255)
	if err != nil {
		t.Fatalf("strict 档: %v", err)
	}
	if !strings.Contains(directiveLines(strict.Content), "MemoryMax=536870912\n") ||
		strings.Contains(directiveLines(strict.Content), "MemoryLimit=") {
		t.Fatalf("strict 档应当用 MemoryMax=:\n%s", strict.Content)
	}
	// 指令名随档位变化这件事必须能在审计里看到。
	noted := false
	for _, degradation := range TierLegacy.Degradations() {
		if strings.Contains(degradation, "MemoryLimit=") {
			noted = true
		}
	}
	if !noted {
		t.Fatal("legacy 档的语义损失里应当写明内存上限用 MemoryLimit= 表达")
	}
}

package manifest

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/runtime/unitfile"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// 与 1c 规格第 4 节的示例一致，只把 secretEnvironment 换成已收敛的语义。
const validManifest = `apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: billing-api
runtime: go
artifact:
  id: art_XXXX
  unpack:
    strategy: tar
    stripComponents: 1
exec:
  argv: [/opt/billing-api/bin/billing-api]
  workingDirectory: /var/lib/billing-api
  runUser: billing-api
  environment:
    GOMEMLIMIT: 40MiB
  secretEnvironment:
    DB_PASSWORD:
      kind: env
      name: billing_db_password
    TLS_KEY:
      kind: file
      name: /etc/opsd/secrets/billing-tls.key
  ports: [8080]
health:
  readiness:
    type: tcp
    target: "127.0.0.1:8080"
    consecutiveSuccesses: 2
  startTimeoutSeconds: 60
  stopTimeoutSeconds: 30
logs:
  directory: /var/log/billing-api
systemd:
  unitName: billing-api.service
  restartPolicy: on-failure
`

func parse(t *testing.T, text string) (*domain.ApplicationSpec, error) {
	t.Helper()
	return Parse([]byte(text))
}

func mustParse(t *testing.T, text string) *domain.ApplicationSpec {
	t.Helper()
	spec, err := parse(t, text)
	if err != nil {
		t.Fatalf("应当解析成功，却失败: %v", err)
	}
	return spec
}

func TestParseValidManifest(t *testing.T) {
	spec := mustParse(t, validManifest)

	if spec.Application != "billing-api" || spec.Runtime != domain.RuntimeKindGo {
		t.Fatalf("unexpected headline fields: %+v", spec)
	}
	if spec.Artifact.ID != "art_XXXX" || spec.Artifact.Unpack.Strategy != domain.UnpackTar {
		t.Fatalf("unexpected artifact: %+v", spec.Artifact)
	}
	if !spec.Artifact.Unpack.StrategyExplicit {
		t.Fatal("manifest 写了 strategy，StrategyExplicit 应为 true")
	}
	if spec.Artifact.Unpack.StripComponents != 1 {
		t.Fatalf("stripComponents: %+v", spec.Artifact.Unpack)
	}
	if len(spec.Exec.Argv) != 1 || spec.Exec.RunUser != "billing-api" {
		t.Fatalf("unexpected exec: %+v", spec.Exec)
	}
	if got := spec.Exec.SecretEnvironment["DB_PASSWORD"]; got.Kind != domain.SecretKindEnv || got.Name != "billing_db_password" {
		t.Fatalf("unexpected secret ref: %+v", got)
	}
	if got := spec.Exec.SecretEnvironment["TLS_KEY"]; got.Kind != domain.SecretKindFile {
		t.Fatalf("unexpected secret ref: %+v", got)
	}
	if spec.Health.StartTimeout != 60*time.Second || spec.Health.StopTimeout != 30*time.Second {
		t.Fatalf("unexpected health timeouts: %+v", spec.Health)
	}
	if spec.Health.Readiness.ConsecutiveSuccesses != 2 || spec.Health.Readiness.Type != domain.ReadinessTCP {
		t.Fatalf("unexpected readiness: %+v", spec.Health.Readiness)
	}
	if spec.Systemd.UnitName != "billing-api.service" || spec.Systemd.RestartPolicy != domain.RestartOnFailure {
		t.Fatalf("unexpected systemd: %+v", spec.Systemd)
	}
}

// 未声明的可选字段必须被填上默认值，而不是留空让下游各自猜。
func TestParseAppliesDefaults(t *testing.T) {
	text := `apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: minimal
runtime: java
artifact:
  digest: sha256:0000000000000000000000000000000000000000000000000000000000000001
exec:
  argv: [/opt/minimal/bin/run]
  workingDirectory: /var/lib/minimal
  runUser: minimal
health:
  readiness:
    type: tcp
    target: "127.0.0.1:9000"
logs:
  directory: /var/log/minimal
`
	spec := mustParse(t, text)

	if spec.Artifact.Unpack.Strategy != domain.UnpackNone {
		t.Fatalf("未声明 unpack 时应为 none，got %q", spec.Artifact.Unpack.Strategy)
	}
	if spec.Artifact.Unpack.StrategyExplicit {
		t.Fatal("未写 strategy 时 StrategyExplicit 必须为 false")
	}
	if spec.Systemd.UnitName != "minimal.service" {
		t.Fatalf("unitName 应由应用名派生，got %q", spec.Systemd.UnitName)
	}
	if spec.Systemd.RestartPolicy != domain.RestartOnFailure {
		t.Fatalf("restartPolicy 默认应为 on-failure，got %q", spec.Systemd.RestartPolicy)
	}
	if spec.Health.StartTimeout != domain.DefaultStartTimeout || spec.Health.StopTimeout != domain.DefaultStopTimeout {
		t.Fatalf("健康检查超时应有默认值: %+v", spec.Health)
	}
	if spec.Health.Readiness.ConsecutiveSuccesses != 1 {
		t.Fatalf("consecutiveSuccesses 默认应为 1，got %d", spec.Health.Readiness.ConsecutiveSuccesses)
	}
}

// 相对 argv 是迭代 3 新增的用法（规格 D4）：解析到 release 的 current 之下。
// 它**不是**非法形态——把 1c 起就允许的绝对 argv 收紧成唯一形态属于破坏性变更。
func TestParseAcceptsRelativeArgv(t *testing.T) {
	manifest := strings.Replace(validManifest,
		"  argv: [/opt/billing-api/bin/billing-api]",
		"  argv: [bin/billing-api, --config, /etc/billing/config.yaml]", 1)
	spec, err := parse(t, manifest)
	if err != nil {
		t.Fatalf("相对 argv 应当被接受: %v", err)
	}
	if spec.Exec.Argv[0] != "bin/billing-api" {
		t.Fatalf("argv 应当原样保留（解析发生在渲染 unit 时）: %v", spec.Exec.Argv)
	}
}

func TestParseRejectsInvalidManifest(t *testing.T) {
	// 用 base 做定点替换，避免为每种错误重复整份 manifest。
	base := validManifest
	cases := []struct {
		name   string
		mutate func(string) string
	}{
		{"缺少 apiVersion", func(s string) string {
			return strings.Replace(s, "apiVersion: ops.frz.io/v1alpha1\n", "", 1)
		}},
		{"apiVersion 不受支持", func(s string) string {
			return strings.Replace(s, "ops.frz.io/v1alpha1", "ops.frz.io/v9", 1)
		}},
		{"kind 错误", func(s string) string {
			return strings.Replace(s, "kind: ApplicationSpec", "kind: SomethingElse", 1)
		}},
		{"未知字段（严格解码）", func(s string) string {
			return s + "unexpectedTopLevelField: 1\n"
		}},
		{"嵌套未知字段", func(s string) string {
			return strings.Replace(s, "  runUser: billing-api", "  runUser: billing-api\n  typo: 1", 1)
		}},
		{"application 含路径穿越", func(s string) string {
			return strings.Replace(s, "application: billing-api", "application: ../etc/passwd", 1)
		}},
		{"application 含斜杠", func(s string) string {
			return strings.Replace(s, "application: billing-api", "application: a/b", 1)
		}},
		{"runtime 非法", func(s string) string {
			return strings.Replace(s, "runtime: go", "runtime: rust", 1)
		}},
		{"artifact 同时给 id 与 digest", func(s string) string {
			// 用**合法**摘要，好让这条用例只钉「id 与 digest 不能同时给」这一件事；
			// 摘要格式本身由另一条用例覆盖。
			return strings.Replace(s, "  id: art_XXXX",
				"  id: art_XXXX\n  digest: sha256:"+strings.Repeat("a", 64), 1)
		}},
		{"artifact 两者都不给", func(s string) string {
			return strings.Replace(s, "  id: art_XXXX\n", "", 1)
		}},
		{"digest 前缀不支持", func(s string) string {
			return strings.Replace(s, "  id: art_XXXX", "  digest: md5:abc", 1)
		}},
		{"unpack.strategy 非法", func(s string) string {
			return strings.Replace(s, "    strategy: tar", "    strategy: rar", 1)
		}},
		{"strategy=none 却给了 stripComponents", func(s string) string {
			return strings.Replace(s, "    strategy: tar", "    strategy: none", 1)
		}},
		{"stripComponents 为负", func(s string) string {
			return strings.Replace(s, "    stripComponents: 1", "    stripComponents: -1", 1)
		}},
		{"argv 为空", func(s string) string {
			return strings.Replace(s, "  argv: [/opt/billing-api/bin/billing-api]", "  argv: []", 1)
		}},
		// 相对 argv 现在**合法**了（迭代 3 规格 D4：相对路径解析到 release 的 current
		// 之下），但相对元素里出现 .. 就是逃出 release 目录的直接手段——那一条必须被拒。
		{"argv 的相对元素含 ..", func(s string) string {
			return strings.Replace(s, "/opt/billing-api/bin/billing-api]", "bin/../../etc/shadow]", 1)
		}},
		{"workingDirectory 是相对路径", func(s string) string {
			return strings.Replace(s, "  workingDirectory: /var/lib/billing-api", "  workingDirectory: var/lib/x", 1)
		}},
		{"logs.directory 是相对路径", func(s string) string {
			return strings.Replace(s, "  directory: /var/log/billing-api", "  directory: var/log/x", 1)
		}},
		{"runUser 含大写", func(s string) string {
			return strings.Replace(s, "  runUser: billing-api", "  runUser: Billing", 1)
		}},
		{"runUser 含注入字符", func(s string) string {
			return strings.Replace(s, "  runUser: billing-api", "  runUser: \"a;rm -rf /\"", 1)
		}},
		{"端口越界", func(s string) string {
			return strings.Replace(s, "  ports: [8080]", "  ports: [70000]", 1)
		}},
		{"环境变量名非法", func(s string) string {
			return strings.Replace(s, "    GOMEMLIMIT: 40MiB", "    \"1BAD\": x", 1)
		}},
		{"凭据引用非法 kind", func(s string) string {
			return strings.Replace(s, "      kind: env", "      kind: vault", 1)
		}},
		{"凭据引用缺 name", func(s string) string {
			return strings.Replace(s, "      name: billing_db_password", "      name: \"\"", 1)
		}},
		{"readiness.type 非法", func(s string) string {
			return strings.Replace(s, "    type: tcp", "    type: grpc", 1)
		}},
		{"readiness 缺 type", func(s string) string {
			return strings.Replace(s, "    type: tcp\n", "", 1)
		}},
		{"tcp target 不是 host:port", func(s string) string {
			return strings.Replace(s, "    target: \"127.0.0.1:8080\"", "    target: \"127.0.0.1\"", 1)
		}},
		{"http target 不是完整 URL", func(s string) string {
			return strings.Replace(s, "    type: tcp", "    type: http", 1)
		}},
		{"unitName 不是 .service", func(s string) string {
			return strings.Replace(s, "  unitName: billing-api.service", "  unitName: billing-api", 1)
		}},
		{"unitName 含路径分隔符", func(s string) string {
			return strings.Replace(s, "  unitName: billing-api.service", "  unitName: a/b.service", 1)
		}},
		{"restartPolicy 非法", func(s string) string {
			return strings.Replace(s, "  restartPolicy: on-failure", "  restartPolicy: sometimes", 1)
		}},
		{"空内容", func(string) string { return "   \n" }},
		{"非 YAML", func(string) string { return "{{{" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.mutate(base))
			if err == nil {
				t.Fatal("应当被拒绝，却解析成功了")
			}
			if code := domain.CodeOf(err); code != v1.CodeManifestInvalid {
				t.Fatalf("want MANIFEST_INVALID, got %v (%v)", code, err)
			}
		})
	}
}

// 环境变量与凭据撞键是语义冲突，不是格式错误，错误码必须区分开。
func TestEnvironmentSecretClashIsConflict(t *testing.T) {
	text := strings.Replace(validManifest, "    GOMEMLIMIT: 40MiB", "    DB_PASSWORD: x", 1)
	_, err := parse(t, text)
	if code := domain.CodeOf(err); code != v1.CodeManifestConflict {
		t.Fatalf("want MANIFEST_CONFLICT, got %v (%v)", code, err)
	}
}

func TestParseRejectsUnknownReadinessExecType(t *testing.T) {
	// 1c 的规格提到过 exec，但从未定义它的 target 与参数语义。
	// 与其凭空实现一套未冻结的语义，不如明确不支持。
	text := strings.Replace(validManifest, "    type: tcp", "    type: exec", 1)
	_, err := parse(t, text)
	if code := domain.CodeOf(err); code != v1.CodeManifestInvalid {
		t.Fatalf("want MANIFEST_INVALID, got %v (%v)", code, err)
	}
	if !strings.Contains(err.Error(), "readiness.type") {
		t.Fatalf("错误信息应当指向具体字段，got %v", err)
	}
}

func TestHTTPReadinessAcceptsFullURL(t *testing.T) {
	text := strings.Replace(validManifest, "    type: tcp\n    target: \"127.0.0.1:8080\"",
		"    type: http\n    target: \"http://127.0.0.1:8080/healthz\"", 1)
	spec := mustParse(t, text)
	if spec.Health.Readiness.Type != domain.ReadinessHTTP {
		t.Fatalf("unexpected readiness: %+v", spec.Health.Readiness)
	}
}

func TestCheckUnpackMediaType(t *testing.T) {
	t.Run("未显式声明时采 mediaType", func(t *testing.T) {
		text := strings.Replace(validManifest, "  unpack:\n    strategy: tar\n    stripComponents: 1\n", "", 1)
		spec := mustParse(t, text)
		if err := spec.CheckUnpackMediaType("application/zip"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if spec.Artifact.Unpack.Strategy != domain.UnpackZip {
			t.Fatalf("应当采 mediaType 对应的 zip，got %q", spec.Artifact.Unpack.Strategy)
		}
	})

	t.Run("显式声明与 mediaType 冲突", func(t *testing.T) {
		spec := mustParse(t, validManifest) // 显式 tar
		err := spec.CheckUnpackMediaType("application/zip")
		if code := domain.CodeOf(err); code != v1.CodeManifestConflict {
			t.Fatalf("want MANIFEST_CONFLICT, got %v (%v)", code, err)
		}
	})

	t.Run("mediaType 推断不出具体策略时不算冲突", func(t *testing.T) {
		spec := mustParse(t, validManifest) // 显式 tar
		if err := spec.CheckUnpackMediaType("application/octet-stream"); err != nil {
			t.Fatalf("裸二进制的 mediaType 不该判为冲突: %v", err)
		}
		if spec.Artifact.Unpack.Strategy != domain.UnpackTar {
			t.Fatalf("显式声明应当保留，got %q", spec.Artifact.Unpack.Strategy)
		}
	})
}

func TestSecretNameHelpers(t *testing.T) {
	spec := mustParse(t, validManifest)
	if got := spec.SecretEnvNames(); len(got) != 1 || got[0] != "DB_PASSWORD" {
		t.Fatalf("unexpected env secret names: %v", got)
	}
	if got := spec.SecretFileNames(); len(got) != 1 || got[0] != "TLS_KEY" {
		t.Fatalf("unexpected file secret names: %v", got)
	}
}

// 路径约定必须集中一处，否则适配器与 harness 会各自拼出不同路径。
func TestLayoutPaths(t *testing.T) {
	cases := map[string]string{
		domain.EnvFilePath("app"):             "/etc/opsd/apps/app.env",
		domain.SecretsEnvFilePath("app"):      "/etc/opsd/apps/app.secrets.env",
		domain.SecretsDir("app"):              "/etc/opsd/apps/app.secrets",
		domain.ReleaseDir("app", "rel_1"):     "/opt/opsd/apps/app/releases/rel_1",
		domain.UnitPath("app.service"):        "/etc/systemd/system/app.service",
		domain.SecretFileVarValue("app", "k"): "/etc/opsd/apps/app.secrets/k",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("want %q, got %q", want, got)
		}
	}
}

// 迭代 3 新增的字段：artifact.version / fileName、resources、release。
//
// 这条用例的意义在于**钉住 wire 名字**：YAML 里的键名一旦写错，严格解码会拒绝它
// （那是好事），但如果我们自己在文档里写错了名字，用户就会照着错的写。
func TestParseIteration3Fields(t *testing.T) {
	text := `apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: orders-api
runtime: java
artifact:
  id: art_1
  version: 1.4.2-rc1
  unpack:
    strategy: tar-gz
    stripComponents: 1
exec:
  argv: [/opt/jdk-17.0.1/bin/java, -Xmx512m, -jar, app.jar]
  workingDirectory: /var/lib/orders-api
  runUser: orders-api
health:
  readiness:
    type: tcp
    target: "127.0.0.1:8080"
logs:
  directory: /var/log/orders-api
resources:
  cpuQuotaPercent: 250
  memoryMaxBytes: 1073741824
release:
  keepLast: 7
`
	spec := mustParse(t, text)

	if spec.Artifact.Version != "1.4.2-rc1" {
		t.Fatalf("artifact.version 没解析出来: %q", spec.Artifact.Version)
	}
	if spec.Resources.CPUQuotaPercent != 250 {
		t.Fatalf("resources.cpuQuotaPercent 没解析出来: %d", spec.Resources.CPUQuotaPercent)
	}
	if spec.Resources.MemoryMaxBytes != 1073741824 {
		t.Fatalf("resources.memoryMaxBytes 没解析出来: %d", spec.Resources.MemoryMaxBytes)
	}
	if spec.Release.KeepLast != 7 {
		t.Fatalf("release.keepLast 没解析出来: %d", spec.Release.KeepLast)
	}
	if got := spec.Artifact.VersionOrDerived(); got != "1.4.2-rc1" {
		t.Fatalf("写了版本号就该用它，got %q", got)
	}
}

// artifact.fileName 的相容性：它**可选**（缺省取相对的 argv[0]）。
//
// 把它判成必填会破坏 1c 起就合法的 manifest——`unpack` 省略时策略默认就是 none，
// 于是所有既有 manifest 都会提交不了。
func TestParseAllowsMissingFileNameForSingleFile(t *testing.T) {
	// 把 unpack 整段去掉：策略默认 none，argv[0] 又是绝对路径——这就是 1c 的形态，
	// 表示「跑一个住在发布目录之外的既有程序」，物化没有意义。
	withoutUnpack := strings.Replace(validManifest,
		"  unpack:\n    strategy: tar\n    stripComponents: 1\n", "", 1)
	spec, err := parse(t, withoutUnpack)
	if err != nil {
		t.Fatalf("1c 形态的 manifest 必须仍然合法: %v", err)
	}
	if spec.Materializes() {
		t.Fatal("argv[0] 是绝对路径且没给 fileName 时，不该声称需要物化")
	}

	relative := strings.Replace(withoutUnpack,
		"  argv: [/opt/billing-api/bin/billing-api]", "  argv: [bin/billing-api]", 1)
	spec, err = parse(t, relative)
	if err != nil {
		t.Fatalf("相对 argv[0] 的 manifest 必须合法: %v", err)
	}
	if got := spec.MaterializedFileName(); got != "bin/billing-api" {
		t.Fatalf("缺省应当取相对的 argv[0]，got %q", got)
	}
	if !spec.Materializes() {
		t.Fatal("有落点时应当声称需要物化")
	}
}

func TestParseRejectsMismatchedFileName(t *testing.T) {
	tarWithName := strings.Replace(validManifest,
		"    strategy: tar\n", "    fileName: server\n    strategy: tar\n", 1)
	if _, err := parse(t, tarWithName); err == nil {
		t.Fatal("归档制品给了 fileName 必须被拒（它不会生效）")
	}

	singleFile := strings.Replace(validManifest,
		"  unpack:\n    strategy: tar\n    stripComponents: 1\n",
		"  unpack:\n    strategy: none\n  fileName: bin/server\n", 1)
	spec, err := parse(t, singleFile)
	if err != nil {
		t.Fatalf("strategy=none + fileName 应当被接受: %v", err)
	}
	if spec.Artifact.FileName != "bin/server" {
		t.Fatalf("fileName 没解析出来: %q", spec.Artifact.FileName)
	}
}

// 相对 argv[0] 必须被解析成 current 之下的路径——**解析发生在渲染 unit 时**，
// 因此这里断言的是渲染结果，而不是 spec（spec 里保留用户写的样子）。
func TestRenderUnitResolvesRelativeArgv(t *testing.T) {
	text := strings.Replace(validManifest,
		"  argv: [/opt/billing-api/bin/billing-api]",
		"  argv: [bin/billing-api, --config, /etc/billing-api/config.yaml]", 1)
	spec := mustParse(t, text)

	rendered, err := unitfile.RenderUnit(spec, unitfile.TierStrict, 255)
	if err != nil {
		t.Fatalf("RenderUnit: %v", err)
	}
	want := domain.CurrentReleaseDir(spec.Application) + "/bin/billing-api"
	if !strings.Contains(rendered.Content, "ExecStart="+want+" --config /etc/billing-api/config.yaml") {
		t.Fatalf("ExecStart 里应当出现解析后的路径 %q：\n%s", want, rendered.Content)
	}
	if len(rendered.Argv) == 0 || rendered.Argv[0] != want {
		t.Fatalf("渲染结果应当带回解析后的 argv: %v", rendered.Argv)
	}
	// 绝对参数一个字节都不能动。
	if rendered.Argv[2] != "/etc/billing-api/config.yaml" {
		t.Fatalf("绝对参数被改了: %v", rendered.Argv)
	}
}

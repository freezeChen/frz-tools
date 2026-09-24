package proc_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/runtime/proc"
	"github.com/freezeChen/frz-tools/internal/application/runtimecontract"
	"github.com/freezeChen/frz-tools/internal/domain"
)

type stubResolver struct{ values map[string]string }

func (s *stubResolver) Resolve(_ context.Context, ref domain.SecretRef) (string, error) {
	value, ok := s.values[ref.String()]
	if !ok {
		return "", domain.NewError(v1.CodeSecretUnresolved, "no secret %s", ref)
	}
	return value, nil
}

func TestProcAdapterContract(t *testing.T) {
	runtimecontract.Run(t, func(t *testing.T) runtimecontract.Harness {
		root := t.TempDir()
		resolver := &stubResolver{values: map[string]string{
			"env:APP_TOKEN":     `tok\en"v"`,      // 含反斜杠与双引号，顺带压到环境文件转义
			"file:/tmp/app.key": "line1\nline2\n", // 多行，只能走 kind=file
		}}

		return runtimecontract.Harness{
			Adapter: proc.New(root, resolver),
			NewSpec: func(t *testing.T, name string) *domain.ApplicationSpec {
				spec := &domain.ApplicationSpec{
					APIVersion:  domain.ManifestAPIVersion,
					Kind:        domain.ManifestKind,
					Application: name,
					Runtime:     domain.RuntimeKindGo,
					Artifact: domain.SpecArtifact{
						Digest: contractDigest(name),
					},
					Exec: domain.SpecExec{
						Argv:             []string{"/bin/sleep", "300"},
						WorkingDirectory: "/var/lib/" + name,
						RunUser:          "appuser",
						Environment:      map[string]string{"GOMEMLIMIT": "40MiB"},
						SecretEnvironment: map[string]domain.SecretRef{
							"APP_TOKEN": {Kind: domain.SecretKindEnv, Name: "APP_TOKEN"},
							"APP_KEY":   {Kind: domain.SecretKindFile, Name: "/tmp/app.key"},
						},
					},
					Health: domain.SpecHealth{
						Readiness: domain.SpecReadiness{
							Type:                 domain.ReadinessTCP,
							Target:               "127.0.0.1:1", // 由 Serve 覆盖
							ConsecutiveSuccesses: 1,
						},
					},
					Logs: domain.SpecLogs{Directory: "/var/log/" + name},
				}
				if err := spec.Validate(); err != nil {
					t.Fatalf("夹具规格本身必须合法: %v", err)
				}
				return spec
			},
			Serve: func(t *testing.T, spec *domain.ApplicationSpec) func() {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("listen: %v", err)
				}
				spec.Health.Readiness.Target = listener.Addr().String()
				return func() { _ = listener.Close() }
			},
		}
	})
}

// Prepare 的产物必须真的落在沙箱里，并且权限符合约定——这是 macOS 上唯一能
// 提前验证文件模式语义的机会（真实属主语义仍需 Linux 容器）。
func TestPrepareWritesSandboxedFilesWithExpectedModes(t *testing.T) {
	root := t.TempDir()
	resolver := &stubResolver{values: map[string]string{
		"env:APP_TOKEN":     `tok\en"v"`,
		"file:/tmp/app.key": "line1\nline2\n",
	}}
	adapter := proc.New(root, resolver)

	spec := &domain.ApplicationSpec{
		Application: "modes",
		Runtime:     domain.RuntimeKindGo,
		Artifact:    domain.SpecArtifact{Digest: contractDigest("x")},
		Exec: domain.SpecExec{
			Argv:             []string{"/bin/sleep", "1"},
			WorkingDirectory: "/var/lib/modes",
			RunUser:          "appuser",
			Environment:      map[string]string{"PLAIN": "value"},
			SecretEnvironment: map[string]domain.SecretRef{
				"APP_TOKEN": {Kind: domain.SecretKindEnv, Name: "APP_TOKEN"},
				"APP_KEY":   {Kind: domain.SecretKindFile, Name: "/tmp/app.key"},
			},
		},
		Health: domain.SpecHealth{Readiness: domain.SpecReadiness{
			Type: domain.ReadinessTCP, Target: "127.0.0.1:1", ConsecutiveSuccesses: 1,
		}},
		Logs: domain.SpecLogs{Directory: "/var/log/modes"},
	}
	if err := adapter.Prepare(context.Background(), spec); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// 沙箱路径由适配器给出，测试不复制沙箱规则。
	sandbox := func(absolute string) string { return adapter.SandboxPath("modes", absolute) }

	// 敏感环境文件 0600，非敏感 0600，凭据目录 0700，凭据文件 0600。
	for path, wantMode := range map[string]os.FileMode{
		sandbox(domain.EnvFilePath("modes")):                          0o600,
		sandbox(domain.SecretsEnvFilePath("modes")):                   0o600,
		sandbox(domain.SecretsDir("modes")):                           0o700,
		filepath.Join(sandbox(domain.SecretsDir("modes")), "APP_KEY"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Fatalf("%s 权限 want %o, got %o", path, wantMode, got)
		}
	}

	// 多行的 kind=file 凭据必须原样落盘。
	content, err := os.ReadFile(filepath.Join(sandbox(domain.SecretsDir("modes")), "APP_KEY"))
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if string(content) != "line1\nline2\n" {
		t.Fatalf("多行凭据必须逐字节保留，got %q", content)
	}

	// 敏感环境文件里的值必须按实测规则转义，且 kind=file 的变量承载的是路径。
	rawSecrets, err := os.ReadFile(sandbox(domain.SecretsEnvFilePath("modes")))
	if err != nil {
		t.Fatalf("read secrets env: %v", err)
	}
	secrets := string(rawSecrets)

	// 源值 tok\en"v" ：反斜杠翻倍、双引号前加反斜杠。
	const wantToken = `APP_TOKEN="tok\\en\"v\""`
	if !strings.Contains(secrets, wantToken) {
		t.Fatalf("凭据值未按实测规则转义，want 包含 %s，got %q", wantToken, secrets)
	}

	wantKey := `APP_KEY="` + filepath.Join(sandbox(domain.SecretsDir("modes")), "APP_KEY") + `"`
	if !strings.Contains(secrets, wantKey) {
		t.Fatalf("kind=file 的变量应当承载文件路径，want 包含 %s，got %q", wantKey, secrets)
	}

	// 非敏感文件不得出现任何凭据值。
	rawPlain, err := os.ReadFile(sandbox(domain.EnvFilePath("modes")))
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if strings.Contains(string(rawPlain), "tok") {
		t.Fatalf("非敏感环境文件里不得出现凭据内容，got %q", rawPlain)
	}

	// 重复 Prepare 之后权限不得被改坏（os.WriteFile 对已存在文件不改权限）。
	if err := os.Chmod(sandbox(domain.EnvFilePath("modes")), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := adapter.Prepare(context.Background(), spec); err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	info, err := os.Stat(sandbox(domain.EnvFilePath("modes")))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("Prepare 必须把权限改回 0600，got %o", got)
	}
}

// 含换行的 kind=env 凭据必须被拒绝，而不是写成被吃掉换行的值。
func TestPrepareRejectsMultilineEnvSecret(t *testing.T) {
	root := t.TempDir()
	resolver := &stubResolver{values: map[string]string{
		"env:MULTILINE": "line1\nline2",
	}}
	adapter := proc.New(root, resolver)

	spec := &domain.ApplicationSpec{
		Application: "multiline",
		Runtime:     domain.RuntimeKindGo,
		Artifact:    domain.SpecArtifact{Digest: contractDigest("x")},
		Exec: domain.SpecExec{
			Argv:             []string{"/bin/sleep", "1"},
			WorkingDirectory: "/var/lib/multiline",
			RunUser:          "appuser",
			SecretEnvironment: map[string]domain.SecretRef{
				"MULTILINE": {Kind: domain.SecretKindEnv, Name: "MULTILINE"},
			},
		},
		Health: domain.SpecHealth{Readiness: domain.SpecReadiness{
			Type: domain.ReadinessTCP, Target: "127.0.0.1:1", ConsecutiveSuccesses: 1,
		}},
		Logs: domain.SpecLogs{Directory: "/var/log/multiline"},
	}

	err := adapter.Prepare(context.Background(), spec)
	if domain.CodeOf(err) != v1.CodeSecretUnresolved {
		t.Fatalf("want SECRET_UNRESOLVED, got %v", err)
	}
}

// contractDigest 把任意名字映射成一个**合法**的 sha256 摘要。
//
// 摘要从迭代 3 起是严格校验的（前缀对、长度不对的写法在提交期就被拒），因此夹具不能再
// 用 "sha256:x" 这种"看起来像摘要"的字符串。这里用十六进制的名字填充到 64 位：同一名字
// 永远得到同一个摘要，不同名字几乎不可能相同。
func contractDigest(name string) string {
	var b strings.Builder
	for i := 0; i < 64; i++ {
		b.WriteByte(hexDigits[hashByte(name, i)%16])
	}
	return "sha256:" + b.String()
}

const hexDigits = "0123456789abcdef"

func hashByte(name string, index int) int {
	sum := index + 7
	for i := 0; i < len(name); i++ {
		sum = (sum*31 + int(name[i])) % 251
	}
	return sum
}

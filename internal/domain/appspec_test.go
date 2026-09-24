package domain

import (
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// ==== 迭代 3：argv 的解析规则与新增字段 ====

func TestResolveArgv(t *testing.T) {
	const app = "orders-api"
	base := CurrentReleaseDir(app) // /opt/opsd/apps/orders-api/releases/current

	cases := []struct {
		name    string
		argv    []string
		want    []string
		wantErr bool
	}{
		{
			// Go 的典型形态：编译产物住在 release 里。
			name: "相对路径拼到 current 之下",
			argv: []string{"bin/server", "--config", "/etc/orders/config.yaml"},
			want: []string{base + "/bin/server", "--config", "/etc/orders/config.yaml"},
		},
		{
			// Java 的典型形态：解释器是绝对路径，其余参数**一个字节都不动**
			// （`-Xmx512m` 不是路径；`app.jar` 由 java 按自己的工作目录解释）。
			name: "解释器之后的参数原样",
			argv: []string{"/opt/jdk-17.0.1/bin/java", "-Xmx512m", "-jar", "app.jar"},
			want: []string{"/opt/jdk-17.0.1/bin/java", "-Xmx512m", "-jar", "app.jar"},
		},
		{
			// 这条是本用例存在的理由：把参数也当路径解析会把它们**静默改写**——
			// 不是报错，是行为变了。
			name: "带等号的参数不被当成路径",
			argv: []string{"bin/server", "-Dlogging.file=/var/log/orders/app.log"},
			want: []string{base + "/bin/server", "-Dlogging.file=/var/log/orders/app.log"},
		},
		{
			name: "单元素相对路径",
			argv: []string{"server"},
			want: []string{base + "/server"},
		},
		{
			name: "相对 argv[0] 里的 . 被规范化",
			argv: []string{"./bin/server", "--flag"},
			want: []string{base + "/bin/server", "--flag"},
		},
		{
			// 逃出 release 目录：path.Join 会把它清成 current 的兄弟目录，
			// 前缀校验必须拦住它。
			name:    "相对 argv[0] 用 .. 逃出 release",
			argv:    []string{"../../../etc/shadow"},
			wantErr: true,
		},
		{
			name:    "相对 argv[0] 里夹着 ..",
			argv:    []string{"bin/../../../../etc/passwd"},
			wantErr: true,
		},
		{
			// 绝对 argv[0] 原样使用：用户可以指向系统二进制（那是他自己的选择），
			// 我们只保证**相对** argv[0] 不会逃出 release。
			name: "绝对 argv[0] 原样",
			argv: []string{"/usr/bin/env", "sh"},
			want: []string{"/usr/bin/env", "sh"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveArgv(app, tc.argv)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应当被拒: %v", tc.argv)
				}
				if code := CodeOf(err); code != v1.CodeManifestInvalid {
					t.Fatalf("want MANIFEST_INVALID, got %v", code)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveArgv: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("元素个数不符: want %v, got %v", tc.want, got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("第 %d 个元素: want %q, got %q", i, tc.want[i], got[i])
				}
			}
		})
	}
}

// 相对 argv 现在合法（迭代 3 规格 D4），但**相对元素里的 ..** 在校验期就该被拒——
// 它是逃出 release 目录最直接的手段，而 argv 会变成 ExecStart=。
func TestSpecExecRejectsDotDotInRelativeArgv(t *testing.T) {
	spec := validGoSpec()
	spec.Exec.Argv = []string{"bin/../../etc/shadow"}
	if err := spec.Validate(); err == nil {
		t.Fatal("相对元素里的 .. 必须被拒")
	} else if CodeOf(err) != v1.CodeManifestInvalid {
		t.Fatalf("want MANIFEST_INVALID, got %v", CodeOf(err))
	}

	// 绝对路径里出现 .. 不属于这条规则管的范围：用户本来就能直接写 /etc/shadow，
	// 拦 .. 只是自欺。
	spec = validGoSpec()
	spec.Exec.Argv = []string{"/usr/bin/env", "sh"}
	if err := spec.Validate(); err != nil {
		t.Fatalf("绝对路径不该被 .. 规则拦住: %v", err)
	}
}

func TestSpecResourcesAndReleaseValidation(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*ApplicationSpec)
		wantError bool
	}{
		{"默认 keepLast", func(*ApplicationSpec) {}, false},
		{"cpuQuotaPercent 上限内", func(s *ApplicationSpec) { s.Resources.CPUQuotaPercent = 10000 }, false},
		{"cpuQuotaPercent 越界", func(s *ApplicationSpec) { s.Resources.CPUQuotaPercent = 10001 }, true},
		{"cpuQuotaPercent 为负", func(s *ApplicationSpec) { s.Resources.CPUQuotaPercent = -1 }, true},
		{"memoryMaxBytes 为 0（不限制）", func(s *ApplicationSpec) { s.Resources.MemoryMaxBytes = 0 }, false},
		{"memoryMaxBytes 太小", func(s *ApplicationSpec) { s.Resources.MemoryMaxBytes = 1 << 20 }, true},
		{"memoryMaxBytes 合理", func(s *ApplicationSpec) { s.Resources.MemoryMaxBytes = 512 << 20 }, false},
		{"keepLast 显式给值", func(s *ApplicationSpec) { s.Release.KeepLast = 3 }, false},
		{"keepLast 越界", func(s *ApplicationSpec) { s.Release.KeepLast = MaxKeepReleases + 1 }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := validGoSpec()
			tc.mutate(spec)
			err := spec.Validate()
			if tc.wantError && err == nil {
				t.Fatal("应当被拒")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("不该被拒: %v", err)
			}
			if !tc.wantError && spec.Release.KeepLast == 0 {
				t.Fatal("校验之后 keepLast 必须落到默认值")
			}
		})
	}
}

// keepLast=0 是「没写」，不是「不清理」：制品永远在制品库里、重新解包即可，因此
// 「永不清理 release 目录」没有真实价值，只会把盘慢慢占满。
func TestSpecReleaseRejectsExplicitZero(t *testing.T) {
	spec := validGoSpec()
	spec.Release = SpecRelease{KeepLast: 0}
	if err := spec.Validate(); err != nil {
		t.Fatalf("没写 keepLast 应当走默认值: %v", err)
	}
	if spec.Release.KeepLast != DefaultKeepLast {
		t.Fatalf("want %d, got %d", DefaultKeepLast, spec.Release.KeepLast)
	}
}

func TestVersionOrDerived(t *testing.T) {
	explicit := SpecArtifact{ID: "art_1", Version: "1.2.3"}
	if got := explicit.VersionOrDerived(); got != "1.2.3" {
		t.Fatalf("写了版本号就该用它，got %q", got)
	}

	derived := SpecArtifact{ID: "art_1", Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	if got := derived.VersionOrDerived(); got != "sha256-0123456789ab" {
		t.Fatalf("没写版本号应当按 digest 前 12 位派生，got %q", got)
	}

	// 同一个制品永远得到同一个版本号——这正是「版本号不可复用」想要的性质。
	if a, b := derived.VersionOrDerived(), derived.VersionOrDerived(); a != b {
		t.Fatalf("派生版本号必须稳定: %q vs %q", a, b)
	}
}

// validGoSpec 造一份最小的合法 go 规格。
func validGoSpec() *ApplicationSpec {
	return &ApplicationSpec{
		APIVersion:  ManifestAPIVersion,
		Kind:        ManifestKind,
		Application: "orders-api",
		Runtime:     RuntimeKindGo,
		Artifact:    SpecArtifact{ID: "art_1", Version: "1.0.0"},
		Exec: SpecExec{
			Argv:             []string{"bin/server"},
			WorkingDirectory: "/var/lib/orders-api",
			RunUser:          "orders-api",
		},
		Health: SpecHealth{
			Readiness: SpecReadiness{Type: ReadinessTCP, Target: "127.0.0.1:8080"},
		},
		Logs: SpecLogs{Directory: "/var/log/orders-api"},
	}
}

package systemd

import (
	"strings"
	"testing"
)

func TestEscapeArg(t *testing.T) {
	cases := map[string]string{
		"":                 `""`,
		"plain":            "plain",
		"/opt/app/bin/run": "/opt/app/bin/run",
		"a b":              `"a b"`,
		"a\tb":             `"a	b"`,
		`a"b`:              `"a\"b"`,
		`a\b`:              `"a\\b"`,
		"a'b":              `"a'b"`,
		`--flag=a b`:       `"--flag=a b"`,
		`a\"b`:             `"a\\\"b"`,
	}
	for input, want := range cases {
		if got := EscapeArg(input); got != want {
			t.Fatalf("EscapeArg(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestEscapeArgs(t *testing.T) {
	got := EscapeArgs([]string{"/opt/app/bin/run", "--name", "a b", ""})
	want := `/opt/app/bin/run --name "a b" ""`
	if got != want {
		t.Fatalf("EscapeArgs = %q, want %q", got, want)
	}
}

// 期望值是逐字节实测的结果：这些输入按下面的写法送进 systemd 的 EnvironmentFile，
// 进程收到的字节与源完全一致。改动这里之前请先在 Linux 容器里重跑端到端验证。
func TestEscapeEnvValueMatchesMeasuredBehaviour(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"反斜杠", `a\b`, `"a\\b"`},
		{"双引号", `a"b`, `"a\"b"`},
		{"字面反斜杠加 n（不是换行）", `a\nb`, `"a\\nb"`},
		{"制表符", "tab\there", "\"tab\there\""},
		{"美元符不展开", "$HOME", `"$HOME"`},
		{"单引号不特殊", "a'b", `"a'b"`},
		{"尾部空格保留", "trail ", `"trail "`},
		{"空值", "", `""`},
		{"纯 ASCII", "abc123", `"abc123"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EscapeEnvValue(tc.value); got != tc.want {
				t.Fatalf("EscapeEnvValue(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// 转义必须覆盖所有会被 systemd 改写的字节，一个都不能漏。
func TestEscapeEnvValueCoversEverySpecialByte(t *testing.T) {
	for _, special := range []byte{'\\', '"'} {
		value := string([]byte{'a', special, 'b'})
		escaped := EscapeEnvValue(value)
		if !strings.Contains(escaped, `\`+string(special)) {
			t.Fatalf("字节 %q 未被转义: %q", special, escaped)
		}
	}
}

func TestRenderEnvFileIsSortedAndStable(t *testing.T) {
	values := map[string]string{
		"ZED":   "last",
		"ALPHA": "first",
		"MID":   `a\b`,
	}
	want := "ALPHA=\"first\"\nMID=\"a\\\\b\"\nZED=\"last\"\n"
	if got := string(RenderEnvFile(values)); got != want {
		t.Fatalf("RenderEnvFile = %q, want %q", got, want)
	}

	// 多次渲染必须字节一致，否则每次 Prepare 都会重写文件、触发无意义的 unit 重启。
	first := string(RenderEnvFile(values))
	for i := 0; i < 8; i++ {
		if got := string(RenderEnvFile(values)); got != first {
			t.Fatalf("渲染结果不稳定: %q vs %q", got, first)
		}
	}
}

func TestRenderEnvFileEmpty(t *testing.T) {
	if got := RenderEnvFile(nil); len(got) != 0 {
		t.Fatalf("空输入应生成空内容，got %q", got)
	}
}

func TestHasNewline(t *testing.T) {
	if HasNewline("single line") {
		t.Fatal("单行不应判定为含换行")
	}
	for _, value := range []string{"a\nb", "a\r\nb", "a\rb"} {
		if !HasNewline(value) {
			t.Fatalf("%q 应当判定为含换行", value)
		}
	}
	// \n 两个字面字符不是换行，必须放行：它正是 EscapeEnvValue 会转义的形态。
	if HasNewline(`a\nb`) {
		t.Fatal("字面反斜杠加 n 不应判定为含换行")
	}
}

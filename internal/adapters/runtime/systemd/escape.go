// Package systemd 实现 RuntimeAdapter 的 systemd 适配器：unit 渲染、安装、
// 启停、状态与健康检查。
package systemd

import (
	"bytes"
	"sort"
	"strings"
)

// EscapeArg 按 systemd 的 ExecStart 语义转义单个参数。
//
// 规则（照 systemd 的解析行为，不自行发明）：
//   - 参数按空白切分，除非整体用双引号包裹；
//   - 空参数写作 ""；
//   - 含空白、双引号、单引号或反斜杠时，整体用双引号包裹，
//     并在双引号内把双引号与反斜杠前加反斜杠；
//   - 其余原样输出。
func EscapeArg(arg string) string {
	if arg == "" {
		return `""`
	}
	if !strings.ContainsAny(arg, " \t\n\r\"'\\") {
		return arg
	}

	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(arg); i++ {
		if c := arg[i]; c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(arg[i])
	}
	b.WriteByte('"')
	return b.String()
}

func EscapeArgs(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		parts = append(parts, EscapeArg(arg))
	}
	return strings.Join(parts, " ")
}

// EscapeEnvValue 按 systemd 的 EnvironmentFile 语义转义单个值。
//
// 这条规则是实测出来的，不是推断的（systemd 255，见 1c 规格第 6 节）：
// 双引号内的 \\ 收敛为一个 \、\" 收敛为 "，其余 \x 原样保留；$ 不做展开；
// 单引号不特殊。因此必须整体加双引号并把 \ 与 " 各自翻倍，
// 否则含反斜杠或双引号的凭据会被 systemd 静默改写——密码里的 \ 会消失。
func EscapeEnvValue(value string) string {
	var b strings.Builder
	b.Grow(len(value) + 2)
	b.WriteByte('"')
	for i := 0; i < len(value); i++ {
		if c := value[i]; c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(value[i])
	}
	b.WriteByte('"')
	return b.String()
}

// RenderEnvFile 生成 environment.d 风格的 KEY=value 文件。
// 键排序后输出，保证内容字节稳定，便于比对与测试。
func RenderEnvFile(values map[string]string) []byte {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	var b bytes.Buffer
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(EscapeEnvValue(values[name]))
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// HasNewline 判定值是否含换行。environment 文件承载不了含真实换行的值
// （续行会把换行吃掉），因此 kind=env 的凭据必须先过这一关。
func HasNewline(value string) bool {
	return strings.ContainsAny(value, "\n\r")
}

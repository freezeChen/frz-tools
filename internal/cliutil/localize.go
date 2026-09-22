// Package cliutil 提供把 cobra 默认帮助界面本地化成中文的辅助函数。
// opsctl 与 opsd 共用，避免两处各写一份而产生偏差。
package cliutil

import (
	"strings"

	"github.com/spf13/cobra"
)

// LocalizeUsage 把 cobra 用法模板里的固定英文标签换成中文。
//
// 只做文本替换、不重写模板结构，这样 cobra 自己维护的分组、别名、示例等分支
// 仍然继续工作；必须在 SetUsageTemplate 之前调用，否则读到的是刚设置过的模板。
func LocalizeUsage(root *cobra.Command) {
	replacements := [][2]string{
		{`Use "{{.CommandPath}} [command] --help" for more information about a command.`,
			`输入 "{{.CommandPath}} [command] --help" 查看某个命令的详细帮助。`},
		{"Additional help topics:", "附加帮助主题:"},
		{"Available Commands:", "可用命令:"},
		{"Additional Commands:", "其他命令:"},
		{"Global Flags:", "全局选项:"},
		{"Aliases:", "别名:"},
		{"Examples:", "示例:"},
		{"Flags:", "选项:"},
		{"Usage:", "用法:"},
	}

	template := root.UsageTemplate()
	for _, pair := range replacements {
		template = strings.Replace(template, pair[0], pair[1], 1)
	}
	root.SetUsageTemplate(template)
}

// LocalizeHelpFlag 递归到每个子命令，把 `-h` 的说明改成中文。帮助旗标由 cobra 按需
// 创建，只改根命令覆盖不到子命令。
func LocalizeHelpFlag(root *cobra.Command) {
	forEachCommand(root, func(cmd *cobra.Command) {
		cmd.InitDefaultHelpFlag()
		if flag := cmd.Flags().Lookup("help"); flag != nil {
			flag.Usage = "显示当前命令的帮助"
		}
	})
}

func forEachCommand(root *cobra.Command, visit func(*cobra.Command)) {
	visit(root)
	for _, sub := range root.Commands() {
		forEachCommand(sub, visit)
	}
}

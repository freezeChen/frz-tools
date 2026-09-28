// Package cliutil 提供把 cobra 默认帮助界面本地化成中文的辅助函数。
// opsctl 与 opsd 共用，避免两处各写一份而产生偏差。
package cliutil

import (
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"

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

// RequireKnownSubcommand 让「只装子命令」的父命令在收到**未知**子命令时报错。
//
// cobra 的默认行为是：有子命令、自己又没有 Run 的父命令，会把未知的输入当成位置参数，
// 打印一遍帮助，然后**以退出码 0 结束**。于是 `opsctl artifact push`（正确名字是 put）
// 在脚本里表现为「命令成功」——与这个仓库最在意的「静默成功」是同一类问题，而它影响的是
// 每一台机器上的每一次手滑。
//
// 遍历整棵树在一处装好，而不是给十几个父命令各加一段：那样迟早会漏掉新建的那个，
// 而漏掉的表现正是「没有报错」。
//
// 直接给了子命令、或给了 --help 时不受影响（cobra 在那两种情况下不会走到这里）；
// 光写父命令名（`opsctl app`）仍然打印帮助并成功——那是正常的用法，不是错误。
func RequireKnownSubcommand(cmd *cobra.Command) {
	for _, sub := range cmd.Commands() {
		RequireKnownSubcommand(sub)
	}
	if !cmd.HasSubCommands() || cmd.Run != nil || cmd.RunE != nil {
		return
	}
	path := cmd.CommandPath()
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return domain.NewError(v1.CodeInvalidRequest,
				"%s 没有子命令 %q；用 `%s --help` 看有哪些可用", path, args[0], path)
		}
		return cmd.Help()
	}
}

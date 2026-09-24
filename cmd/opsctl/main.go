package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/client"
	"github.com/freezeChen/frz-tools/internal/cliutil"
	"github.com/freezeChen/frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

type rootOptions struct {
	socket  string
	timeout time.Duration
	json    bool
}

func main() {
	root := newRootCommand()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "opsctl:", domain.MessageOf(err))
		os.Exit(v1.ExitCode(domain.CodeOf(err)))
	}
}

func newRootCommand() *cobra.Command {
	opts := &rootOptions{}

	root := &cobra.Command{
		Use:           "opsctl",
		Short:         "opsctl 用于向 opsd 提交操作并查询状态与日志",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&opts.socket, "socket", defaultSocket(), "opsd 的 unix socket 路径（也可用环境变量 OPSD_SOCKET）")
	root.PersistentFlags().DurationVar(&opts.timeout, "timeout", 30*time.Second, "HTTP 客户端超时时间")
	root.PersistentFlags().BoolVar(&opts.json, "json", false, "输出原始 JSON 而不是人类可读文本")

	root.AddCommand(
		newHealthCommand(opts),
		newOperationCommand(opts),
		newArtifactCommand(opts),
		newAppCommand(opts),
		newReleaseCommand(opts),
		newSpecCommand(opts),
		newRuntimeCommand(opts),
		newHostCommand(opts),
		newEnvCommand(opts),
		newScheduleCommand(opts),
		newConfigCommand(opts),
	)
	localizeBuiltinCommands(root)
	return root
}

// completionShort 是 cobra 自动生成的 shell 补全子命令的中文说明。
var completionShort = map[string]string{
	"bash":       "bash",
	"zsh":        "zsh",
	"fish":       "fish",
	"powershell": "PowerShell",
}

// localizeBuiltinCommands 把 cobra 自带的 help 与 completion 命令改成中文。
// 这两条命令由 cobra 在 Execute 时才创建，所以这里先手动触发一次创建再改文案；
// 之后 cobra 自己那次会因为「已经存在」而跳过，不会把英文覆盖回来。
func localizeBuiltinCommands(root *cobra.Command) {
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	// 用法模板与帮助旗标的说明由 cobra 维护，这里只替换其中的固定英文标签，
	// 保留模板本身的结构，避免漏掉分组、别名、示例这些分支。
	cliutil.LocalizeUsage(root)
	cliutil.LocalizeHelpFlag(root)

	for _, cmd := range root.Commands() {
		switch cmd.Name() {
		case "help":
			cmd.Short = "查看任意命令的帮助"
			cmd.Long = "查看某个命令的帮助。\n用法：opsctl help [命令路径]"
		case "completion":
			cmd.Short = "为指定的 shell 生成自动补全脚本"
			cmd.Long = fmt.Sprintf("为指定的 shell 生成 %s 的自动补全脚本，详见各子命令的帮助。", root.Name())
			for _, sub := range cmd.Commands() {
				if shell, ok := completionShort[sub.Name()]; ok {
					sub.Short = fmt.Sprintf("为 %s 生成自动补全脚本", shell)
					sub.Long = fmt.Sprintf(completionLong[sub.Name()], root.Name())
					if flag := sub.Flags().Lookup("no-descriptions"); flag != nil {
						flag.Usage = "生成的脚本里不写入补全描述"
					}
				}
			}
		}
	}
}

// completionLong 是各 shell 补全子命令的详细帮助。其中的 shell 命令本身保持原样，
// 用户需要原样复制执行，因此不做翻译。
var completionLong = map[string]string{
	"bash": `为 bash 生成自动补全脚本。

该脚本依赖 bash-completion 包，若尚未安装请用系统包管理器安装。

当前会话加载一次：

	source <(%[1]s completion bash)

每个新会话都加载，执行一次：

#### Linux:

	%[1]s completion bash > /etc/bash_completion.d/%[1]s

#### macOS:

	%[1]s completion bash > $(brew --prefix)/etc/bash_completion.d/%[1]s

改动之后需要重新开一个 shell 才会生效。
`,
	"zsh": `为 zsh 生成自动补全脚本。

若环境中尚未启用补全，先执行一次：

	echo "autoload -U compinit; compinit" >> ~/.zshrc

然后把脚本放到 fpath 里（先执行 ${fpath[1]} 确认目录）：

	%[1]s completion zsh > "${fpath[1]}/_%[1]s"

改动之后需要重新开一个 shell 才会生效。
`,
	"fish": `为 fish 生成自动补全脚本。

当前会话加载一次：

	%[1]s completion fish | source

每个新会话都加载，执行一次：

	%[1]s completion fish > ~/.config/fish/completions/%[1]s.fish
`,
	"powershell": `为 PowerShell 生成自动补全脚本。

动态补全需要 PowerShell 5.2 及以上，且 PSReadLine 不低于 2.1.0。

当前会话加载一次：

	%[1]s completion powershell | Out-String | Invoke-Expression

每个新会话都加载，把上面这条命令追加到 $PROFILE 文件里。
`,
}

// defaultSocket 默认连接的 opsd socket 路径，可用 OPSD_SOCKET 环境变量覆盖。
func defaultSocket() string {
	if fromEnv := os.Getenv("OPSD_SOCKET"); fromEnv != "" {
		return fromEnv
	}
	return "/run/opsd/opsd.sock"
}

func (o *rootOptions) client() *client.Client {
	return client.New(o.socket, o.timeout)
}

func (o *rootOptions) printJSON(value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

// printOperation 的字段名刻意与 api/v1 的 JSON 字段保持一致（见 AGENTS.md 的
// 语言约定：API 字段名不翻译），这样人类可读输出与 --json 输出可以逐字段对照。
func printOperation(op *v1.Operation) {
	fmt.Printf("id:        %s\n", op.ID)
	fmt.Printf("kind:      %s\n", op.Kind)
	fmt.Printf("resource:  %s\n", op.Resource)
	fmt.Printf("status:    %s\n", op.Status)
	if op.Phase != "" {
		fmt.Printf("phase:     %s\n", op.Phase)
	}
	fmt.Printf("dryRun:    %t\n", op.DryRun)
	if op.RetryOf != "" {
		fmt.Printf("retryOf:   %s\n", op.RetryOf)
	}
	// 只有真的牵涉重试时才打印这几行，避免给每一个普通操作都加噪音。
	if op.MaxAttempts > 1 || op.Attempt > 1 {
		fmt.Printf("attempt:   %d/%d\n", op.Attempt, op.MaxAttempts)
	}
	if op.NextAttemptAt != nil {
		fmt.Printf("nextAttemptAt: %s\n", op.NextAttemptAt.Format(time.RFC3339))
	}
	if op.RetryExhausted {
		fmt.Println("retry:     尝试次数已用尽，不会再自动重试")
	}
	if op.ExitCode != nil {
		fmt.Printf("exitCode:  %d\n", *op.ExitCode)
	}
	if op.ErrorCode != "" {
		fmt.Printf("error:     %s (%s)\n", op.ErrorCode, op.ErrorMessage)
	}
	fmt.Printf("createdAt: %s\n", op.CreatedAt.Format(time.RFC3339))
	if op.FinishedAt != nil {
		fmt.Printf("finishedAt:%s\n", op.FinishedAt.Format(time.RFC3339))
	}
}

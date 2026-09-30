package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/client"
	"github.com/freezeChen/frz-tools/internal/cliutil"
	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/pki"

	"github.com/spf13/cobra"
)

type rootOptions struct {
	socket  string
	timeout time.Duration
	json    bool

	// 远程目标（迭代 5a）。--host 与 --remote 都与 --socket 互斥。
	host       string
	remote     string
	clientCert string
	clientKey  string
	caCert     string

	// ready 是 PersistentPreRunE 里定好的客户端。为 nil 时退回本机 socket——
	// 那条路不需要任何解析，因此即使预处理没跑（例如直接调内部函数）也仍然可用。
	ready *client.Client
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
		// 目标解析放在这里而不是每个命令里：`--host` 需要先查本机库才知道地址，
		// 而那是一次可能失败的 IO。放在预处理里，所有命令都自动获得「选目标」的能力，
		// 也不会有人漏判互斥。
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return opts.prepare(cmd.Context(), cmd)
		},
	}

	root.PersistentFlags().StringVar(&opts.socket, "socket", defaultSocket(), "opsd 的 unix socket 路径（也可用环境变量 OPSD_SOCKET）")
	root.PersistentFlags().DurationVar(&opts.timeout, "timeout", 30*time.Second, "HTTP 客户端超时时间")
	root.PersistentFlags().BoolVar(&opts.json, "json", false, "输出原始 JSON 而不是人类可读文本")

	// 远程目标的三个选择与身份三件套。地址与证书刻意分成两组：--host 是从本机库
	// 里查地址，--remote 直接给地址（排障时本机 opsd 可能正好没起来）。
	root.PersistentFlags().StringVar(&opts.host, "host", "", "打到本机库里登记的那台主机上（地址从记录里读）")
	root.PersistentFlags().StringVar(&opts.remote, "remote", "", "直接打到这个 host:port 上的 opsd（不查库，排障用）")
	root.PersistentFlags().StringVar(&opts.clientCert, "client-cert", envOr("OPSD_CLIENT_CERT", ""), "mTLS 客户端证书路径；连接远程目标（--remote、host check、host list --check）时使用（也可用环境变量 OPSD_CLIENT_CERT）")
	root.PersistentFlags().StringVar(&opts.clientKey, "client-key", envOr("OPSD_CLIENT_KEY", ""), "mTLS 客户端私钥路径，模式必须不宽于 0600（也可用环境变量 OPSD_CLIENT_KEY）")
	root.PersistentFlags().StringVar(&opts.caCert, "ca-cert", envOr("OPSD_CA_CERT", ""), "校验对端证书用的 CA 路径；连接远程目标时使用（也可用环境变量 OPSD_CA_CERT）")

	root.AddCommand(
		newHealthCommand(opts),
		newIdentityCommand(opts),
		newStatusCommand(opts),
		newVersionCommand(opts),
		newInitCommand(opts),
		newOperationCommand(opts),
		newArtifactCommand(opts),
		newAppCommand(opts),
		newReleaseCommand(opts),
		newSpecCommand(opts),
		newRuntimeCommand(opts),
		newHostCommand(opts),
		newEnvCommand(opts),
		newScheduleCommand(opts),
		newBackupCommand(opts),
		newConfigCommand(opts),
	)
	localizeBuiltinCommands(root)
	// 放在 localizeBuiltinCommands 之后：cobra 自带的 help/completion 是在那一步才
	// 被创建出来的，早于它的遍历会漏掉它们。
	cliutil.RequireKnownSubcommand(root)
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

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// prepare 决定这一次请求打到谁那里，并把客户端定下来。
//
// 互斥是硬要求而不是礼貌：`--socket` 与 `--remote` 同时给，只有一种顺序会让调用方
// 得到它以为的结果，而另一种会静默地打到本机——「我以为我在操作生产、其实动的是本机」
// 是这类工具最不该犯的错。因此宁可直接拒绝。
func (o *rootOptions) prepare(ctx context.Context, cmd *cobra.Command) error {
	chosen := make([]string, 0, 3)
	if cmd.Flags().Changed("host") {
		chosen = append(chosen, "--host")
	}
	if cmd.Flags().Changed("remote") {
		chosen = append(chosen, "--remote")
	}
	if cmd.Flags().Changed("socket") {
		chosen = append(chosen, "--socket")
	}
	if len(chosen) > 1 {
		return domain.NewError(v1.CodeInvalidRequest,
			"%s 互斥，只能给一个（不给就是本机 socket）", strings.Join(chosen, " 与 "))
	}

	switch {
	case o.remote != "":
		ready, err := o.remoteClient(o.remote)
		if err != nil {
			return err
		}
		o.ready = ready
	case o.host != "":
		ready, err := o.hostClient(ctx)
		if err != nil {
			return err
		}
		o.ready = ready
	default:
		o.ready = client.New(o.socket, o.timeout)
	}
	return nil
}

// hostClient 从**本机库**里查出那台主机的地址，再连过去。
func (o *rootOptions) hostClient(ctx context.Context) (*client.Client, error) {
	host, err := client.New(o.socket, o.timeout).GetHost(ctx, o.host)
	if err != nil {
		// 最可能的原因是本机 opsd 没在跑。把替代方案直接写进报错里：排障时
		// 正是最需要连上另一台机的时候，而这时本机往往正好是坏的。
		if domain.CodeOf(err) == v1.CodeInternal {
			return nil, domain.NewError(v1.CodeInternal,
				"--host %s 需要先问本机 opsd 要地址，但本机连不上（%v）；"+
					"排障时可以直接用 --remote <host:port>", o.host, err)
		}
		return nil, err
	}
	if host.Address == "" {
		return nil, domain.NewError(v1.CodeInvalidRequest,
			"主机 %q 的 address 为空，它是**本机**记录：不要加 --host，直接执行即可", host.Name)
	}
	return o.remoteClient(host.Address)
}

// remoteClient 按地址与证书建一个远程客户端。
func (o *rootOptions) remoteClient(address string) (*client.Client, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, domain.NewError(v1.CodeInvalidRequest,
			"远程地址 %q 必须写成 host:port（例如 10.0.0.5:9443）", address)
	}
	// ServerName 取地址里的主机名：证书校验的对象是「我用来连它的那个名字」，
	// 因此给目标机签证书时 SAN 必须包含它。
	tlsConfig, err := pki.ClientTLSConfig(o.clientCert, o.clientKey, o.caCert, host)
	if err != nil {
		return nil, err
	}
	return client.NewRemote(address, tlsConfig, o.timeout), nil
}

func (o *rootOptions) client() *client.Client {
	if o.ready != nil {
		return o.ready
	}
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

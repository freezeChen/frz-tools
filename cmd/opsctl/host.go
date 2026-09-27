package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/client"
	"github.com/freezeChen/frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

func newHostCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "管理主机记录：1c 只建身份与标签，地址为空表示本机",
		Long: `管理主机记录。

address 为空表示**本机**；填了地址的记录（host:port）会被 opsctl 真的拿去拨号，
因此 --host 与 host check 都要求地址是能连上的形态。`,
	}
	cmd.AddCommand(
		newHostListCommand(opts),
		newHostCreateCommand(opts),
		newHostInspectCommand(opts),
		newHostCheckCommand(opts),
	)
	return cmd
}

// newHostCheckCommand 连过去问对端「你是谁、我以什么身份连上来的」。
//
// 它是 `make verify-host` 之外，运维手上唯一一条「这条远程通道到底通不通」的命令：
// 报出来的 backend / clientCn / scope 三个字段，正好对应远程链路上最容易错的三处
// ——打错了机器、证书不是这张、档位配低了。
func newHostCheckCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "check <name-or-id>",
		Short: "连到那台主机并报出它的身份与本次连接的身份",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			host, err := opts.client().GetHost(ctx, args[0])
			if err != nil {
				return err
			}
			target, err := opts.clientForKey(host.Address)
			if err != nil {
				return err
			}
			identity, err := target.Identity(ctx)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(identity)
			}
			printHostAndIdentity(host, identity)
			return nil
		},
	}
}

func newHostListCommand(opts *rootOptions) *cobra.Command {
	var check bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出主机",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := opts.client().ListHosts(cmd.Context())
			if err != nil {
				return err
			}
			if opts.json {
				if !check {
					return opts.printJSON(response)
				}
				results := checkHosts(cmd.Context(), opts, response.Items)
				if err := opts.printJSON(results); err != nil {
					return err
				}
				return unreachableError(results)
			}
			if check {
				return printHostChecks(cmd.Context(), opts, response.Items)
			}
			for i := range response.Items {
				printHost(&response.Items[i])
				fmt.Println()
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&check, "check", false, "逐台连过去探活并给出汇总（结果不写库）")
	return cmd
}

// hostCheckResult 是一台主机的一次探活结果。它刻意**不落库**：写进去就会出现
// 「库里说可达、其实三小时前就挂了」——迭代 4c 刚为「第二份会过期的真相」做过
// 一整套对账，不在这里再造一个。
type hostCheckResult struct {
	Name      string `json:"name"`
	Address   string `json:"address"`
	Reachable bool   `json:"reachable"`
	// Identity 只在可达时有值。
	Identity *v1.IdentityResponse `json:"identity,omitempty"`
	// Error 只在不可达时有值，内容就是那道错（HOST_UNREACHABLE / HOST_TLS_FAILED /
	// REMOTE_FORBIDDEN 各自说各自的事）。
	Error string `json:"error,omitempty"`
}

func checkHosts(ctx context.Context, opts *rootOptions, hosts []v1.Host) []hostCheckResult {
	results := make([]hostCheckResult, 0, len(hosts))
	for i := range hosts {
		host := hosts[i]
		result := hostCheckResult{Name: host.Name, Address: host.Address}
		target, err := opts.clientForKey(host.Address)
		if err == nil {
			var identity *v1.IdentityResponse
			identity, err = target.Identity(ctx)
			if err == nil {
				result.Reachable = true
				result.Identity = identity
			}
		}
		if err != nil {
			result.Error = domain.MessageOf(err)
		}
		results = append(results, result)
	}
	return results
}

func printHostChecks(ctx context.Context, opts *rootOptions, hosts []v1.Host) error {
	results := checkHosts(ctx, opts, hosts)
	reachable := 0
	for _, result := range results {
		address := result.Address
		if address == "" {
			address = "（本机）"
		}
		if result.Reachable {
			reachable++
			fmt.Printf("可达    %-20s %s  对方主机名=%s 版本=%s\n",
				result.Name, address, result.Identity.Hostname, displayVersion(result.Identity.DaemonVersion))
			fmt.Printf("        身份=%s 档位=%s 应用数=%s\n",
				displayClientCn(result.Identity), result.Identity.Scope, displayCount(result.Identity.Applications))
			continue
		}
		fmt.Printf("不可达  %-20s %s  %s\n", result.Name, address, result.Error)
	}
	fmt.Printf("\n%d 台可达，%d 台不可达（共 %d 台）\n", reachable, len(results)-reachable, len(results))
	return unreachableError(results)
}

// unreachableError 把「有主机不可达」变成退出码 33。
//
// **文本与 --json 两条路都走它**：只在文本那条路上返回错误，会让脚本用 --json 时
// 把「集群少了一半」读成成功——退出码是脚本唯一看得见的东西。
func unreachableError(results []hostCheckResult) error {
	unreachable := 0
	for _, result := range results {
		if !result.Reachable {
			unreachable++
		}
	}
	if unreachable == 0 {
		return nil
	}
	return domain.NewError(v1.CodeHostUnreachable, "有 %d 台主机不可达", unreachable)
}

// clientForKey 按一条主机记录给出该用哪个客户端。
//
// address 为空 = 本机，这时用的就是**当前目标**的客户端：进程所连的那台机器上，
// address 为空的那条记录就是它自己。因此 `opsctl --host web1 host list --check`
// 报的第一行是 web1 本机，而不是调用方那台。
func (o *rootOptions) clientForKey(address string) (*client.Client, error) {
	if address == "" {
		return o.client(), nil
	}
	return o.remoteClient(address)
}

func printHostAndIdentity(host *v1.Host, identity *v1.IdentityResponse) {
	printHost(host)
	fmt.Printf("hostname:  %s\n", identity.Hostname)
	fmt.Printf("version:   %s\n", displayVersion(identity.DaemonVersion))
	fmt.Printf("backend:   %s\n", identity.Backend)
	fmt.Printf("clientCn:  %s\n", displayClientCn(identity))
	fmt.Printf("scope:     %s\n", identity.Scope)
	fmt.Printf("applications: %s\n", displayCount(identity.Applications))
	if len(identity.ApplicationsAllowed) == 0 {
		fmt.Println("allowed:   （本机上的全部应用）")
	} else {
		fmt.Printf("allowed:   %s\n", strings.Join(identity.ApplicationsAllowed, ", "))
	}
}

// displayVersion 把空版本显示成一句人话。空串是「构建信息里没有版本」这个事实，
// 直接印一个空白会让运维以为命令没把这一行打出来。
func displayVersion(version string) string {
	if version == "" {
		return "（构建时未记录）"
	}
	return version
}

func displayClientCn(identity *v1.IdentityResponse) string {
	if identity.ClientCn == "" {
		return "（本机 socket，无证书身份）"
	}
	return identity.ClientCn
}

func displayCount(count *int) string {
	if count == nil {
		return "未知（对端的库读不出来）"
	}
	return strconv.Itoa(*count)
}

func newHostInspectCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name-or-id>",
		Short: "查看单个主机",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host, err := opts.client().GetHost(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(host)
			}
			printHost(host)
			return nil
		},
	}
}

func newHostCreateCommand(opts *rootOptions) *cobra.Command {
	var (
		address string
		labels  []string
	)

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "创建主机记录",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			parsed, err := parseLabels(labels)
			if err != nil {
				return err
			}
			host, err := opts.client().CreateHost(cmd.Context(), args[0], address, parsed)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(host)
			}
			printHost(host)
			return nil
		},
	}

	cmd.Flags().StringVar(&address, "address", "", "主机地址；留空表示本机")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "键值对标签，可重复使用，例如 --label env=prod")
	return cmd
}

func newEnvCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "管理环境记录：1c 只建身份与标签",
	}
	cmd.AddCommand(newEnvListCommand(opts), newEnvCreateCommand(opts), newEnvInspectCommand(opts))
	return cmd
}

func newEnvInspectCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name-or-id>",
		Short: "查看单个环境",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			environment, err := opts.client().GetEnvironment(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(environment)
			}
			printEnvironment(environment)
			return nil
		},
	}
}

func newEnvListCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出环境",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := opts.client().ListEnvironments(cmd.Context())
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(response)
			}
			for i := range response.Items {
				printEnvironment(&response.Items[i])
				fmt.Println()
			}
			return nil
		},
	}
}

func newEnvCreateCommand(opts *rootOptions) *cobra.Command {
	var labels []string

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "创建环境记录",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			parsed, err := parseLabels(labels)
			if err != nil {
				return err
			}
			environment, err := opts.client().CreateEnvironment(cmd.Context(), args[0], parsed)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(environment)
			}
			printEnvironment(environment)
			return nil
		},
	}

	cmd.Flags().StringArrayVar(&labels, "label", nil, "键值对标签，可重复使用，例如 --label tier=web")
	return cmd
}

// address 为空是本机的语义，因此这里显式写出「本机」而不是留白。
func printHost(host *v1.Host) {
	fmt.Printf("id:        %s\n", host.ID)
	fmt.Printf("name:      %s\n", host.Name)
	if host.Address == "" {
		fmt.Println("address:   （本机）")
	} else {
		fmt.Printf("address:   %s\n", host.Address)
	}
	printLabels(host.Labels)
	fmt.Printf("createdAt: %s\n", host.CreatedAt.Format(time.RFC3339))
}

func printEnvironment(environment *v1.Environment) {
	fmt.Printf("id:        %s\n", environment.ID)
	fmt.Printf("name:      %s\n", environment.Name)
	printLabels(environment.Labels)
	fmt.Printf("createdAt: %s\n", environment.CreatedAt.Format(time.RFC3339))
}

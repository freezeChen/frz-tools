package main

import (
	"context"
	"fmt"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

func newTargetCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "target",
		Short: "管理应用的部署目标：这个应用应该跑在哪些主机上",
		Long: `管理应用的部署目标。

部署目标是**声明的意图**，不是探测的结果：一台机不在这里，意思是「它本来就不该跑这个
应用」，而不是「它现在没跑」。批量发布（app deploy --to @<应用名>）只认这份声明——
靠探测来推导发布范围，会把「挂了的机器」悄悄从范围里去掉，而那是这类工具最不该做的错。

登记的目标主机必须是本机主机表里已有的记录（opsctl host create）；写错的后果不该等到
部署那天才显形。`,
	}
	cmd.AddCommand(newTargetSetCommand(opts), newTargetListCommand(opts))
	return cmd
}

func newTargetSetCommand(opts *rootOptions) *cobra.Command {
	var (
		app   string
		hosts string
	)

	cmd := &cobra.Command{
		Use:   "set --app <应用名> --hosts <主机名,主机名...>",
		Short: "替换某个应用的部署目标列表",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if app == "" {
				return domain.NewError(v1.CodeInvalidRequest, "--app 必填")
			}
			// 刻意不叫 --host：那是**全局**旗标「打到哪台 opsd」，两者在同一个命令行上
			// 会直接撞车（`opsctl app target set --host web-1` 到底选哪个目标？）。
			names := domain.NormalizeTargets(strings.Split(hosts, ","))
			if len(names) == 0 {
				return domain.NewError(v1.CodeInvalidRequest,
					"--hosts 不能为空：至少给一个主机名（逗号分隔）")
			}
			targets, err := opts.client().SetTargets(cmd.Context(), app, names, "")
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(targets)
			}
			// 打印**替换之后**的完整列表：替换语义意味着一次少写一台就是真的少了一台，
			// 而这一点必须让人当场看见。
			fmt.Printf("%s 的部署目标现在是：\n", targets.Application)
			printTargetHosts(targets.Hosts)
			return nil
		},
	}

	cmd.Flags().StringVar(&app, "app", "", "应用名（必填）")
	cmd.Flags().StringVar(&hosts, "hosts", "", "目标主机名，逗号分隔（必填，至少一个）")
	return cmd
}

func newTargetListCommand(opts *rootOptions) *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "list [--app <应用名>]",
		Short: "列出部署目标；不给 --app 时列出全部应用（跨主机汇总）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if app != "" {
				targets, err := opts.client().GetTargets(cmd.Context(), app)
				if err != nil {
					return err
				}
				if opts.json {
					return opts.printJSON(targets)
				}
				fmt.Printf("application: %s\n", targets.Application)
				printTargetHosts(targets.Hosts)
				return nil
			}

			response, err := opts.client().ListTargets(cmd.Context())
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(response)
			}
			if len(response.Items) == 0 {
				fmt.Println("没有任何应用登记了部署目标")
				return nil
			}
			for i := range response.Items {
				fmt.Printf("application: %s\n", response.Items[i].Application)
				printTargetHosts(response.Items[i].Hosts)
				fmt.Println()
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&app, "app", "", "只看某个应用；不给就列出全部")
	return cmd
}

func printTargetHosts(hosts []v1.TargetHost) {
	for _, host := range hosts {
		address := host.Address
		if address == "" {
			address = "（本机）"
		}
		fmt.Printf("  %s\t%s\n", host.Name, address)
	}
}

// parseTargetFlag 解析 `--hosts` 的值：逗号分隔的主机名列表，或 `@应用名`（用那台机上
// 登记的部署目标）。
//
// `@` 前缀是刻意的：引用注册表必须显式，否则「我只想发两台」与「发给注册表里的五台」
// 会长得一模一样，而这种混淆的后果是往计划外的机器上推版本。
func parseTargetFlag(ctx context.Context, opts *rootOptions, raw string) (hosts []string, fromRegistry bool, err error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, false, domain.NewError(v1.CodeInvalidRequest,
			"--hosts 不能为空：给主机名列表（逗号分隔）或用 @<应用名> 引用已登记的部署目标")
	}
	if strings.HasPrefix(value, "@") {
		application := strings.TrimSpace(strings.TrimPrefix(value, "@"))
		if application == "" {
			return nil, false, domain.NewError(v1.CodeInvalidRequest,
				"--hosts @ 后面要写应用名：例如 --hosts @orders-api")
		}
		targets, err := opts.client().GetTargets(ctx, application)
		if err != nil {
			return nil, false, err
		}
		if len(targets.Hosts) == 0 {
			return nil, false, domain.NewError(v1.CodeInvalidRequest,
				"应用 %q 还没有登记部署目标：先跑 opsctl app target set --app %s --host <主机名>",
				application, application)
		}
		names := make([]string, 0, len(targets.Hosts))
		for _, host := range targets.Hosts {
			names = append(names, host.Name)
		}
		return names, true, nil
	}

	parts := strings.Split(value, ",")
	hosts = domain.NormalizeTargets(parts)
	if len(hosts) == 0 {
		return nil, false, domain.NewError(v1.CodeInvalidRequest,
			"--hosts 里没有解析出任何主机名：%q", raw)
	}
	return hosts, false, nil
}

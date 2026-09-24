package main

import (
	"fmt"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/client"
	"github.com/freezeChen/frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

func newRuntimeCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runtime",
		Short: "校验、准备、启停应用的运行时并检查就绪状态",
		Long: "管理应用在目标主机上的运行时。\n\n" +
			"start 与 stop 走 opsd 的 Operation（kind=runtime.start / runtime.stop），\n" +
			"因此可以用 operation get/logs/cancel/retry 查询进度、看日志、取消与重试；\n" +
			"start 会先做一次幂等的 prepare（适配器要求未 prepare 的规格不能被启动）。\n" +
			"validate、prepare、health 是同步调用，立即返回结果。",
	}
	cmd.AddCommand(
		newRuntimeValidateCommand(opts),
		newRuntimePrepareCommand(opts),
		newRuntimeStartCommand(opts),
		newRuntimeStopCommand(opts),
		newRuntimeHealthCommand(opts),
	)
	return cmd
}

func newRuntimeValidateCommand(opts *rootOptions) *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "validate --app <name>",
		Short: "只校验应用的 manifest 能否被本机适配器执行，不产生副作用",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireApp(app); err != nil {
				return err
			}
			result, err := opts.client().ValidateRuntime(cmd.Context(), app)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(result)
			}
			fmt.Printf("%s 的 manifest 可以被本机运行时适配器执行\n", app)
			return nil
		},
	}

	cmd.Flags().StringVar(&app, "app", "", "应用名称或 ID")
	return cmd
}

func newRuntimePrepareCommand(opts *rootOptions) *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "prepare --app <name>",
		Short: "创建运行用户、目录、环境文件与 unit（幂等，可重复执行）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireApp(app); err != nil {
				return err
			}
			result, err := opts.client().PrepareRuntime(cmd.Context(), app)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(result)
			}
			fmt.Printf("%s 已准备完成\n", app)
			printRuntimeDecision(result.Decision)
			return nil
		},
	}

	cmd.Flags().StringVar(&app, "app", "", "应用名称或 ID")
	return cmd
}

func newRuntimeStartCommand(opts *rootOptions) *cobra.Command {
	action := &runtimeActionFlags{}
	cmd := &cobra.Command{
		Use:   "start --app <name>",
		Short: "启动应用：先幂等准备，再启动（走 Operation，可用 operation 命令查询）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRuntimeAction(cmd, opts, v1.KindRuntimeStart, action)
		},
	}
	action.bind(cmd)
	return cmd
}

func newRuntimeStopCommand(opts *rootOptions) *cobra.Command {
	action := &runtimeActionFlags{}
	cmd := &cobra.Command{
		Use:   "stop --app <name>",
		Short: "停止应用（走 Operation，可用 operation 命令查询）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRuntimeAction(cmd, opts, v1.KindRuntimeStop, action)
		},
	}
	action.bind(cmd)
	return cmd
}

func newRuntimeHealthCommand(opts *rootOptions) *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "health --app <name>",
		Short: "查询应用是否已就绪；未就绪时退出码为 19（RUNTIME_NOT_READY）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireApp(app); err != nil {
				return err
			}
			health, err := opts.client().RuntimeHealth(cmd.Context(), app)
			if err != nil {
				return err
			}
			if opts.json {
				if err := opts.printJSON(health); err != nil {
					return err
				}
			} else if health.Ready {
				fmt.Printf("%s 已就绪\n", app)
			} else {
				fmt.Printf("%s 未就绪：%s\n", app, health.Detail)
			}
			if !health.Ready {
				// 退出码 19 就是 RUNTIME_NOT_READY 的含义（健康检查未就绪），
				// 脚本可以直接用它判断，而不必解析输出。
				return domain.NewError(v1.CodeRuntimeNotReady, "应用 %s 未就绪：%s", app, health.Detail)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&app, "app", "", "应用名称或 ID")
	return cmd
}

type runtimeActionFlags struct {
	app            string
	idempotencyKey string
	createdBy      string
	retryMax       int
	retryBase      time.Duration
	retryMaxDelay  time.Duration
}

func (f *runtimeActionFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.app, "app", "", "应用名称或 ID")
	cmd.Flags().StringVar(&f.idempotencyKey, "idempotency-key", "", "幂等键：相同请求重复提交会返回同一个操作")
	cmd.Flags().StringVar(&f.createdBy, "created-by", "", "调用方标识")
	cmd.Flags().IntVar(&f.retryMax, "retry-max", 0,
		"最多尝试次数，含首次（1 表示不重试，上限 10）。省略则不自动重试")
	cmd.Flags().DurationVar(&f.retryBase, "retry-base", 0, "退避基数（默认 5s）")
	cmd.Flags().DurationVar(&f.retryMaxDelay, "retry-max-delay", 0, "退避上限（默认 5m）")
}

// retrySpec 把 --retry-* 组装成策略；一个都没设置时返回 nil，也就是不自动重试。
//
// runtime.start 的重试有一处额外收口：只有「就绪从未通过」才算失败、才可重试；
// 已就绪过再崩溃归 unit 的 Restart= 与健康检查（迭代 1d 规格 D6）。
func (f *runtimeActionFlags) retrySpec(cmd *cobra.Command) (*v1.RetrySpec, error) {
	if !cmd.Flags().Changed("retry-max") && !cmd.Flags().Changed("retry-base") && !cmd.Flags().Changed("retry-max-delay") {
		return nil, nil
	}
	base, err := wholeSeconds(f.retryBase, "retry-base")
	if err != nil {
		return nil, err
	}
	maxDelay, err := wholeSeconds(f.retryMaxDelay, "retry-max-delay")
	if err != nil {
		return nil, err
	}
	return &v1.RetrySpec{
		MaxAttempts:      f.retryMax,
		BaseDelaySeconds: base,
		MaxDelaySeconds:  maxDelay,
	}, nil
}

func runRuntimeAction(cmd *cobra.Command, opts *rootOptions, kind string, flags *runtimeActionFlags) error {
	if err := requireApp(flags.app); err != nil {
		return err
	}

	retry, err := flags.retrySpec(cmd)
	if err != nil {
		return err
	}
	in := client.RuntimeActionInput{
		IdempotencyKey: flags.idempotencyKey,
		CreatedBy:      flags.createdBy,
		Retry:          retry,
	}
	var (
		op      *v1.Operation
		created bool
	)
	if kind == v1.KindRuntimeStart {
		op, created, err = opts.client().StartRuntime(cmd.Context(), flags.app, in)
	} else {
		op, created, err = opts.client().StopRuntime(cmd.Context(), flags.app, in)
	}
	if err != nil {
		return err
	}

	if opts.json {
		return opts.printJSON(op)
	}
	if !created {
		fmt.Println("相同请求已存在，复用既有操作")
	}
	printOperation(op)
	// 操作是异步的：这里只负责提交，进度与日志用既有的 operation 命令查询。
	fmt.Printf("\n查询进度：opsctl operation get %s\n查看日志：opsctl operation logs %s\n", op.ID, op.ID)
	return nil
}

func requireApp(app string) error {
	if app == "" {
		return domain.NewError(v1.CodeInvalidRequest, "必须提供 --app")
	}
	return nil
}

func printRuntimeDecision(decision *v1.RuntimeDecision) {
	if decision == nil {
		return
	}
	fmt.Printf("unit:     %s\n", decision.UnitName)
	fmt.Printf("unitPath: %s\n", decision.UnitPath)
	if decision.Tier != "" {
		fmt.Printf("tier:     %s\n", decision.Tier)
	}
	if decision.SystemdVersion > 0 {
		fmt.Printf("systemd:  %d\n", decision.SystemdVersion)
	}
	for _, degradation := range decision.Degradations {
		fmt.Printf("降级:     %s\n", degradation)
	}
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

func newOperationCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "operation",
		Short: "提交、查询、取消、重试操作并查看日志",
	}
	cmd.AddCommand(
		newSubmitCommand(opts),
		newGetCommand(opts),
		newCancelCommand(opts),
		newRetryCommand(opts),
		newLogsCommand(opts),
	)
	return cmd
}

func newSubmitCommand(opts *rootOptions) *cobra.Command {
	var (
		kind          string
		resource      string
		idemKey       string
		dryRun        bool
		secrets       []string
		retryMax      int
		retryBase     time.Duration
		retryMaxDelay time.Duration
	)

	cmd := &cobra.Command{
		Use:   "submit --kind <kind> --resource <name> [--dry-run] -- <argv...>",
		Short: "提交一个操作交由 opsd 执行",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() == -1 {
				return domain.NewError(v1.CodeInvalidRequest, "请把命令 argv 写在 -- 之后，例如：-- /usr/bin/true")
			}
			if len(args) == 0 {
				return domain.NewError(v1.CodeInvalidRequest, "-- 之后的 argv 不能为空")
			}

			secretEnvironment, err := parseSecretRefs(secrets)
			if err != nil {
				return err
			}

			spec, err := json.Marshal(v1.ExecutorCommandSpec{
				Argv:              args,
				SecretEnvironment: secretEnvironment,
			})
			if err != nil {
				return err
			}

			// --retry-* 任一被显式设置就构成一个重试策略；都没设时不带 retry 段，
			// 也就是不自动重试（1d 规格 D2）。
			var retry *v1.RetrySpec
			if cmd.Flags().Changed("retry-max") || cmd.Flags().Changed("retry-base") || cmd.Flags().Changed("retry-max-delay") {
				base, err := wholeSeconds(retryBase, "retry-base")
				if err != nil {
					return err
				}
				maxDelay, err := wholeSeconds(retryMaxDelay, "retry-max-delay")
				if err != nil {
					return err
				}
				retry = &v1.RetrySpec{
					MaxAttempts:      retryMax,
					BaseDelaySeconds: base,
					MaxDelaySeconds:  maxDelay,
				}
			}

			operation, created, err := opts.client().CreateOperation(cmd.Context(), v1.CreateOperationRequest{
				Kind:           kind,
				Resource:       resource,
				DryRun:         dryRun,
				Spec:           spec,
				IdempotencyKey: idemKey,
				Retry:          retry,
			})
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(operation)
			}
			if !created {
				fmt.Println("该幂等键对应的请求已存在，复用既有操作")
			}
			printOperation(operation)
			return nil
		},
	}

	cmd.Flags().StringVar(&kind, "kind", v1.KindExecutorCommand, "操作类型")
	cmd.Flags().StringVar(&resource, "resource", "", "操作期间持有的资源锁，相同资源会串行执行（必填）")
	cmd.Flags().StringVar(&idemKey, "idempotency-key", "", "幂等键；相同请求再次提交会复用同一个操作")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "只校验并记录计划，不执行任何东西")
	cmd.Flags().IntVar(&retryMax, "retry-max", 0,
		"最多尝试次数，含首次（1 表示不重试，上限 10）。省略则不自动重试")
	cmd.Flags().DurationVar(&retryBase, "retry-base", 0,
		"退避基数（默认 5s），失败后按指数增长并叠加 ±20% 抖动")
	cmd.Flags().DurationVar(&retryMaxDelay, "retry-max-delay", 0,
		"退避上限（默认 5m），也是抖动之后的绝对上限")
	cmd.Flags().StringArrayVar(&secrets, "secret-env", nil,
		"把凭据注入命令环境，格式 VAR=kind:name（kind 为 env 或 file）；明文不会进入请求体或日志")
	_ = cmd.MarkFlagRequired("resource")

	return cmd
}

// parseSecretRefs 解析 VAR=kind:name 形式的旗标：从左到右先按 = 切出环境变量名，
// 再按 : 切出 kind 和 name，因此 file 类凭据的路径里可以继续出现 = 但不能出现 :。
func parseSecretRefs(raw []string) (map[string]v1.SecretRef, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	refs := make(map[string]v1.SecretRef, len(raw))
	for _, item := range raw {
		varName, ref, found := strings.Cut(item, "=")
		if !found || strings.TrimSpace(varName) == "" {
			return nil, domain.NewError(v1.CodeInvalidRequest, "--secret-env %q 必须是 VAR=kind:name 形式", item)
		}
		kind, name, found := strings.Cut(ref, ":")
		if !found || strings.TrimSpace(name) == "" {
			return nil, domain.NewError(v1.CodeInvalidRequest, "--secret-env %q 的 kind 与 name 用冒号分隔", item)
		}
		refs[varName] = v1.SecretRef{Kind: kind, Name: name}
	}
	return refs, nil
}

func newGetCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "get <operation-id>",
		Short: "查看单个操作",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			operation, err := opts.client().GetOperation(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(operation)
			}
			printOperation(operation)
			return nil
		},
	}
}

func newCancelCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <operation-id>",
		Short: "取消一个待执行或运行中的操作",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			operation, err := opts.client().CancelOperation(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(operation)
			}
			printOperation(operation)
			return nil
		},
	}
}

func newRetryCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "retry <operation-id>",
		Short: "把失败或已取消的操作重新建一个操作执行",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			operation, err := opts.client().RetryOperation(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(operation)
			}
			printOperation(operation)
			return nil
		},
	}
}

func newLogsCommand(opts *rootOptions) *cobra.Command {
	var (
		cursor int64
		limit  int
		follow bool
	)

	cmd := &cobra.Command{
		Use:   "logs <operation-id>",
		Short: "打印操作的结构化日志",
		Long: `打印操作的结构化日志。

--follow 持续输出新日志，直到操作到达终态：拉完剩余日志后退出，退出码
沿用该操作终态的映射（成功为 0，失败与取消按 api/v1 的退出码表）。
Ctrl-C 随时停止跟随，操作本身不受影响，仍可用 operation get 查看。

--follow 与 --cursor 同时给出时以 follow 为主，--cursor 只决定从哪条
日志开始跟随。`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if follow {
				if opts.json {
					return domain.NewError(v1.CodeInvalidRequest,
						"--follow 与 --json 不能同时使用：follow 是持续的增量输出，拼不成一份 JSON")
				}
				return followLogs(cmd.Context(), opts, args[0], cursor, limit)
			}
			response, err := opts.client().Logs(cmd.Context(), args[0], cursor, limit)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(response)
			}
			printLogEntries(response.Items)
			return nil
		},
	}

	cmd.Flags().Int64Var(&cursor, "cursor", 0, "只返回 id 大于该游标的日志条目（--follow 下作为跟随的起点）")
	cmd.Flags().IntVar(&limit, "limit", 0, "返回条数上限")
	cmd.Flags().BoolVar(&follow, "follow", false, "持续输出新日志，操作到终态后退出（退出码随终态）")

	return cmd
}

// followInterval 是 --follow 的轮询间隔（迭代 6 规格 D5：固定 1 秒，不做退避）。
const followInterval = time.Second

// followLogs 用客户端轮询实现流式跟随（迭代 6 规格 D5）：不改协议也不加依赖——
// 服务端推送要么改协议要么引依赖，而客户端本来就靠轮询拿操作状态，这只是同一件事的延伸。
//
// 每一轮先拉日志、再查操作状态。终态与最后一条日志（operation finished）在服务端是
// 同一个事务落库的，所以「看到终态之后再补拉一轮」就不会漏日志；反过来的顺序
// （先查状态再拉日志）在两者之间落下的那批就要靠运气了。
func followLogs(ctx context.Context, opts *rootOptions, id string, cursor int64, limit int) error {
	// Ctrl-C 只是「不看了」：操作在 opsd 那边照常跑完。把 SIGINT 变成 ctx 取消，
	// 循环在下一轮等待处干净退出，而不是带着一堆 gore 被默认行为砸掉。
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	client := opts.client()
	for {
		response, err := client.Logs(ctx, id, cursor, limit)
		if err != nil {
			return err
		}
		printLogEntries(response.Items)
		cursor = response.NextCursor

		operation, err := client.GetOperation(ctx, id)
		if err != nil {
			return err
		}
		if domain.Status(operation.Status).Terminal() {
			// 退出前补拉一轮：状态翻转前一刻提交的日志也在这一轮里。
			final, err := client.Logs(ctx, id, cursor, limit)
			if err != nil {
				return err
			}
			printLogEntries(final.Items)
			return followExitStatus(operation)
		}

		// 刚才那一批还有条目，说明可能还没追上实时：立刻再来一轮。
		// 1 秒间隔是「没有新东西时」的轮询节奏，不该给追赶中的循环也垫上延迟。
		if len(response.Items) > 0 {
			continue
		}

		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr,
				"已停止跟随；操作 %s 仍在后台继续，可用 opsctl operation get %s 查看结果\n", id, id)
			return nil
		case <-time.After(followInterval):
		}
	}
}

// followExitStatus 把操作终态映射成本命令的退出结果：成功为 nil（退出码 0）；
// 失败与取消沿用 api/v1 的退出码表按操作错误码映射——错误以 CodedError 冒泡回
// main 的统一出口，文本里自然带着码（迭代 6 规格 D4 的同一形状）。
func followExitStatus(op *v1.Operation) error {
	switch domain.Status(op.Status) {
	case domain.StatusSucceeded:
		return nil
	case domain.StatusCancelled:
		code := v1.ErrorCode(op.ErrorCode)
		if code == "" {
			code = v1.CodeExecCancelled
		}
		return domain.NewError(code, "操作 %s 已取消", op.ID)
	case domain.StatusFailed, domain.StatusRolledBack:
		code := v1.ErrorCode(op.ErrorCode)
		if code == "" {
			code = v1.CodeInternal
		}
		if op.ErrorMessage != "" {
			return domain.NewError(code, "操作 %s 以 %s 结束：%s", op.ID, op.Status, op.ErrorMessage)
		}
		return domain.NewError(code, "操作 %s 以 %s 结束", op.ID, op.Status)
	default:
		return domain.NewError(v1.CodeInternal, "操作 %s 的终态 %s 无法映射退出码", op.ID, op.Status)
	}
}

// printLogEntries 按人类可读格式打印一批日志条目；字段名与 api/v1 保持一致
// （AGENTS.md 的语言约定），人类可读与 --json 两种输出可以逐字段对照。
func printLogEntries(items []v1.LogEntry) {
	for _, entry := range items {
		fmt.Printf("%s  %-5s %s", entry.Time.Format(time.RFC3339), entry.Level, entry.Message)
		for key, value := range entry.Fields {
			fmt.Printf("  %s=%s", key, value)
		}
		fmt.Println()
	}
}

// wholeSeconds 把时长转成整秒。API 的重试字段以秒为单位，非整秒的输入会被静默截断，
// 那等于悄悄改了用户给的退避时间——所以直接拒绝，而不是替用户取整。
func wholeSeconds(d time.Duration, flag string) (int, error) {
	if d < 0 {
		return 0, domain.NewError(v1.CodeRetryPolicyInvalid, "--%s 不能为负数", flag)
	}
	if d%time.Second != 0 {
		return 0, domain.NewError(v1.CodeRetryPolicyInvalid, "--%s 必须是整秒（got %s）", flag, d)
	}
	return int(d / time.Second), nil
}

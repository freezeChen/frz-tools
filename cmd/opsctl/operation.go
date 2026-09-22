package main

import (
	"encoding/json"
	"fmt"
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
		kind     string
		resource string
		idemKey  string
		dryRun   bool
		secrets  []string
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

			operation, created, err := opts.client().CreateOperation(cmd.Context(), v1.CreateOperationRequest{
				Kind:           kind,
				Resource:       resource,
				DryRun:         dryRun,
				Spec:           spec,
				IdempotencyKey: idemKey,
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
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if follow {
				return domain.NewError(v1.CodeInvalidRequest, "--follow 流式日志本迭代未实现，请改用 --cursor 轮询")
			}
			response, err := opts.client().Logs(cmd.Context(), args[0], cursor, limit)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(response)
			}
			for _, entry := range response.Items {
				fmt.Printf("%s  %-5s %s", entry.Time.Format(time.RFC3339), entry.Level, entry.Message)
				for key, value := range entry.Fields {
					fmt.Printf("  %s=%s", key, value)
				}
				fmt.Println()
			}
			return nil
		},
	}

	cmd.Flags().Int64Var(&cursor, "cursor", 0, "只返回 id 大于该游标的日志条目")
	cmd.Flags().IntVar(&limit, "limit", 0, "返回条数上限")
	cmd.Flags().BoolVar(&follow, "follow", false, "流式输出，本迭代保留参数但尚未实现")

	return cmd
}

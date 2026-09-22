package main

import (
	"encoding/json"
	"fmt"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/client"
	"github.com/freezeChen/frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

func newScheduleCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "管理定时计划：到点由 opsd 自动创建并执行操作",
	}
	cmd.AddCommand(
		newScheduleCreateCommand(opts),
		newScheduleListCommand(opts),
		newScheduleInspectCommand(opts),
		newScheduleRunsCommand(opts),
		newScheduleEnableCommand(opts),
		newScheduleDisableCommand(opts),
		newScheduleDeleteCommand(opts),
	)
	return cmd
}

func newScheduleCreateCommand(opts *rootOptions) *cobra.Command {
	var (
		name       string
		resource   string
		cronExpr   string
		interval   time.Duration
		timezone   string
		policy     string
		createdBy  string
		secretEnvs []string
	)

	cmd := &cobra.Command{
		Use:   "create --name <n> --resource <r> (--cron <表达式> | --interval <时长>) -- <argv...>",
		Short: "创建定时计划",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() == -1 {
				return domain.NewError(v1.CodeInvalidRequest, "请把命令 argv 写在 -- 之后，例如：-- /usr/bin/true")
			}
			if len(args) == 0 {
				return domain.NewError(v1.CodeInvalidRequest, "-- 之后的 argv 不能为空")
			}
			if (cronExpr == "") == (interval == 0) {
				return domain.NewError(v1.CodeScheduleInvalid, "必须且只能提供 --cron 或 --interval 之一")
			}

			// 与 operation submit 共用同一套 spec 构造，两个入口的语义不会漂移。
			secretEnvironment, err := parseSecretRefs(secretEnvs)
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

			kind := string(domain.ScheduleKindCron)
			if interval > 0 {
				kind = string(domain.ScheduleKindInterval)
			}

			schedule, err := opts.client().CreateSchedule(cmd.Context(), client.CreateScheduleInput{
				Name:            name,
				Kind:            kind,
				Cron:            cronExpr,
				Interval:        interval,
				Timezone:        timezone,
				Resource:        resource,
				Spec:            spec,
				MissedRunPolicy: policy,
				CreatedBy:       createdBy,
			})
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(schedule)
			}
			printSchedule(schedule)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "计划名称，唯一（必填）")
	cmd.Flags().StringVar(&resource, "resource", "", "触发时创建的 Operation 使用的 resource（必填）")
	cmd.Flags().StringVar(&cronExpr, "cron", "", "cron 表达式，标准 5 字段：分 时 日 月 周")
	cmd.Flags().DurationVar(&interval, "interval", 0, "固定间隔，例如 30m；最小 1m")
	cmd.Flags().StringVar(&timezone, "timezone", "UTC", "IANA 时区，例如 Asia/Shanghai")
	cmd.Flags().StringVar(&policy, "missed-run-policy", string(domain.MissedRunSkip),
		"错过执行策略：skip 丢弃、runOnce 只补跑最近一次")
	cmd.Flags().StringVar(&createdBy, "created-by", "", "调用方标识")
	cmd.Flags().StringArrayVar(&secretEnvs, "secret-env", nil,
		"把凭据注入命令环境，格式 VAR=kind:name（kind 为 env 或 file）")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("resource")

	return cmd
}

func newScheduleListCommand(opts *rootOptions) *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出计划",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := opts.client().ListSchedules(cmd.Context(), limit)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(response)
			}
			for i := range response.Items {
				printSchedule(&response.Items[i])
				fmt.Println()
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "返回条数上限")
	return cmd
}

func newScheduleInspectCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name-or-id>",
		Short: "查看单个计划",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			schedule, err := opts.client().GetSchedule(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(schedule)
			}
			printSchedule(schedule)
			return nil
		},
	}
}

func newScheduleRunsCommand(opts *rootOptions) *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "runs <name-or-id>",
		Short: "查看计划的运行历史",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := opts.client().ScheduleRuns(cmd.Context(), args[0], limit)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(response)
			}
			for _, run := range response.Items {
				line := fmt.Sprintf("%s  %-10s %s", run.ScheduledFor.Format(time.RFC3339), run.Result, run.ID)
				if run.OperationID != "" {
					line += "  operation=" + run.OperationID
				}
				if run.ErrorCode != "" {
					line += "  error=" + run.ErrorCode
				}
				fmt.Println(line)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "返回条数上限")
	return cmd
}

func newScheduleEnableCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "enable <name-or-id>",
		Short: "启用计划；下一次触发时间从当前时刻重新计算",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			schedule, err := opts.client().EnableSchedule(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(schedule)
			}
			printSchedule(schedule)
			return nil
		},
	}
}

func newScheduleDisableCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "disable <name-or-id>",
		Short: "停用计划；已在运行的 Operation 不受影响",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			schedule, err := opts.client().DisableSchedule(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(schedule)
			}
			printSchedule(schedule)
			return nil
		},
	}
}

func newScheduleDeleteCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name-or-id>",
		Short: "删除计划；启用中的计划必须先停用",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := opts.client().DeleteSchedule(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("已删除计划 %s\n", args[0])
			return nil
		},
	}
}

func printSchedule(schedule *v1.Schedule) {
	fmt.Printf("id:        %s\n", schedule.ID)
	fmt.Printf("name:      %s\n", schedule.Name)
	fmt.Printf("enabled:   %t\n", schedule.Enabled)
	fmt.Printf("kind:      %s\n", schedule.Kind)
	if schedule.Cron != "" {
		fmt.Printf("cron:      %s\n", schedule.Cron)
	}
	if schedule.IntervalSeconds > 0 {
		fmt.Printf("interval:  %s\n", time.Duration(schedule.IntervalSeconds)*time.Second)
	}
	fmt.Printf("timezone:  %s\n", schedule.Timezone)
	fmt.Printf("resource:  %s\n", schedule.Resource)
	fmt.Printf("policy:    %s\n", schedule.MissedRunPolicy)
	if schedule.NextRunAt != nil {
		fmt.Printf("nextRunAt: %s\n", schedule.NextRunAt.Format(time.RFC3339))
	}
	if schedule.LastResult != "" {
		fmt.Printf("lastRun:   %s (%s)\n", schedule.LastResult, schedule.LastRunAt.Format(time.RFC3339))
	}
}

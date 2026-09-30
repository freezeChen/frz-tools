package main

import (
	"fmt"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

// statusPerPage 是 status 拉取的最近操作条数，与服务端列表端点的默认值一致。
const statusPerPage = 10

// statusOutput 是 `opsctl status --json` 的输出。health 与 operations 用的都是
// api/v1 的原类型，字段与 API 逐字段对应（见 AGENTS.md 的语言约定），
// 不在客户端另造一套形状。
type statusOutput struct {
	Health     *v1.HealthResponse `json:"health"`
	Operations []v1.Operation     `json:"operations"`
}

// newStatusCommand 一屏回答「这台 opsd 现在怎么样」（迭代 6 规格 D3）：
// 健康四项 + 最近的操作列表。没有任何写操作、不做汇总推断；
// 未完成的操作（pending / running）在人类可读输出里显式标出。
func newStatusCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "一屏查看 opsd 的健康与最近操作",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			health, err := opts.client().Health(cmd.Context())
			if err != nil {
				return err
			}
			list, err := opts.client().ListOperations(cmd.Context(), statusPerPage, "")
			if err != nil {
				return err
			}
			operations := list.Operations
			if operations == nil {
				// 空列表输出 [] 而不是 null：调用方不必为「没有操作」写特例。
				operations = []v1.Operation{}
			}

			if opts.json {
				return opts.printJSON(statusOutput{Health: health, Operations: operations})
			}
			printStatusSummary(health, operations)
			return nil
		},
	}
}

func printStatusSummary(health *v1.HealthResponse, operations []v1.Operation) {
	fmt.Printf("daemon:   %s\n", health.Daemon)
	fmt.Printf("database: %s\n", health.Database)
	fmt.Printf("workers:  %d\n", health.Workers)
	fmt.Printf("uptime:   %s\n", time.Duration(health.UptimeMS)*time.Millisecond)

	if len(operations) == 0 {
		fmt.Println("\n最近的操作：无")
		return
	}
	fmt.Printf("\n最近的操作（最多 %d 条，按创建时间倒序）：\n", statusPerPage)
	for _, op := range operations {
		fmt.Printf("  %s  %s  %s  %s", op.ID, op.Status, op.Kind, op.Resource)
		// 未完成的操作显式标出：一屏扫过去，先看到的应该是「还有什么没完」。
		switch domain.Status(op.Status) {
		case domain.StatusPending, domain.StatusRunning:
			fmt.Print("  （进行中）")
		}
		fmt.Println()
	}
}

package main

import (
	"fmt"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"

	"github.com/spf13/cobra"
)

func newAppSlotCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "slot",
		Short: "查看蓝绿应用的槽位与切换时间线",
		Long: "蓝绿应用有两个槽位（blue / green），同一时刻只有一个在接流量。\n\n" +
			"list 把**两类事实并排**报出来：库里的运营记录，以及线上实际（Nginx 的 upstream " +
			"指向哪一侧、systemd 里进程在不在跑）。两者不一致时会标出来——那本身就是要看的信息。\n\n" +
			"history 是切换时间线：谁在什么时候切到过哪个版本。\n\n" +
			"单槽应用没有槽位可看，这两条命令会明确报错。",
	}
	cmd.AddCommand(newAppSlotListCommand(opts), newAppSlotHistoryCommand(opts))
	return cmd
}

func newAppSlotListCommand(opts *rootOptions) *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "list --app <name>",
		Short: "列出两个槽位的状态、版本、端口与是否在接流量",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireApp(app); err != nil {
				return err
			}
			result, err := opts.client().ApplicationSlots(cmd.Context(), app)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(result)
			}
			printSlotList(result)
			return nil
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "应用名称或 ID")
	return cmd
}

func newAppSlotHistoryCommand(opts *rootOptions) *cobra.Command {
	var (
		app   string
		limit int
	)

	cmd := &cobra.Command{
		Use:   "history --app <name>",
		Short: "查看切换时间线（新的在前）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireApp(app); err != nil {
				return err
			}
			result, err := opts.client().SlotHistory(cmd.Context(), app, limit)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(result)
			}
			printSlotHistory(result)
			return nil
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "应用名称或 ID")
	cmd.Flags().IntVar(&limit, "limit", 20, "最多显示多少条（服务端上限 50）")
	return cmd
}

func printSlotList(result *v1.SlotListResponse) {
	printOnlineTruth(result.ServingSlot, result.RecordedSlot, result.OnlineKnown)
	if result.Detail != "" {
		fmt.Printf("说明:     %s\n", result.Detail)
	}
	fmt.Println()

	for _, slot := range result.Items {
		fmt.Printf("槽位 %s\n", slot.Slot)
		fmt.Printf("  接流量: %s\n", yesNo(slot.Serving))
		fmt.Printf("  状态:   %s\n", orDash(slot.State))
		fmt.Printf("  版本:   %s\n", slotVersion(slot))
		fmt.Printf("  端口:   %s\n", joinInts(slot.Ports))
		fmt.Printf("  unit:   %s\n", orDash(slot.UnitName))
		fmt.Printf("  进程:   %s\n", processLine(slot))
		fmt.Printf("  就绪:   %s\n", readyLine(slot))
		if slot.SwitchedAt != nil {
			fmt.Printf("  切流于: %s\n", slot.SwitchedAt.Format(time.RFC3339))
		}
		if slot.Detail != "" {
			fmt.Printf("  说明:   %s\n", slot.Detail)
		}
	}
}

// printOnlineTruth 打头先把「线上事实」与「库里的记录」分开说清楚。
//
// 它们不是同一个东西——**库里的那份是镜像，会过期**（迭代 4 规格 D2）。不说清楚，
// 读的人会把库里的记录当成事实，而这正是要避免的误读。
func printOnlineTruth(serving, recorded string, onlineKnown bool) {
	if !onlineKnown {
		fmt.Println("线上事实: 读不到（本部署未装配 Nginx 适配器）")
		fmt.Printf("库里记录: serving_slot = %s\n", orNone(recorded))
		return
	}
	fmt.Printf("线上事实: Nginx 的 upstream 指向 %s\n", orNone(serving))
	fmt.Printf("库里记录: serving_slot = %s\n", orNone(recorded))
}

func printSlotHistory(result *v1.SlotHistoryResponse) {
	if len(result.Items) == 0 {
		fmt.Printf("应用 %s 还没有槽位时间线（还没部署过，或还没发生过切流）\n", result.Application)
		return
	}
	fmt.Println("时间（新的在前）               事件       槽位   版本     说明")
	for _, event := range result.Items {
		line := fmt.Sprintf("%s  %-9s  %-5s  %-8s %s",
			event.At.Format(time.RFC3339), event.Kind, event.Slot,
			orDash(event.Version), event.Detail)
		if event.OperationID != "" {
			line += "（操作 " + event.OperationID + "）"
		}
		fmt.Println(line)
	}
}

func slotVersion(slot v1.SlotStatus) string {
	if slot.Version == "" && slot.ReleaseID == "" {
		return "—（还没部署过）"
	}
	if slot.ReleaseID == "" {
		return slot.Version
	}
	return fmt.Sprintf("%s（release %s）", orDash(slot.Version), slot.ReleaseID)
}

func processLine(slot v1.SlotStatus) string {
	if !slot.ProcessKnown {
		return "读不到（本部署未装配运行时适配器）"
	}
	return orDash(string(slot.ProcessState))
}

// readyLine 区分「没探」与「探了不健康」：前者是 null（进程没在跑，探它没有意义），
// 后者是 false。把两者都显示成「否」会让「停着的槽位」看起来像「起来但没好」。
func readyLine(slot v1.SlotStatus) string {
	if slot.Ready == nil {
		return "—（进程没在跑，没探）"
	}
	if *slot.Ready {
		if slot.ReadyDetail != "" {
			return "是（" + slot.ReadyDetail + "）"
		}
		return "是"
	}
	return "否" + detailSuffix(slot.ReadyDetail)
}

func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return "（" + detail + "）"
}

func yesNo(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

func orDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

// orNone 把空槽位说成人话：空字符串与「还没部署过」是同一件事，但前者看着像漏了字段。
func orNone(value string) string {
	if value == "" {
		return "（还没部署过）"
	}
	return value
}

func joinInts(values []int) string {
	if len(values) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, fmt.Sprint(value))
	}
	return strings.Join(parts, ", ")
}

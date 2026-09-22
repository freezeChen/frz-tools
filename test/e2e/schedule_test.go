package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1 "frz-tools/api/v1"
)

func decodeSchedule(t *testing.T, raw string) v1.Schedule {
	t.Helper()
	var schedule v1.Schedule
	if err := json.Unmarshal([]byte(raw), &schedule); err != nil {
		t.Fatalf("cannot decode schedule from %q: %v", raw, err)
	}
	return schedule
}

func createSchedule(t *testing.T, socket, name, resource string, extra ...string) v1.Schedule {
	t.Helper()

	args := append([]string{
		"schedule", "create",
		"--name", name,
		"--resource", resource,
		"--interval", "5m",
		"--json",
	}, extra...)
	args = append(args, "--", "/usr/bin/true")

	stdout, _, err := runOpsctl(t, socket, args...)
	if err != nil {
		t.Fatalf("schedule create: %v", err)
	}
	return decodeSchedule(t, stdout)
}

func TestScheduleLifecycleThroughCLI(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	schedule := createSchedule(t, d.socket, "nightly", "sched-nightly")
	if schedule.NextRunAt == nil {
		t.Fatal("创建后应当带回下一次触发时间")
	}
	if schedule.Kind != "interval" || schedule.IntervalSeconds != 300 {
		t.Fatalf("unexpected schedule: %+v", schedule)
	}

	if _, _, err := runOpsctl(t, d.socket, "schedule", "inspect", "nightly"); err != nil {
		t.Fatalf("按名称查询失败：%v", err)
	}
	list, _, err := runOpsctl(t, d.socket, "schedule", "list", "--json")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list, schedule.ID) {
		t.Fatalf("列表中缺少计划：%s", list)
	}

	runs, _, err := runOpsctl(t, d.socket, "schedule", "runs", "nightly", "--json")
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if !strings.Contains(runs, `"items": []`) && !strings.Contains(runs, `"items":[]`) {
		t.Fatalf("尚未触发时不应有记录：%s", runs)
	}

	// 启用中的计划拒绝删除 → SCHEDULE_ENABLED，退出码 15。
	_, code, err := runOpsctl(t, d.socket, "schedule", "delete", "nightly")
	if err == nil {
		t.Fatal("启用中的计划必须拒绝删除")
	}
	if code != 15 {
		t.Fatalf("SCHEDULE_ENABLED 必须退出 15，got %d (%v)", code, err)
	}

	if _, _, err := runOpsctl(t, d.socket, "schedule", "disable", "nightly"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, _, err := runOpsctl(t, d.socket, "schedule", "delete", "nightly"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, code, err := runOpsctl(t, d.socket, "schedule", "inspect", "nightly"); err == nil {
		t.Fatal("删除后必须查不到")
	} else if code != 2 {
		t.Fatalf("SCHEDULE_NOT_FOUND 必须退出 2，got %d", code)
	}
}

func TestScheduleRejectsInvalidCron(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	_, code, err := runOpsctl(t, d.socket, "schedule", "create",
		"--name", "bad", "--resource", "r", "--cron", "not a cron", "--", "/usr/bin/true")
	if err == nil {
		t.Fatal("非法 cron 必须失败")
	}
	if code != 16 {
		t.Fatalf("SCHEDULE_INVALID 必须退出 16，got %d (%v)", code, err)
	}
}

// 真实触发：建一条每分钟的计划，等它到点，验证 Operation 被创建并执行完成。
// 这是 1b 的核心行为，必须用真实进程与真实时钟验证，不能用 fake clock 替代。
func TestScheduleActuallyFiresAndRunsCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("需要等待下一个整分钟，-short 模式下跳过")
	}

	d := newDaemon(t)
	d.start(t)

	// cron 的最小粒度是 1 分钟且触发点固定在整分钟，所以这个测试必然要等到下一个
	// 整分钟，平均约 30 秒、最多约 60 秒。这是 cron 语义决定的成本，无法规避；
	// -short 模式下跳过。
	const resource = "sched-fire"
	stdout, _, err := runOpsctl(t, d.socket, "schedule", "create",
		"--name", "every-minute",
		"--resource", resource,
		"--cron", "* * * * *",
		"--json",
		"--", "/bin/echo", "scheduled-run")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	schedule := decodeSchedule(t, stdout)

	// 等这次触发被派发：调度器到点后创建 Operation 并写一条 dispatched 记录。
	deadline := time.Now().Add(90 * time.Second)
	var operationID string
	for time.Now().Before(deadline) {
		raw, _, err := runOpsctl(t, d.socket, "schedule", "runs", schedule.ID, "--json")
		if err == nil {
			var runs v1.ScheduleRunListResponse
			if err := json.Unmarshal([]byte(raw), &runs); err == nil {
				for _, run := range runs.Items {
					if run.Result == "dispatched" && run.OperationID != "" {
						operationID = run.OperationID
					}
				}
			}
		}
		if operationID != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if operationID == "" {
		t.Fatalf("计划在 90 秒内没有被触发（计划 %s）", schedule.ID)
	}

	// 触发出来的 Operation 必须真的被执行，且日志里能看到命令输出。
	final := waitForStatus(t, d.socket, operationID, "succeeded", "failed", "cancelled")
	if final.Status != "succeeded" {
		t.Fatalf("计划触发的操作应当成功，got %s (%s)", final.Status, final.ErrorCode)
	}

	logs, _, err := runOpsctl(t, d.socket, "operation", "logs", operationID)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(logs, "scheduled-run") {
		t.Fatalf("日志应当包含计划命令的输出：%s", logs)
	}
}

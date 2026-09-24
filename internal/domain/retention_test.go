package domain

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// 保留策略是这一块里最容易「看起来对」的部分：它错了只会表现为「某天少了一份备份」，
// 而那要等到真要恢复时才发现。因此这里是表驱动的，边界逐条钉住。

func mkBackup(id string, finishedAt *time.Time, status BackupStatus, verified *bool) Backup {
	return Backup{
		ID:         id,
		Status:     status,
		FinishedAt: finishedAt,
		VerifiedOK: verified,
	}
}

func daysBefore(now time.Time, days float64) *time.Time {
	at := now.Add(-time.Duration(days * 24 * float64(time.Hour)))
	return &at
}

func boolPtr(value bool) *bool { return &value }

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func TestPlanRetention(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		retention  BackupRetention
		backups    []Backup
		wantKeep   []string
		wantDelete []string
	}{
		{
			// keepLast 就是「最近 N 份」，与完成时刻的绝对年龄无关。
			name:      "keepLast 只留最近 N 份",
			retention: BackupRetention{KeepLast: 3},
			backups: []Backup{
				mkBackup("b5", daysBefore(now, 1), BackupSucceeded, nil),
				mkBackup("b4", daysBefore(now, 2), BackupSucceeded, nil),
				mkBackup("b3", daysBefore(now, 3), BackupSucceeded, nil),
				mkBackup("b2", daysBefore(now, 4), BackupSucceeded, nil),
				mkBackup("b1", daysBefore(now, 5), BackupSucceeded, nil),
			},
			wantKeep:   []string{"b3", "b4", "b5"},
			wantDelete: []string{"b1", "b2"},
		},
		{
			// 并集：keepLast 该保的，哪怕它已经比 keepDays 老得多也得保。
			name:      "keepLast 与 keepDays 取并集",
			retention: BackupRetention{KeepLast: 2, KeepDays: 3},
			backups: []Backup{
				mkBackup("b4", daysBefore(now, 100), BackupSucceeded, nil),
				mkBackup("b3", daysBefore(now, 101), BackupSucceeded, nil),
				mkBackup("b2", daysBefore(now, 102), BackupSucceeded, nil),
				mkBackup("b1", daysBefore(now, 103), BackupSucceeded, nil),
			},
			wantKeep:   []string{"b3", "b4"},
			wantDelete: []string{"b1", "b2"},
		},
		{
			// 反过来也要成立：keepDays 该保的，哪怕它在 keepLast 的名额之外。
			name:      "keepDays 保下 keepLast 名额之外那些",
			retention: BackupRetention{KeepLast: 1, KeepDays: 5},
			backups: []Backup{
				mkBackup("b4", daysBefore(now, 1), BackupSucceeded, nil),
				mkBackup("b3", daysBefore(now, 2), BackupSucceeded, nil),
				mkBackup("b2", daysBefore(now, 4.9), BackupSucceeded, nil),
				mkBackup("b1", daysBefore(now, 30), BackupSucceeded, nil),
			},
			wantKeep:   []string{"b2", "b3", "b4"},
			wantDelete: []string{"b1"},
		},
		{
			// 边界：恰好落在 cutoff 上算「还在窗口内」（用 !Before 而不是 After）。
			name:      "恰好落在 keepDays 边界上的保留",
			retention: BackupRetention{KeepDays: 7},
			backups: []Backup{
				mkBackup("edge", daysBefore(now, 7), BackupSucceeded, nil),
				mkBackup("just-out", daysBefore(now, 7.001), BackupSucceeded, nil),
			},
			wantKeep:   []string{"edge"},
			wantDelete: []string{"just-out"},
		},
		{
			// 「不参与计算」不只是「不删」：它也不能**占**keepLast 的名额，
			// 否则一次失败的备份会把一份真备份挤掉。
			name:      "非 Usable 的行不占 keepLast 名额",
			retention: BackupRetention{KeepLast: 2},
			backups: []Backup{
				mkBackup("failed-newest", daysBefore(now, 0.1), BackupFailed, nil),
				mkBackup("running", daysBefore(now, 0.2), BackupRunning, nil),
				mkBackup("verified-bad", daysBefore(now, 0.3), BackupSucceeded, boolPtr(false)),
				mkBackup("b2", daysBefore(now, 1), BackupSucceeded, nil),
				mkBackup("b1", daysBefore(now, 2), BackupSucceeded, nil),
			},
			wantKeep:   []string{"b1", "b2"},
			wantDelete: []string{}, // 非 Usable 的三行两边都不在
		},
		{
			// 校验失败的备份**既不参与计算、也不被删**（D9）：它是「这份备份有问题」的
			// 证据，静默删掉证据比留着垃圾坏得多。
			name:      "校验失败的备份既不参与也不被删",
			retention: BackupRetention{KeepLast: 1, KeepDays: 1},
			backups: []Backup{
				mkBackup("bad", daysBefore(now, 400), BackupSucceeded, boolPtr(false)),
				mkBackup("good", daysBefore(now, 1), BackupSucceeded, nil),
			},
			wantKeep:   []string{"good"},
			wantDelete: []string{},
		},
		{
			name:      "全部非 Usable 时什么都不删",
			retention: BackupRetention{KeepLast: 1},
			backups: []Backup{
				mkBackup("a", daysBefore(now, 1), BackupPending, nil),
				mkBackup("b", daysBefore(now, 2), BackupRunning, nil),
				mkBackup("c", daysBefore(now, 3), BackupFailed, nil),
			},
			wantKeep:   []string{},
			wantDelete: []string{},
		},
		{
			// 校验通过的行照常参与（VerifiedOK 为 true 与 nil 都是「没校验失败」）。
			name:      "校验通过的备份照常参与",
			retention: BackupRetention{KeepLast: 1},
			backups: []Backup{
				mkBackup("b2", daysBefore(now, 1), BackupSucceeded, boolPtr(true)),
				mkBackup("b1", daysBefore(now, 2), BackupSucceeded, nil),
			},
			wantKeep:   []string{"b2"},
			wantDelete: []string{"b1"},
		},
		{
			// succeeded 却没有完成时刻：数据不自洽，哪一边错都别往删除那边错。
			name:      "缺完成时刻的行被保守地留下",
			retention: BackupRetention{KeepLast: 1, KeepDays: 1},
			backups: []Backup{
				mkBackup("no-time", nil, BackupSucceeded, nil),
				mkBackup("b1", daysBefore(now, 1), BackupSucceeded, nil),
			},
			wantKeep:   []string{"b1", "no-time"},
			wantDelete: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := &BackupPolicy{Retention: tc.retention}
			plan := PlanRetention(policy, tc.backups, now)

			var kept []string
			for id := range plan.Keep {
				kept = append(kept, id)
			}
			if got, want := sortedCopy(kept), sortedCopy(tc.wantKeep); !equalStrings(got, want) {
				t.Fatalf("保留集合不符：want %v, got %v", want, got)
			}
			if got, want := sortedCopy(plan.Delete), sortedCopy(tc.wantDelete); !equalStrings(got, want) {
				t.Fatalf("删除集合不符：want %v, got %v", want, got)
			}
			if len(plan.Ignored) != 0 {
				t.Fatalf("没声明 gfs 时不该有 Ignored：%v", plan.Ignored)
			}
		})
	}
}

// 「保留」与「删除」必须覆盖且**只**覆盖 Usable 的行：漏一个会让该删的永远留着，
// 多一个会把不该删的删掉——后者是数据丢失。
func TestPlanRetentionPartitionsUsableBackups(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	backups := []Backup{
		mkBackup("u1", daysBefore(now, 1), BackupSucceeded, nil),
		mkBackup("u2", daysBefore(now, 2), BackupSucceeded, boolPtr(true)),
		mkBackup("bad", daysBefore(now, 3), BackupSucceeded, boolPtr(false)),
		mkBackup("failed", daysBefore(now, 4), BackupFailed, nil),
		mkBackup("pending", daysBefore(now, 5), BackupPending, nil),
		mkBackup("running", daysBefore(now, 6), BackupRunning, nil),
		mkBackup("pruned", daysBefore(now, 7), BackupPruned, nil),
	}
	plan := PlanRetention(&BackupPolicy{Retention: BackupRetention{KeepLast: 1}}, backups, now)

	covered := map[string]bool{}
	for id := range plan.Keep {
		covered[id] = true
	}
	for _, id := range plan.Delete {
		if covered[id] {
			t.Fatalf("%s 同时出现在保留与删除里", id)
		}
		covered[id] = true
	}
	// 覆盖性：每一个 Usable 的行都必须**恰好**落进保留或删除之一。
	for _, id := range []string{"u1", "u2"} {
		if !covered[id] {
			t.Fatalf("Usable 的 %s 既没被保留也没被删除", id)
		}
	}
	// 而具体保哪一份由 keepLast 定：只要 1 份，就是最新的 u1。
	if !plan.Keep["u1"] {
		t.Fatalf("keepLast=1 应当保下最新的 u1，got %v", planKeep(plan))
	}
	if plan.Keep["u2"] {
		t.Fatalf("keepLast=1 不该保下第二新的 u2，got %v", planKeep(plan))
	}
	for _, id := range []string{"bad", "failed", "pending", "running", "pruned"} {
		if covered[id] {
			t.Fatalf("非 Usable 的 %s 不该进入保留或删除集合", id)
		}
	}
}

// prune 必须可重放：同一批数据跑两次得删同一批。没有决胜键时，同一完成时刻的几份
// 备份在两次运行里可能排出不同顺序，「最近 N 份」就指向了不同的一组。
func TestPlanRetentionIsDeterministic(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	sameMoment := daysBefore(now, 1)
	backups := []Backup{
		mkBackup("c", sameMoment, BackupSucceeded, nil),
		mkBackup("a", sameMoment, BackupSucceeded, nil),
		mkBackup("b", sameMoment, BackupSucceeded, nil),
	}

	first := PlanRetention(&BackupPolicy{Retention: BackupRetention{KeepLast: 1}}, backups, now)
	second := PlanRetention(&BackupPolicy{Retention: BackupRetention{KeepLast: 1}}, backups, now)

	if !equalStrings(sortedCopy(planDelete(first)), sortedCopy(planDelete(second))) {
		t.Fatalf("同一批数据两次计算结果不同：%v vs %v", first.Delete, second.Delete)
	}
	if len(first.Delete) != 2 {
		t.Fatalf("keepLast=1、三份同一时刻：应当删 2 份，got %v", first.Delete)
	}
	// 决胜键是 ID 倒序，因此留下的是 c。
	if !first.Keep["c"] {
		t.Fatalf("同一完成时刻应当按 ID 倒序决胜（留下 c），got %v", planKeep(first))
	}

	// 打乱输入顺序不该改变结果：调用方拿到的顺序不该参与决定。
	shuffled := []Backup{backups[1], backups[2], backups[0]}
	third := PlanRetention(&BackupPolicy{Retention: BackupRetention{KeepLast: 1}}, shuffled, now)
	if !equalStrings(sortedCopy(planDelete(third)), sortedCopy(planDelete(first))) {
		t.Fatalf("输入顺序影响了结果：%v vs %v", third.Delete, first.Delete)
	}
}

func TestPlanRetentionReportsIgnoredGFS(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	policy := &BackupPolicy{
		Retention: BackupRetention{
			KeepLast: 1,
			GFS:      BackupGFS{Daily: 7, Weekly: 4, Monthly: 6},
		},
	}
	plan := PlanRetention(policy, []Backup{mkBackup("b1", daysBefore(now, 1), BackupSucceeded, nil)}, now)

	// 旧库里可能留着带 gfs 的策略（提交期已经拦住新的），prune 必须把「没实现它」
	// 这件事带到调用方看得见的地方，而不是悄悄忽略。
	if len(plan.Ignored) != 1 || plan.Ignored[0] != "gfs" {
		t.Fatalf("want Ignored=[gfs], got %v", plan.Ignored)
	}
}

func TestPlanRetentionHandlesNilPolicy(t *testing.T) {
	plan := PlanRetention(nil, nil, time.Now())
	if len(plan.Keep) != 0 || len(plan.Delete) != 0 {
		t.Fatalf("nil 策略应当得到空计划，got %+v", plan)
	}
}

func TestPlanRetentionDeleteOrderIsNewestFirst(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	backups := []Backup{
		mkBackup("old", daysBefore(now, 30), BackupSucceeded, nil),
		mkBackup("mid", daysBefore(now, 20), BackupSucceeded, nil),
		mkBackup("new", daysBefore(now, 10), BackupSucceeded, nil),
	}
	plan := PlanRetention(&BackupPolicy{Retention: BackupRetention{KeepLast: 1}}, backups, now)
	if got := strings.Join(plan.Delete, ","); got != "mid,old" {
		t.Fatalf("删除顺序应当是「新的在前」，got %s", got)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func planKeep(plan RetentionPlan) []string {
	out := make([]string, 0, len(plan.Keep))
	for id := range plan.Keep {
		out = append(out, id)
	}
	return sortedCopy(out)
}

func planDelete(plan RetentionPlan) []string { return plan.Delete }

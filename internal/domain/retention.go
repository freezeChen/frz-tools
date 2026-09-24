package domain

import (
	"sort"
	"time"
)

// RetentionPlan 是一次保留计算的结果。
//
// 「保留」与「删除」是两个**互补但不等价**的集合：非 Usable 的备份（未完成、失败、
// 校验失败）**两边都不在**——它们既不参与计算，也不进入清理范围（迭代 2 规格 D9）。
// 调用方对它们什么都不做，理由见 Backup.Usable 的注释。
type RetentionPlan struct {
	// Keep 是应当保留的备份 ID。
	Keep map[string]bool
	// Delete 是应当删除的备份 ID（Usable 但不被任何规则保留）。
	Delete []string
	// Ignored 是策略里声明了、而这一版**没有实现**的保留项（当前只有可能出现 "gfs"）。
	//
	// 它的存在是为了**不静默**：库里可能留着 2a/2b 时期写下的带 gfs 的策略，
	// 而 prune 这一版不看 gfs。与其悄悄忽略，不如把这件事带到调用方看得见的地方。
	Ignored []string
}

// PlanRetention 算出「哪些备份该留、哪些该删」。
//
// 判据（迭代 2 §23 的 P1）：
//
//	候选 = 该策略下 Usable() 的备份（succeeded 且校验没失败）
//	保留 = 最近 keepLast 份  ∪  完成时刻在 keepDays 之内的
//
// 两者取**并集**，各自独立成立：一条规则不该把另一条的名额挤掉。keepLast<=0 表示不按
// 份数保，keepDays<=0 表示不按天数保（领域校验保证不会两者同时为 0）。
//
// keepDays 按「24 小时 × N」算而不是自然日：它是"多久之内不删"的**时长**承诺，与 1a 制品
// GC 的 olderThan 同源。需要时区感知的日历语义的是 GFS 的日/周/月（决定 4），不是这里。
//
// 这是一个**纯函数**：只依赖策略、备份列表与「现在」。把它放在领域层并单独表驱动测试，
// 是因为保留策略是这一块里唯一容易"看起来对"的部分——它错了只会表现为"某天少了一份
// 备份"，而那要等到真要恢复时才发现。
func PlanRetention(policy *BackupPolicy, backups []Backup, now time.Time) RetentionPlan {
	plan := RetentionPlan{Keep: map[string]bool{}}
	if policy == nil {
		return plan
	}
	if policy.Retention.GFS.Declared() {
		// 本版本不实现 GFS（见 docs/plans/2026-09-24-future-iterations.md）。
		// 提交期已经把它拒了，走到这里只可能是**旧库里留下的**策略——那就必须说出来。
		plan.Ignored = append(plan.Ignored, "gfs")
	}

	usable := make([]*Backup, 0, len(backups))
	for i := range backups {
		if backups[i].Usable() {
			usable = append(usable, &backups[i])
		}
	}
	sortUsableByRecency(usable)

	cutoff := now.Add(-time.Duration(policy.Retention.KeepDays) * 24 * time.Hour)
	for i, backup := range usable {
		keep := policy.Retention.KeepLast > 0 && i < policy.Retention.KeepLast
		switch {
		case keep:
		case policy.Retention.KeepDays > 0 && backup.FinishedAt != nil &&
			!backup.FinishedAt.Before(cutoff):
			keep = true
		case backup.FinishedAt == nil:
			// succeeded 却没有完成时刻：数据不自洽，而"删掉一份我们说不清它什么时候完成的
			// 备份"是不可逆的。哪一边错都别往删除那边错——留着，让人去看。
			keep = true
		}
		if keep {
			plan.Keep[backup.ID] = true
			continue
		}
		plan.Delete = append(plan.Delete, backup.ID)
	}
	return plan
}

// sortUsableByRecency 按完成时刻**倒序**排（最新在前），同一时刻按 ID 倒序。
//
// ID 是最后的决胜键：没有它，「最近 N 份」在同一次 prune 与下一次之间可能指不同的一组，
// 而 prune 必须是可重放的——同样的数据跑两次得删同一批。
func sortUsableByRecency(backups []*Backup) {
	sort.SliceStable(backups, func(i, j int) bool {
		left, right := backups[i], backups[j]
		switch {
		case left.FinishedAt != nil && right.FinishedAt != nil:
			if !left.FinishedAt.Equal(*right.FinishedAt) {
				return left.FinishedAt.After(*right.FinishedAt)
			}
		case left.FinishedAt != nil:
			return true // 有完成时刻的排在没完成时刻的前面
		case right.FinishedAt != nil:
			return false
		}
		return left.ID > right.ID
	})
}

// Declared 报告策略里是否真的声明了 GFS 的任一项。
//
// 它只是把「模型里有没有写」这件事说出来；「这一版实不实现」是应用层的判断
// （domain 不该知道版本能力）。
func (g BackupGFS) Declared() bool {
	return g.Daily > 0 || g.Weekly > 0 || g.Monthly > 0
}

package domain

import (
	"sort"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

type Application struct {
	ID        string
	Name      string
	Labels    map[string]string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (a *Application) Validate() error {
	if strings.TrimSpace(a.Name) == "" {
		return NewError(v1.CodeInvalidRequest, "application name is required")
	}
	return nil
}

// ReleaseStatus 是一次部署的状态。
//
// 它与 Operation 的状态是**两件事**：Operation 说「这次动作跑得怎么样」，Release 说
// 「这个版本现在是什么身份」。一次成功的部署会让 Operation 变成 succeeded、让 Release 变成
// active，而**上一次** active 的那个 Release 变成 superseded——那一刻并没有任何 Operation
// 在跑。
//
// 状态转移只有这些（迭代 3 规格 §5）：
//
//	created    → deploying → active → superseded
//	                       ↘ failed
//	active     → superseded（被下一次成功部署取代）
//	非 active  → removed（被保留策略清理；current 指向的那个永远不能被删）
type ReleaseStatus string

const (
	// ReleaseCreated 表示记录已建、部署还没成功过。1a 时期写下的历史行也是这个状态
	// ——对它们来说「记录已建、部署未尝试」是准确的说法。
	ReleaseCreated ReleaseStatus = "created"
	// ReleaseDeploying 表示这一次部署正在进行。
	ReleaseDeploying ReleaseStatus = "deploying"
	// ReleaseActive 是**当前激活**：current 指针指向它。
	ReleaseActive ReleaseStatus = "active"
	// ReleaseSuperseded 是「曾经激活、被后来的版本取代」。它仍然可以回滚回去。
	ReleaseSuperseded ReleaseStatus = "superseded"
	// ReleaseFailed 是这次部署失败（进程没起来、健康没过）。失败时 current 已经切回原处，
	// 因此它的目录会被清掉。
	ReleaseFailed ReleaseStatus = "failed"
	// ReleaseRemoved 是目录已被保留策略清理，记录仍在（历史可查，但回滚不到它）。
	ReleaseRemoved ReleaseStatus = "removed"
)

// Rollbackable 报告这个版本还能不能被回滚回去。
//
// 只有「目录还在、且曾经验证过能跑」的版本才算数：active 与 superseded 都满足，
// failed（从没跑成功）与 removed（目录没了）都不满足。
func (s ReleaseStatus) Rollbackable() bool {
	return s == ReleaseActive || s == ReleaseSuperseded
}

// Release 是一次部署的记录。
type Release struct {
	ID            string
	ApplicationID string
	ArtifactID    string
	Version       string
	Labels        map[string]string
	CreatedAt     time.Time
	CreatedBy     string

	Status ReleaseStatus
	// Directory 是磁盘上这个 release 的目录（绝对路径）。它记在库里，是为了让
	// 「库里有记录、盘上没目录」这类不一致能被发现，而不是要靠拼路径去猜。
	Directory   string
	ActivatedAt *time.Time
	FinishedAt  *time.Time

	ErrorCode    string
	ErrorMessage string
}

func (r *Release) Validate() error {
	if strings.TrimSpace(r.ApplicationID) == "" {
		return NewError(v1.CodeInvalidRequest, "release.applicationId is required")
	}
	if strings.TrimSpace(r.ArtifactID) == "" {
		return NewError(v1.CodeInvalidRequest, "release.artifactId is required")
	}
	if strings.TrimSpace(r.Version) == "" {
		return NewError(v1.CodeInvalidRequest, "release.version is required")
	}
	return nil
}

// PlanReleaseRemoval 算出「哪些 release 的目录该被清掉」（迭代 3 规格 D7）。
//
// 判据：
//
//	保留 = {当前激活的那个} ∪ {最年轻的 keepLast-1 个仍带目录的版本}
//	删除 = 其余**仍带目录**的版本
//
// 三条刻意的规则：
//
//  1. **current 永远不删**，且它不占 keepLast 的名额——回滚到旧版本之后，激活的那个可能
//     比某些 superseded 的更老，把它当成"最新的一份"去数会让保留集合算错。
//  2. 只有**还带目录**的版本参与（`superseded`）：`created` / `deploying` 的行目录可能
//     还没建好，`failed` / `removed` 的目录已经没了，删它们没有意义。
//  3. 删除顺序**从老到新**：先删最没用的，中途出错时剩下的都是更有价值的那些。
//
// 它是纯函数，因此「保留集合长什么样」可以完全用表驱动测出来。而这条策略算错的后果是
// 「某个还能回滚的版本被删掉了」——只在真要回滚时才发现。
func PlanReleaseRemoval(releases []Release, keepLast int) []string {
	active := ""
	candidates := make([]Release, 0, len(releases))
	for _, release := range releases {
		if release.Status == ReleaseActive {
			active = release.ID
			continue
		}
		if release.Status == ReleaseSuperseded && release.Directory != "" {
			candidates = append(candidates, release)
		}
	}
	if keepLast <= 0 {
		// 0 在领域校验里是非法值（release.keepLast 至少 1），走到这里说明调用方没校验过。
		return nil
	}

	// 最年轻的在前：完成时刻优先，其次创建时刻，最后用 ID 决胜（保证可重放）。
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.ActivatedAt != nil && right.ActivatedAt != nil &&
			!left.ActivatedAt.Equal(*right.ActivatedAt) {
			return left.ActivatedAt.After(*right.ActivatedAt)
		}
		if left.ActivatedAt != nil && right.ActivatedAt == nil {
			return true
		}
		if left.ActivatedAt == nil && right.ActivatedAt != nil {
			return false
		}
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.After(right.CreatedAt)
		}
		return left.ID > right.ID
	})

	// keepLast 数的是**盘上有目录的版本**，其中一个是 current 自己。
	keepOthers := keepLast - 1
	if active == "" {
		// 没有 current（首次部署之前，或第一次部署失败）：keepLast 全部给 superseded。
		keepOthers = keepLast
	}
	if keepOthers < 0 {
		keepOthers = 0
	}
	if len(candidates) <= keepOthers {
		return nil
	}

	removable := candidates[keepOthers:]
	removal := make([]string, 0, len(removable))
	for i := len(removable) - 1; i >= 0; i-- { // 从老到新
		removal = append(removal, removable[i].ID)
	}
	return removal
}

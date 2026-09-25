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
// 状态转移只有这些（迭代 3 规格 §5 + 迭代 4 的 standby）：
//
//	created    → deploying → active → superseded → active（回滚：旧的又被切回来）
//	                       ↘ standby → active
//	                       ↘ failed
//	非 active  → removed（被保留策略清理；current / 槽位指针指向的那个永远不能被删）
type ReleaseStatus string

const (
	// ReleaseCreated 表示记录已建、部署还没成功过。1a 时期写下的历史行也是这个状态
	// ——对它们来说「记录已建、部署未尝试」是准确的说法。
	ReleaseCreated ReleaseStatus = "created"
	// ReleaseDeploying 表示这一次部署正在进行。
	ReleaseDeploying ReleaseStatus = "deploying"
	// ReleaseActive 是**正在接流量**的那个版本。
	//
	// 单槽形态里它就是 `current` 指向的那一个；蓝绿形态（迭代 4）里它是
	// `applications.serving_slot` 所指槽位正在跑的那一个。**两种形态共用一个取值**：
	// 「哪个版本正在对外服务」是同一个概念，为蓝绿另立一个 `serving` 只会让
	// ActiveRelease / Rollbackable / 保留策略三处都要各判一次，进而漂移。
	ReleaseActive ReleaseStatus = "active"
	// ReleaseStandby 是**起来了、就绪了，但还没接流量**（迭代 4）。
	//
	// 观察窗口内的新槽位就是这个状态：进程在跑、探活通过，只是 Nginx 还没把流量切过来。
	// 它**不是回滚目标**（Rollbackable 为假）：回滚到一个从没接过流量的版本没有意义，
	// 而且它可能随时被判定为失败并停掉。
	ReleaseStandby ReleaseStatus = "standby"
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
// failed（从没跑成功）、standby（还没接过流量）、removed（目录没了）都不满足。
func (s ReleaseStatus) Rollbackable() bool {
	return s == ReleaseActive || s == ReleaseSuperseded
}

// SlotState 是一个**槽位**的运营状态（迭代 4）。
//
// 它与 ReleaseStatus 是两个层次的东西：Release 是一次部署的身份，槽位是「这一侧现在
// 处于什么状态」。同一个槽位先后跑过很多 release，而槽位状态只有这五种。
type SlotState string

const (
	// SlotServing 是这一侧正在接流量（流量由 Nginx upstream 指向它）。
	SlotServing SlotState = "serving"
	// SlotStandby 是这一侧在运行、但没接流量（新版本起来之后的观察期就是这个状态）。
	SlotStandby SlotState = "standby"
	// SlotDraining 是这一侧刚被切走、正在等在途请求结束。
	SlotDraining SlotState = "draining"
	// SlotStopped 是这一侧没在运行（回滚过后它就停在 standby 里等着被重新拉起）。
	SlotStopped SlotState = "stopped"
	// SlotFailed 是这一侧最后一次动作失败了（进程起不来、观察期被判退化）。
	SlotFailed SlotState = "failed"
)

func (s SlotState) Valid() bool {
	switch s {
	case SlotServing, SlotStandby, SlotDraining, SlotStopped, SlotFailed:
		return true
	}
	return false
}

// ApplicationSlot 是一个槽位的运营视图（对应 `application_slots` 表）。
//
// 它是**运营视图而不是线上事实**：真正决定请求去哪一边的是 Nginx 的 upstream
// （见迭代 4 规格 D2）。对账时以 Nginx 为准，这一份是它的镜像。
type ApplicationSlot struct {
	ApplicationID string
	Slot          Slot
	// ReleaseID 是这个槽位当前跑着的 release。空表示还没部署过。
	ReleaseID string
	State     SlotState
	// SwitchedAt 是**流量最近一次切到这一侧**的时刻（不是部署时刻）：
	// 排查「什么时候切的」时，这正是要看的那个时间。
	SwitchedAt *time.Time
	UpdatedAt  time.Time
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
	// Slot 是这次部署落在哪个槽位（迭代 4）。**空表示单槽形态**——迭代 3 的既有行都是空的，
	// 而且单槽部署至今仍然是默认形态。
	Slot Slot
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

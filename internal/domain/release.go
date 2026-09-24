package domain

import (
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

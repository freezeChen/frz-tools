package domain

import (
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// BackupStatus 是一次备份记录的状态。
//
// 它刻意与 Operation 的状态分开：Operation 描述「这次动作跑得怎么样」，
// Backup 描述「这份备份能不能用」。中断的备份可能 Operation 是 failed 而
// Backup 从未落成 succeeded —— 保留策略只看后者，才不会把半份备份当成一份。
type BackupStatus string

const (
	BackupPending   BackupStatus = "pending"
	BackupRunning   BackupStatus = "running"
	BackupSucceeded BackupStatus = "succeeded"
	BackupFailed    BackupStatus = "failed"
	BackupPruned    BackupStatus = "pruned"
)

func (s BackupStatus) Terminal() bool {
	switch s {
	case BackupSucceeded, BackupFailed, BackupPruned:
		return true
	}
	return false
}

// Backup 是一次备份的元数据。内容本身在 StorageBackend 里按 digest 存放。
type Backup struct {
	ID          string
	PolicyID    string
	OperationID string

	Status BackupStatus

	// StorageDigest 只在 succeeded 时有值。逻辑流与落盘字节数分开存，
	// 才能同时回答「压缩比是多少」与「占了多少盘」。
	StorageDigest   Digest
	LogicalBytes    int64
	StoredBytes     int64
	Compression     Compression
	EncryptionKeyID string

	ResourceKind  BackupResourceKind
	ServerVersion string
	ClientVersion string
	Tool          string

	// Labels 是保留策略需要的标记（GFS 的日/周/月）。
	Labels map[string]string

	StartedAt  time.Time
	FinishedAt *time.Time
	VerifiedAt *time.Time
	VerifiedOK *bool

	ErrorCode    string
	ErrorMessage string
}

// Usable 报告这份备份是否可以参与保留策略的计算。
//
// 判据是「已完成**且**校验通过」。未完成或校验不一致的对象**不得**进入清理范围
// （迭代 2 规格 D9）——否则一次失败的备份会被当成一份有效备份，把真正的备份挤掉。
func (b *Backup) Usable() bool {
	if b.Status != BackupSucceeded {
		return false
	}
	return b.VerifiedOK == nil || *b.VerifiedOK
}

// FinishInput 是收尾一次备份所需的最小信息。成功与失败共用一条路径，
// 避免「成功的分支忘了写某个字段」这类只在失败路径上暴露的问题。
type FinishBackupInput struct {
	BackupID        string
	Status          BackupStatus
	StorageDigest   Digest
	LogicalBytes    int64
	StoredBytes     int64
	Compression     Compression
	EncryptionKeyID string
	ServerVersion   string
	ClientVersion   string
	Tool            string
	Labels          map[string]string
	ErrorCode       string
	ErrorMessage    string
}

// BackupRunRequest 是触发一次备份所需的解析结果。
//
// 它把「策略」与「这次要做什么」分开：策略是可复用、可被覆盖的声明，
// 请求是某一次具体的动作。
type BackupRunRequest struct {
	Policy *BackupPolicy
	// RestoreMode 只在恢复时使用；备份时留空。
	RestoreMode RestoreMode
	// BackupID 是恢复的目标备份；备份时留空。
	BackupID string
}

// Validate 校验一次运行请求与策略是否自洽。
func (r *BackupRunRequest) Validate() error {
	if r.Policy == nil {
		return NewError(v1.CodeInvalidRequest, "缺少备份策略")
	}
	if r.Policy.Name == "" {
		return NewError(v1.CodeInvalidRequest, "备份策略缺少 name")
	}
	if r.RestoreMode != "" && !r.RestoreMode.Valid() {
		return NewError(v1.CodeInvalidRequest, "恢复模式取值非法: %q", r.RestoreMode)
	}
	if r.RestoreMode != "" && strings.TrimSpace(r.BackupID) == "" {
		return NewError(v1.CodeInvalidRequest, "恢复时必须给出备份 ID")
	}
	return nil
}

// PreflightCheck 是预检里的一项。
//
// 失败的那一项必须能说清「差在哪」：只回「预检没过」，用户不知道该修什么，
// 而预检的全部意义正是在**产生备份产物之前**把问题暴露出来。
type PreflightCheck struct {
	Name   string
	OK     bool
	Detail string
}

// PreflightReport 是一次预检的汇总。
type PreflightReport struct {
	Checks []PreflightCheck
	// ToolVersion / ServerVersion 记下「哪个版本的工具备的哪个版本的对端」。
	// 恢复时的兼容性判断只能靠它——「12 的 pg_dump 备的库能不能恢复到 15」
	// 这个问题没有别的依据。
	ToolVersion   string
	ServerVersion string
}

// Passed 报告预检是否全部通过。
func (r PreflightReport) Passed() bool {
	_, failed := r.Failure()
	return !failed
}

// Failure 返回第一项未通过的检查，便于直接作为错误信息的内容。
func (r PreflightReport) Failure() (PreflightCheck, bool) {
	for _, check := range r.Checks {
		if !check.OK {
			return check, true
		}
	}
	return PreflightCheck{}, false
}

// BackupMetadata 是适配器对「这次备份是什么」的自述。
//
// 它刻意**不含**摘要、字节数与编码方式：那些由应用层在流上算出，适配器看不到、
// 也不该看到（规格 D1）——否则每个适配器都会长出一份自己的摘要实现。
type BackupMetadata struct {
	ResourceKind  BackupResourceKind
	ServerVersion string
	ClientVersion string
	Tool          string
	// Labels 是保留策略需要的标记（如 GFS 的日/周/月）；没有就留空。
	Labels map[string]string
}

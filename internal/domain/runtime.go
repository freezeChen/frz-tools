package domain

import "time"

// RuntimeStatus 是「进程本身」的状态，来自底层运行时（systemd 的 ActiveState）。
type RuntimeStatus string

const (
	RuntimeActive       RuntimeStatus = "active"
	RuntimeInactive     RuntimeStatus = "inactive"
	RuntimeFailed       RuntimeStatus = "failed"
	RuntimeActivating   RuntimeStatus = "activating"
	RuntimeDeactivating RuntimeStatus = "deactivating"
	RuntimeUnknown      RuntimeStatus = "unknown"
)

// RuntimeHealth 是「能否接流量」的状态。它与 RuntimeStatus 刻意不合并：
// 进程活着不等于已就绪（数据库还没连上、预热还没结束），
// 把两者合成一个字段会让调用方无法区分「该等」和「该重启」。
type RuntimeHealth struct {
	Ready     bool
	CheckedAt time.Time
	Detail    string
}

// RuntimeInfo 是进程的运行信息，用于展示与排查；字段取自 systemd 的
// MainPID / ActiveEnterTimestamp 等属性，缺失时为零值。
type RuntimeInfo struct {
	Status  RuntimeStatus
	MainPID int
	Since   time.Time
	Detail  string
}

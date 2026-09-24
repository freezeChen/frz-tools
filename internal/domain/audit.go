package domain

import "time"

const (
	EventOperationCreated   = "operation.created"
	EventOperationStarted   = "operation.started"
	EventOperationSucceeded = "operation.succeeded"
	EventOperationFailed    = "operation.failed"
	EventOperationCancelled = "operation.cancelled"
	EventOperationRetried   = "operation.retried"
	EventDaemonRecovered    = "daemon.recovered"
	EventLockAcquired       = "lock.acquired"
	EventLockReleased       = "lock.released"

	EventArtifactCreated   = "artifact.created"
	EventArtifactDeleted   = "artifact.deleted"
	EventArtifactCollected = "artifact.collected"

	EventScheduleCreated  = "schedule.created"
	EventScheduleEnabled  = "schedule.enabled"
	EventScheduleDisabled = "schedule.disabled"
	EventScheduleDeleted  = "schedule.deleted"

	// EventRuntimePrepared 记录一次运行时准备的档位决策：同一份 manifest 在不同
	// systemd 版本的主机上生成的 unit 不同，审计里的档位与版本是唯一的解释来源。
	EventRuntimePrepared = "runtime.prepared"
)

type AuditEvent struct {
	EventType   string
	Actor       string
	OperationID string
	Resource    string
	Result      string
	Time        time.Time
	Details     map[string]string
}

type LogEntry struct {
	ID          int64
	OperationID string
	Level       string
	Phase       string
	Message     string
	Fields      map[string]string
	Time        time.Time
}

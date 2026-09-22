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

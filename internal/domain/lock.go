package domain

import "time"

type ResourceLock struct {
	Resource         string
	OwnerOperationID string
	AcquiredAt       time.Time
	ReleasedAt       *time.Time
}

func (l ResourceLock) Active() bool {
	return l.ReleasedAt == nil
}

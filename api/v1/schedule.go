package v1

import (
	"encoding/json"
	"time"
)

type CreateScheduleRequest struct {
	Name            string          `json:"name"`
	Kind            string          `json:"kind"`
	Cron            string          `json:"cron,omitempty"`
	IntervalSeconds int             `json:"intervalSeconds,omitempty"`
	Timezone        string          `json:"timezone,omitempty"`
	Resource        string          `json:"resource"`
	Spec            json.RawMessage `json:"spec,omitempty"`
	MissedRunPolicy string          `json:"missedRunPolicy,omitempty"`
	CreatedBy       string          `json:"createdBy,omitempty"`
}

type Schedule struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Enabled         bool            `json:"enabled"`
	Kind            string          `json:"kind"`
	Cron            string          `json:"cron,omitempty"`
	IntervalSeconds int             `json:"intervalSeconds,omitempty"`
	Timezone        string          `json:"timezone,omitempty"`
	Resource        string          `json:"resource"`
	Spec            json.RawMessage `json:"spec,omitempty"`
	MissedRunPolicy string          `json:"missedRunPolicy"`
	NextRunAt       *time.Time      `json:"nextRunAt,omitempty"`
	LastRunAt       *time.Time      `json:"lastRunAt,omitempty"`
	LastResult      string          `json:"lastResult,omitempty"`
	CreatedAt       time.Time       `json:"createdAt"`
	UpdatedAt       time.Time       `json:"updatedAt"`
	CreatedBy       string          `json:"createdBy,omitempty"`
}

type ScheduleResponse struct {
	APIVersion string   `json:"apiVersion"`
	Schedule   Schedule `json:"schedule"`
}

type ScheduleListResponse struct {
	APIVersion string     `json:"apiVersion"`
	Items      []Schedule `json:"items"`
}

type ScheduleRun struct {
	ID           string     `json:"id"`
	ScheduleID   string     `json:"scheduleId"`
	ScheduledFor time.Time  `json:"scheduledFor"`
	StartedAt    time.Time  `json:"startedAt"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
	Result       string     `json:"result"`
	OperationID  string     `json:"operationId,omitempty"`
	ErrorCode    string     `json:"errorCode,omitempty"`
	ErrorMessage string     `json:"errorMessage,omitempty"`
}

type ScheduleRunListResponse struct {
	APIVersion string        `json:"apiVersion"`
	Schedule   string        `json:"schedule"`
	Items      []ScheduleRun `json:"items"`
}

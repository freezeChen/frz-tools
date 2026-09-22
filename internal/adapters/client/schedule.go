package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

type CreateScheduleInput struct {
	Name            string
	Kind            string
	Cron            string
	Interval        time.Duration
	Timezone        string
	Resource        string
	Spec            json.RawMessage
	MissedRunPolicy string
	CreatedBy       string
}

func (c *Client) CreateSchedule(ctx context.Context, in CreateScheduleInput) (*v1.Schedule, error) {
	var out v1.ScheduleResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/schedules", nil, v1.CreateScheduleRequest{
		Name:            in.Name,
		Kind:            in.Kind,
		Cron:            in.Cron,
		IntervalSeconds: int(in.Interval / time.Second),
		Timezone:        in.Timezone,
		Resource:        in.Resource,
		Spec:            in.Spec,
		MissedRunPolicy: in.MissedRunPolicy,
		CreatedBy:       in.CreatedBy,
	}, &out); err != nil {
		return nil, err
	}
	return &out.Schedule, nil
}

func (c *Client) ListSchedules(ctx context.Context, limit int) (*v1.ScheduleListResponse, error) {
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var out v1.ScheduleListResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/schedules", query, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetSchedule(ctx context.Context, ref string) (*v1.Schedule, error) {
	var out v1.ScheduleResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/schedules/"+url.PathEscape(ref), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Schedule, nil
}

func (c *Client) EnableSchedule(ctx context.Context, ref string) (*v1.Schedule, error) {
	return c.setScheduleEnabled(ctx, ref, "enable")
}

func (c *Client) DisableSchedule(ctx context.Context, ref string) (*v1.Schedule, error) {
	return c.setScheduleEnabled(ctx, ref, "disable")
}

func (c *Client) setScheduleEnabled(ctx context.Context, ref, action string) (*v1.Schedule, error) {
	var out v1.ScheduleResponse
	path := "/api/v1/schedules/" + url.PathEscape(ref) + "/" + action
	if err := c.do(ctx, http.MethodPost, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Schedule, nil
}

func (c *Client) DeleteSchedule(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/schedules/"+url.PathEscape(ref), nil, nil, nil)
}

func (c *Client) ScheduleRuns(ctx context.Context, ref string, limit int) (*v1.ScheduleRunListResponse, error) {
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var out v1.ScheduleRunListResponse
	path := "/api/v1/schedules/" + url.PathEscape(ref) + "/runs"
	if err := c.do(ctx, http.MethodGet, path, query, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

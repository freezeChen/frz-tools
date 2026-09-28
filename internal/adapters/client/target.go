package client

import (
	"context"
	"net/http"
	"net/url"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// SetTargets 替换某个应用的部署目标列表。
func (c *Client) SetTargets(ctx context.Context, application string, hosts []string, updatedBy string) (*v1.ApplicationTargets, error) {
	var out v1.TargetsResponse
	if err := c.do(ctx, http.MethodPut, "/api/v1/targets/"+url.PathEscape(application), nil,
		v1.SetTargetsRequest{Hosts: hosts, UpdatedBy: updatedBy}, &out); err != nil {
		return nil, err
	}
	return &out.Targets, nil
}

func (c *Client) GetTargets(ctx context.Context, application string) (*v1.ApplicationTargets, error) {
	var out v1.TargetsResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/targets/"+url.PathEscape(application), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Targets, nil
}

func (c *Client) ListTargets(ctx context.Context) (*v1.TargetsListResponse, error) {
	var out v1.TargetsListResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/targets", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// ApplicationSlots 取回两个槽位的运营视图与线上实际。
func (c *Client) ApplicationSlots(ctx context.Context, appRef string) (*v1.SlotListResponse, error) {
	var out v1.SlotListResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/applications/"+appRef+"/slots", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SlotHistory 取回切换时间线（limit ≤ 0 时由服务端取默认条数）。
func (c *Client) SlotHistory(ctx context.Context, appRef string, limit int) (*v1.SlotHistoryResponse, error) {
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var out v1.SlotHistoryResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/applications/"+appRef+"/slots/history", query, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

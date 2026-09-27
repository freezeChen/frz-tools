package client

import (
	"context"
	"net/http"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// Identity 问对端「你是谁、我以什么身份连上来的、这次走的是哪条路」（迭代 5a）。
//
// 它是 `host check` 与 `host list --check` 唯一依赖的端点：探活要同时回答
// 「连不连得上」与「是不是我要找的那台机」，而后者只有 identity 答得出来。
func (c *Client) Identity(ctx context.Context) (*v1.IdentityResponse, error) {
	var out v1.IdentityResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/identity", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

package client

import (
	"context"
	"net/http"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// DeployApplication 部署一个版本。
//
// 响应有两种形态：产生了 Operation（异步进行），或者**什么都没做**（Noop=true）——
// 要上的版本已经是当前版本。后者不是错误，重复提交一次部署就该是这个结果。
func (c *Client) DeployApplication(ctx context.Context, app, manifest string, in ActionInput) (*v1.DeployResponse, error) {
	return c.deployAction(ctx, app, "deploy", v1.DeployRequest{
		Manifest:       manifest,
		IdempotencyKey: in.IdempotencyKey,
		CreatedBy:      in.CreatedBy,
		Retry:          in.Retry,
	})
}

// RollbackApplication 回滚到某个既有版本；to 为空表示「上一个曾经激活过的版本」。
func (c *Client) RollbackApplication(ctx context.Context, app, to string, in ActionInput) (*v1.DeployResponse, error) {
	return c.deployAction(ctx, app, "rollback", v1.RollbackRequest{
		To:             to,
		IdempotencyKey: in.IdempotencyKey,
		CreatedBy:      in.CreatedBy,
		Retry:          in.Retry,
	})
}

// deployAction 把两个入口收敛成一条 HTTP 调用。
//
// 状态码区分两种结果（202 = 产生了操作、200 = 什么都没做），因此这里用带状态码的
// 那个方法，好让调用方知道该不该去等一个 Operation。
func (c *Client) deployAction(ctx context.Context, app, action string, body any) (*v1.DeployResponse, error) {
	var out v1.DeployResponse
	_, err := c.doWithStatus(ctx, http.MethodPost, "/api/v1/applications/"+app+"/"+action, nil, body, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

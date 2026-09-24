package client

import (
	"context"
	"net/http"
	"net/url"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// ValidateRuntime 只校验应用的当前 manifest 能否被本机适配器执行，无副作用。
func (c *Client) ValidateRuntime(ctx context.Context, appRef string) (*v1.RuntimeValidateResponse, error) {
	var out v1.RuntimeValidateResponse
	if err := c.do(ctx, http.MethodPost, runtimePath(appRef, "validate"), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PrepareRuntime 执行一次幂等的运行时准备，响应里带上适配器做出的档位决策。
func (c *Client) PrepareRuntime(ctx context.Context, appRef string) (*v1.RuntimePrepareResponse, error) {
	var out v1.RuntimePrepareResponse
	if err := c.do(ctx, http.MethodPost, runtimePath(appRef, "prepare"), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RuntimeHealth 返回就绪快照。未就绪（Ready=false）不是错误，由响应体表达。
func (c *Client) RuntimeHealth(ctx context.Context, appRef string) (*v1.RuntimeHealthResponse, error) {
	var out v1.RuntimeHealthResponse
	if err := c.do(ctx, http.MethodGet, runtimePath(appRef, "health"), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StartRuntimeAction / StopRuntimeAction 输入。
type RuntimeActionInput struct {
	IdempotencyKey string
	CreatedBy      string
	// Retry 为 nil 表示不自动重试。runtime.* 允许声明重试，但只有「就绪从未通过」
	// 才算失败、才可重试（迭代 1d 规格 D6）。
	Retry *v1.RetrySpec
}

// StartRuntime 创建 runtime.start 操作；返回的 Operation 可用既有的 operation 命令查询。
func (c *Client) StartRuntime(ctx context.Context, appRef string, in RuntimeActionInput) (*v1.Operation, bool, error) {
	return c.runtimeAction(ctx, appRef, "start", in)
}

func (c *Client) StopRuntime(ctx context.Context, appRef string, in RuntimeActionInput) (*v1.Operation, bool, error) {
	return c.runtimeAction(ctx, appRef, "stop", in)
}

func (c *Client) runtimeAction(ctx context.Context, appRef, action string, in RuntimeActionInput) (*v1.Operation, bool, error) {
	var out v1.OperationResponse
	created, err := c.doWithStatus(ctx, http.MethodPost, runtimePath(appRef, action), nil,
		v1.RuntimeActionRequest{IdempotencyKey: in.IdempotencyKey, CreatedBy: in.CreatedBy, Retry: in.Retry}, &out)
	if err != nil {
		return nil, false, err
	}
	return &out.Operation, created, nil
}

func runtimePath(appRef, action string) string {
	return "/api/v1/applications/" + url.PathEscape(appRef) + "/runtime/" + action
}

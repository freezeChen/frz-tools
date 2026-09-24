package client

import (
	"context"
	"net/http"
	"net/url"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// PutApplicationSpec 提交 manifest 的原始文本。客户端刻意不做解码：严格解码与迁移链
// 只在 opsd 侧有一份，本地再解一遍迟早会出现「CLI 认为合法、opsd 认为非法」的分歧。
func (c *Client) PutApplicationSpec(ctx context.Context, appRef string, manifest []byte, updatedBy string) (*v1.ApplicationSpec, error) {
	var out v1.ApplicationSpecResponse
	path := "/api/v1/applications/" + url.PathEscape(appRef) + "/spec"
	if err := c.do(ctx, http.MethodPut, path, nil, v1.PutApplicationSpecRequest{
		Manifest:  string(manifest),
		UpdatedBy: updatedBy,
	}, &out); err != nil {
		return nil, err
	}
	return &out.Spec, nil
}

func (c *Client) GetApplicationSpec(ctx context.Context, appRef string) (*v1.ApplicationSpec, error) {
	var out v1.ApplicationSpecResponse
	path := "/api/v1/applications/" + url.PathEscape(appRef) + "/spec"
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Spec, nil
}

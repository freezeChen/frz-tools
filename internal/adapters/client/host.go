package client

import (
	"context"
	"net/http"
	"net/url"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

func (c *Client) ListHosts(ctx context.Context) (*v1.HostListResponse, error) {
	var out v1.HostListResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/hosts", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateHost(ctx context.Context, name, address string, labels map[string]string) (*v1.Host, error) {
	var out v1.HostResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/hosts", nil,
		v1.CreateHostRequest{Name: name, Address: address, Labels: labels}, &out); err != nil {
		return nil, err
	}
	return &out.Host, nil
}

func (c *Client) GetHost(ctx context.Context, ref string) (*v1.Host, error) {
	var out v1.HostResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/hosts/"+url.PathEscape(ref), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Host, nil
}

func (c *Client) ListEnvironments(ctx context.Context) (*v1.EnvironmentListResponse, error) {
	var out v1.EnvironmentListResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/environments", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetEnvironment(ctx context.Context, ref string) (*v1.Environment, error) {
	var out v1.EnvironmentResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/environments/"+url.PathEscape(ref), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Environment, nil
}

func (c *Client) CreateEnvironment(ctx context.Context, name string, labels map[string]string) (*v1.Environment, error) {
	var out v1.EnvironmentResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/environments", nil,
		v1.CreateEnvironmentRequest{Name: name, Labels: labels}, &out); err != nil {
		return nil, err
	}
	return &out.Environment, nil
}

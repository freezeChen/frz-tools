package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

func (c *Client) CreateApplication(ctx context.Context, name string, labels map[string]string) (*v1.Application, error) {
	var out v1.ApplicationResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/applications", nil,
		v1.CreateApplicationRequest{Name: name, Labels: labels}, &out); err != nil {
		return nil, err
	}
	return &out.Application, nil
}

func (c *Client) ListApplications(ctx context.Context) (*v1.ApplicationListResponse, error) {
	var out v1.ApplicationListResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/applications", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetApplication(ctx context.Context, ref string) (*v1.Application, error) {
	var out v1.ApplicationResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/applications/"+url.PathEscape(ref), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Application, nil
}

func (c *Client) ListReleases(ctx context.Context, appRef string, limit int) (*v1.ReleaseListResponse, error) {
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var out v1.ReleaseListResponse
	path := "/api/v1/applications/" + url.PathEscape(appRef) + "/releases"
	if err := c.do(ctx, http.MethodGet, path, query, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type CreateReleaseInput struct {
	Application string
	Artifact    string
	Version     string
	Labels      map[string]string
	CreatedBy   string
}

func (c *Client) CreateRelease(ctx context.Context, in CreateReleaseInput) (*v1.Release, error) {
	var out v1.ReleaseResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/releases", nil, v1.CreateReleaseRequest{
		Application: in.Application,
		Artifact:    in.Artifact,
		Version:     in.Version,
		Labels:      in.Labels,
		CreatedBy:   in.CreatedBy,
	}, &out); err != nil {
		return nil, err
	}
	return &out.Release, nil
}

func (c *Client) GetRelease(ctx context.Context, id string) (*v1.Release, error) {
	var out v1.ReleaseResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/releases/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Release, nil
}

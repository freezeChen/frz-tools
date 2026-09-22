package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

const baseURL = "http://opsd"

type Client struct {
	http   *http.Client
	socket string
}

func New(socketPath string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		http:   &http.Client{Transport: transport, Timeout: timeout},
		socket: socketPath,
	}
}

func (c *Client) Health(ctx context.Context) (*v1.HealthResponse, error) {
	var out v1.HealthResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/health", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateOperation(ctx context.Context, req v1.CreateOperationRequest) (*v1.Operation, bool, error) {
	var out v1.OperationResponse
	created, err := c.doWithStatus(ctx, http.MethodPost, "/api/v1/operations", nil, req, &out)
	if err != nil {
		return nil, false, err
	}
	return &out.Operation, created, nil
}

func (c *Client) GetOperation(ctx context.Context, id string) (*v1.Operation, error) {
	var out v1.OperationResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/operations/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Operation, nil
}

func (c *Client) CancelOperation(ctx context.Context, id string) (*v1.Operation, error) {
	var out v1.OperationResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/operations/"+url.PathEscape(id)+"/cancel", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Operation, nil
}

func (c *Client) RetryOperation(ctx context.Context, id string) (*v1.Operation, error) {
	var out v1.OperationResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/operations/"+url.PathEscape(id)+"/retry", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Operation, nil
}

func (c *Client) Logs(ctx context.Context, id string, cursor int64, limit int) (*v1.LogsResponse, error) {
	query := url.Values{}
	if cursor > 0 {
		query.Set("cursor", strconv.FormatInt(cursor, 10))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var out v1.LogsResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/operations/"+url.PathEscape(id)+"/logs", query, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	_, err := c.doWithStatus(ctx, method, path, query, body, out)
	return err
}

type UploadArtifactInput struct {
	Name      string
	MediaType string
	Digest    string
	Body      io.Reader
	CreatedBy string
}

func (c *Client) UploadArtifact(ctx context.Context, in UploadArtifactInput) (*v1.Artifact, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/artifacts", in.Body)
	if err != nil {
		return nil, false, err
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	setIfNotEmpty(request, "X-Artifact-Name", in.Name)
	setIfNotEmpty(request, "X-Artifact-Media-Type", in.MediaType)
	setIfNotEmpty(request, "X-Artifact-SHA256", in.Digest)
	setIfNotEmpty(request, "X-Requested-By", in.CreatedBy)

	response, err := c.http.Do(request)
	if err != nil {
		return nil, false, domain.NewError(v1.CodeInternal, "cannot reach opsd at %s: %v", c.socket, err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return nil, false, decodeError(response)
	}

	var decoded v1.ArtifactResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, false, domain.NewError(v1.CodeInternal, "malformed response from opsd: %v", err)
	}
	return &decoded.Artifact, response.StatusCode == http.StatusCreated, nil
}

func (c *Client) ListArtifacts(ctx context.Context, cursor string, limit int) (*v1.ArtifactListResponse, error) {
	query := url.Values{}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var out v1.ArtifactListResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/artifacts", query, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetArtifact(ctx context.Context, ref string) (*v1.Artifact, error) {
	var out v1.ArtifactResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/artifacts/"+url.PathEscape(ref), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Artifact, nil
}

func (c *Client) VerifyArtifact(ctx context.Context, ref string) (*v1.VerifyArtifactResponse, error) {
	var out v1.VerifyArtifactResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/artifacts/"+url.PathEscape(ref)+"/verify", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DeleteArtifact(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/artifacts/"+url.PathEscape(ref), nil, nil, nil)
}

type CollectArtifactsInput struct {
	KeepLast    int
	OlderThanMs int64
	DryRun      bool
}

func (c *Client) CollectArtifacts(ctx context.Context, in CollectArtifactsInput) (*v1.CollectArtifactsResponse, error) {
	request := map[string]any{"keepLast": in.KeepLast, "olderThanMs": in.OlderThanMs, "dryRun": in.DryRun}
	var out v1.CollectArtifactsResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/artifacts/gc", nil, request, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func setIfNotEmpty(request *http.Request, header, value string) {
	if value != "" {
		request.Header.Set(header, value)
	}
}

func (c *Client) doWithStatus(ctx context.Context, method, path string, query url.Values, body, out any) (bool, error) {
	endpoint := baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return false, err
		}
		payload = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return false, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.http.Do(request)
	if err != nil {
		return false, domain.NewError(v1.CodeInternal, "cannot reach opsd at %s: %v", c.socket, err)
	}
	defer response.Body.Close()

	if response.StatusCode >= 400 {
		return false, decodeError(response)
	}
	if out != nil {
		if err := json.NewDecoder(response.Body).Decode(out); err != nil {
			return false, domain.NewError(v1.CodeInternal, "malformed response from opsd: %v", err)
		}
	}
	return response.StatusCode == http.StatusAccepted, nil
}

func decodeError(response *http.Response) error {
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return domain.NewError(v1.CodeInternal, "opsd returned HTTP %d", response.StatusCode)
	}

	var apiError v1.ErrorResponse
	if err := json.Unmarshal(raw, &apiError); err != nil || apiError.Code == "" {
		return domain.NewError(v1.CodeInternal, "opsd returned HTTP %d: %s", response.StatusCode, string(raw))
	}

	coded := domain.NewError(apiError.Code, "%s", apiError.Message)
	coded.Details = apiError.Details
	return coded
}

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

	v1 "frz-tools/api/v1"
	"frz-tools/internal/domain"
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

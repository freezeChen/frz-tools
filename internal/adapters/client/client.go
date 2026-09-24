package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// DownloadArtifact 把制品内容下载到 destPath，并返回该制品的元数据。
//
// 先取元数据再下载，是为了拿到「应该是什么摘要」这个期望值：校验必须对着记录做，
// 而不是对着服务端在同一份响应里给出的头做——后者只证明两边自洽，证明不了内容正确。
// ETag 仍会核对一次，两端对同一个制品 ID 的认知不一致时宁可不落盘。
func (c *Client) DownloadArtifact(ctx context.Context, ref, destPath string) (*v1.Artifact, error) {
	artifact, err := c.GetArtifact(ctx, ref)
	if err != nil {
		return nil, err
	}
	expected, err := domain.ParseDigest(artifact.Digest)
	if err != nil {
		return nil, err
	}

	endpoint := baseURL + "/api/v1/artifacts/" + url.PathEscape(artifact.ID) + "/content"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, domain.NewError(v1.CodeInternal, "cannot reach opsd at %s: %v", c.socket, err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return nil, decodeError(response)
	}
	if etag := strings.Trim(response.Header.Get("ETag"), `"`); etag != "" && etag != artifact.Digest {
		return nil, domain.NewError(v1.CodeArtifactChecksum,
			"opsd 为制品 %s 返回的 ETag %s 与记录的摘要 %s 不一致", artifact.ID, etag, artifact.Digest)
	}

	if err := writeArtifactFile(destPath, response.Body, expected, artifact.Size); err != nil {
		return nil, err
	}
	return artifact, nil
}

// writeArtifactFile 把 src 写到 destPath：先写同目录下的临时文件，逐字节计算摘要，
// 校验通过后再原子 rename。临时文件与目标同目录是必需的——跨文件系统的 rename
// 会退化成复制，就不再是原子的。任何一步失败都不会让目标路径出现半截内容：
// 下载中断或内容不一致时，用户要么看到旧的完整文件，要么什么都看不到。
func writeArtifactFile(destPath string, src io.Reader, expected domain.Digest, expectedSize int64) error {
	directory := filepath.Dir(destPath)
	temp, err := os.CreateTemp(directory, "."+filepath.Base(destPath)+".partial-")
	if err != nil {
		return domain.NewError(v1.CodeInvalidRequest, "无法在 %q 下创建临时文件: %v", directory, err)
	}
	tempName := temp.Name()
	committed := false
	defer func() {
		// 成功路径上临时文件已经被 rename 掉、句柄也已经关闭，这里的两次调用
		// 只会返回「已经关闭 / 不存在」，因此有意忽略返回值。
		_ = temp.Close()
		if !committed {
			// rename 之前的所有失败路径都要清掉临时文件，否则会在目标目录里
			// 留下看不懂的残片。
			_ = os.Remove(tempName)
		}
	}()

	hasher := domain.NewHasher()
	if _, err := io.Copy(io.MultiWriter(temp, hasher), src); err != nil {
		return domain.NewError(v1.CodeInternal, "下载制品内容失败: %v", err)
	}
	if hasher.Digest() != expected || hasher.Size() != expectedSize {
		return domain.NewError(v1.CodeArtifactChecksum,
			"下载内容与制品记录不一致: 期望 %s/%d 字节，实际 %s/%d 字节",
			expected, expectedSize, hasher.Digest(), hasher.Size())
	}
	// 先把数据刷到磁盘再 rename：否则断电后可能出现「文件已就位、内容还没落盘」
	// 的窗口，而制品是可被当作发布产物消费的。
	if err := temp.Sync(); err != nil {
		return domain.NewError(v1.CodeInternal, "无法刷新 %q: %v", tempName, err)
	}
	if err := temp.Close(); err != nil {
		return domain.NewError(v1.CodeInternal, "无法关闭 %q: %v", tempName, err)
	}
	if err := os.Chmod(tempName, 0o644); err != nil {
		return domain.NewError(v1.CodeInternal, "无法设置 %q 的权限: %v", tempName, err)
	}
	if err := os.Rename(tempName, destPath); err != nil {
		return domain.NewError(v1.CodeInternal, "无法写入 %q: %v", destPath, err)
	}
	committed = true
	return nil
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

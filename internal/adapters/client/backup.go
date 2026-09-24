package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// PutBackupPolicy 提交一份策略 manifest。manifest 以原文传送，解析与校验发生在服务端。
func (c *Client) PutBackupPolicy(ctx context.Context, manifest, updatedBy string) (*v1.BackupPolicy, error) {
	var out v1.BackupPolicyResponse
	if err := c.do(ctx, http.MethodPut, "/api/v1/backup-policies/"+url.PathEscape(policyNameOf(manifest)),
		nil, v1.PutBackupPolicyRequest{Manifest: manifest, UpdatedBy: updatedBy}, &out); err != nil {
		return nil, err
	}
	return &out.Policy, nil
}

func (c *Client) GetBackupPolicy(ctx context.Context, name string) (*v1.BackupPolicy, error) {
	var out v1.BackupPolicyResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/backup-policies/"+url.PathEscape(name), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Policy, nil
}

func (c *Client) ListBackupPolicies(ctx context.Context) (*v1.BackupPolicyListResponse, error) {
	var out v1.BackupPolicyListResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/backup-policies", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ActionInput 是触发备份类操作的公共参数。
// ActionInput 是「会发起一次动作」的命令行共有的那几个输入。备份与部署都用它，
// 因为「谁发起的、要不要重试、幂等键是什么」与具体做什么无关。
type ActionInput struct {
	IdempotencyKey string
	CreatedBy      string
	Retry          *v1.RetrySpec
}

func (c *Client) RunBackup(ctx context.Context, policy string, in ActionInput) (*v1.Operation, bool, error) {
	var out v1.OperationResponse
	created, err := c.doWithStatus(ctx, http.MethodPost, "/api/v1/backups", nil,
		v1.BackupRunRequest{
			Policy:         policy,
			IdempotencyKey: in.IdempotencyKey,
			CreatedBy:      in.CreatedBy,
			Retry:          in.Retry,
		}, &out)
	if err != nil {
		return nil, false, err
	}
	return &out.Operation, created, nil
}

func (c *Client) VerifyBackup(ctx context.Context, backupID string, in ActionInput) (*v1.Operation, bool, error) {
	var out v1.OperationResponse
	created, err := c.doWithStatus(ctx, http.MethodPost, backupPath(backupID, "verify"), nil,
		v1.BackupVerifyRequest{
			IdempotencyKey: in.IdempotencyKey,
			CreatedBy:      in.CreatedBy,
			Retry:          in.Retry,
		}, &out)
	if err != nil {
		return nil, false, err
	}
	return &out.Operation, created, nil
}

// RestoreBackup 恢复一份备份。confirm 只对 inPlace 模式有意义，且是**服务端**的硬门槛。
func (c *Client) RestoreBackup(ctx context.Context, backupID string, mode string, confirm bool, in ActionInput) (*v1.Operation, bool, error) {
	var out v1.OperationResponse
	created, err := c.doWithStatus(ctx, http.MethodPost, backupPath(backupID, "restore"), nil,
		v1.BackupRestoreRequest{
			Mode:           mode,
			Confirm:        confirm,
			IdempotencyKey: in.IdempotencyKey,
			CreatedBy:      in.CreatedBy,
			Retry:          in.Retry,
		}, &out)
	if err != nil {
		return nil, false, err
	}
	return &out.Operation, created, nil
}

func (c *Client) ListBackups(ctx context.Context, policy string, limit int) (*v1.BackupListResponse, error) {
	query := url.Values{}
	if policy != "" {
		query.Set("policy", policy)
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var out v1.BackupListResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/backups", query, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetBackup(ctx context.Context, id string) (*v1.Backup, error) {
	var out v1.BackupResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/backups/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Backup, nil
}

func backupPath(id, action string) string {
	return "/api/v1/backups/" + url.PathEscape(id) + "/" + action
}

// policyNameOf 从 manifest 原文里读出 name，用于拼路径。
//
// 只做一次极浅的扫描而不是解析 YAML：真正的解析与校验在服务端，客户端不该对
// manifest 的合法性与字段语义有任何第二套理解。读不出来时返回空串，服务端会因为
// 「路径里的名字与 manifest 不一致」给出清晰的错误。
func policyNameOf(manifest string) string {
	for _, line := range strings.Split(manifest, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "name:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
		}
	}
	return ""
}

// PruneBackups 按策略声明的保留规则清理备份。
//
// 它是**同步**调用（与制品 GC 一致）：服务端当场把结果算完再返回，不产生 Operation。
func (c *Client) PruneBackups(ctx context.Context, policy string, dryRun bool) (*v1.BackupPruneResponse, error) {
	var out v1.BackupPruneResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/backups/prune", nil,
		v1.BackupPruneRequest{Policy: policy, DryRun: dryRun}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

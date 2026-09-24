package httpapi

import (
	"net/http"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/manifest"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

func (s *Server) handlePutBackupPolicy(w http.ResponseWriter, r *http.Request) {
	if s.deps.Backups == nil {
		writeError(w, s.logger(), errBackupServiceMissing())
		return
	}

	var req v1.PutBackupPolicyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, s.logger(), err)
		return
	}

	// manifest 必须先过严格解码与迁移链：这是唯一能挡住「未知字段被静默丢弃」的入口，
	// 因此本层不接受已经结构化的字段，也不在这里做字段校验——规则只有 domain 那一份。
	policy, err := manifest.ParseBackupPolicyReader(strings.NewReader(req.Manifest))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	// 路径里的名字是权威：manifest 里也写了 name，两者不一致会让「我改的是哪一份」
	// 变得含糊。
	if name := r.PathValue("name"); name != "" && name != policy.Name {
		writeError(w, s.logger(), domain.NewError(v1.CodeManifestInvalid,
			"路径里的策略名 %q 与 manifest 里的 %q 不一致", name, policy.Name))
		return
	}

	saved, err := s.deps.Backups.SavePolicy(r.Context(), policy, req.UpdatedBy)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.BackupPolicyResponse{
		APIVersion: v1.APIVersion,
		Policy:     backupPolicyDTO(saved),
	})
}

func (s *Server) handleGetBackupPolicy(w http.ResponseWriter, r *http.Request) {
	if s.deps.Backups == nil {
		writeError(w, s.logger(), errBackupServiceMissing())
		return
	}
	policy, err := s.deps.Backups.GetPolicy(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.BackupPolicyResponse{
		APIVersion: v1.APIVersion,
		Policy:     backupPolicyDTO(policy),
	})
}

func (s *Server) handleListBackupPolicies(w http.ResponseWriter, r *http.Request) {
	if s.deps.Backups == nil {
		writeError(w, s.logger(), errBackupServiceMissing())
		return
	}
	policies, err := s.deps.Backups.ListPolicies(r.Context())
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	items := make([]v1.BackupPolicy, 0, len(policies))
	for i := range policies {
		items = append(items, backupPolicyDTO(&policies[i]))
	}
	writeJSON(w, http.StatusOK, v1.BackupPolicyListResponse{
		APIVersion: v1.APIVersion,
		Items:      items,
	})
}

// handleRunBackup 触发一次备份，返回 Operation（异步）。
func (s *Server) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	if s.deps.Backups == nil {
		writeError(w, s.logger(), errBackupServiceMissing())
		return
	}

	var req v1.BackupRunRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, s.logger(), err)
			return
		}
	}
	s.createBackupOperation(w, r, v1.KindBackupRun, req.Policy, req.IdempotencyKey, req.CreatedBy, req.Retry, "")
}

func (s *Server) handleVerifyBackup(w http.ResponseWriter, r *http.Request) {
	if s.deps.Backups == nil {
		writeError(w, s.logger(), errBackupServiceMissing())
		return
	}

	var req v1.BackupVerifyRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, s.logger(), err)
			return
		}
	}
	s.createBackupOperation(w, r, v1.KindBackupVerify, r.PathValue("id"), req.IdempotencyKey, req.CreatedBy, req.Retry, "")
}

func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	if s.deps.Backups == nil {
		writeError(w, s.logger(), errBackupServiceMissing())
		return
	}

	var req v1.BackupRestoreRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, s.logger(), err)
			return
		}
	}
	// 模式与确认要随 Operation 存下来：它们在提交时被确认过，执行时不该再问一次
	// （见 application.RestoreOptions）。
	spec, err := application.RestoreOptionsJSON(domain.RestoreMode(req.Mode), req.Confirm)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	s.createBackupOperation(w, r, v1.KindBackupRestore, r.PathValue("id"), req.IdempotencyKey, req.CreatedBy, req.Retry, string(spec))
}

// createBackupOperation 与 runtime.* 走同一个 Service.Create：锁、幂等键、请求摘要、
// 审计、日志、取消、重试全部复用既有机制，没有第二条旁路。
func (s *Server) createBackupOperation(
	w http.ResponseWriter,
	r *http.Request,
	kind, resource, idempotencyKey, createdBy string,
	retry *v1.RetrySpec,
	spec string,
) {
	if s.deps.Service == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "operation service is not configured"))
		return
	}
	if idempotencyKey == "" {
		idempotencyKey = r.Header.Get("Idempotency-Key")
	}

	createReq := v1.CreateOperationRequest{
		Kind:           kind,
		Resource:       resource,
		IdempotencyKey: idempotencyKey,
		CreatedBy:      createdBy,
		Retry:          retry,
	}
	if spec != "" {
		createReq.Spec = []byte(spec)
	}

	op, created, err := s.deps.Service.Create(r.Context(), createReq)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	status := http.StatusAccepted
	if !created {
		// 幂等键命中既有操作：复用而不是新建。
		status = http.StatusOK
	}
	writeJSON(w, status, v1.OperationResponse{
		APIVersion: v1.APIVersion,
		Operation:  operationDTO(op),
	})
}

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	if s.deps.Backups == nil {
		writeError(w, s.logger(), errBackupServiceMissing())
		return
	}
	limit, err := intQuery(r, "limit", 0)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	backups, err := s.deps.Backups.List(r.Context(), r.URL.Query().Get("policy"), limit)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	items := make([]v1.Backup, 0, len(backups))
	for i := range backups {
		items = append(items, backupDTO(&backups[i]))
	}
	writeJSON(w, http.StatusOK, v1.BackupListResponse{
		APIVersion: v1.APIVersion,
		Items:      items,
	})
}

func (s *Server) handleGetBackup(w http.ResponseWriter, r *http.Request) {
	if s.deps.Backups == nil {
		writeError(w, s.logger(), errBackupServiceMissing())
		return
	}
	backup, err := s.deps.Backups.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.BackupResponse{
		APIVersion: v1.APIVersion,
		Backup:     backupDTO(backup),
	})
}

// errBackupServiceMissing 是与 runtime 端点同形的「本部署没这个能力」错误。
func errBackupServiceMissing() error {
	return domain.NewError(v1.CodeConfigInvalid,
		"本部署未配置备份存储根（backupStore.root），备份相关端点不可用")
}

func backupPolicyDTO(policy *domain.BackupPolicy) v1.BackupPolicy {
	dto := v1.BackupPolicy{
		ID:   policy.ID,
		Name: policy.Name,
		Resource: v1.BackupPolicySource{
			Kind:     string(policy.Resource.Kind),
			Paths:    policy.Resource.Paths,
			Exclude:  policy.Resource.Exclude,
			Database: policy.Resource.Database,
		},
		Encoding: v1.BackupEncodingSpec{
			Compression: string(policy.Encoding.Compression),
			Encryption:  v1.BackupEncryptionSpec{Enabled: policy.Encoding.Encryption.Enabled},
		},
		Retention: v1.BackupRetention{
			KeepLast: policy.Retention.KeepLast,
			KeepDays: policy.Retention.KeepDays,
			GFS: v1.BackupGFS{
				Daily:   policy.Retention.GFS.Daily,
				Weekly:  policy.Retention.GFS.Weekly,
				Monthly: policy.Retention.GFS.Monthly,
			},
		},
		Timeout: v1.BackupTimeoutSpec{
			BackupSeconds:  int(policy.Timeout.Backup.Seconds()),
			RestoreSeconds: int(policy.Timeout.Restore.Seconds()),
		},
	}
	if policy.Resource.Symlinks != "" {
		dto.Resource.Symlinks = string(policy.Resource.Symlinks)
	}
	// 凭据只以**引用**形式出现在响应里，密钥本身永不外泄。
	if policy.Resource.DSNSecret.Name != "" {
		ref := policy.Resource.DSNSecret
		dto.Resource.DSNSecret = &v1.SecretRef{Kind: string(ref.Kind), Name: ref.Name}
	}
	if policy.Encoding.Encryption.KeySecret.Name != "" {
		ref := policy.Encoding.Encryption.KeySecret
		dto.Encoding.Encryption.KeySecret = &v1.SecretRef{Kind: string(ref.Kind), Name: ref.Name}
	}
	return dto
}

func backupDTO(backup *domain.Backup) v1.Backup {
	return v1.Backup{
		ID:              backup.ID,
		PolicyID:        backup.PolicyID,
		OperationID:     backup.OperationID,
		Status:          string(backup.Status),
		StorageDigest:   backup.StorageDigest.String(),
		LogicalBytes:    backup.LogicalBytes,
		StoredBytes:     backup.StoredBytes,
		Compression:     string(backup.Compression),
		EncryptionKeyID: backup.EncryptionKeyID,
		ResourceKind:    string(backup.ResourceKind),
		ServerVersion:   backup.ServerVersion,
		ClientVersion:   backup.ClientVersion,
		Tool:            backup.Tool,
		Labels:          backup.Labels,
		VerifiedOK:      backup.VerifiedOK,
		StartedAt:       backup.StartedAt,
		FinishedAt:      backup.FinishedAt,
		VerifiedAt:      backup.VerifiedAt,
		ErrorCode:       backup.ErrorCode,
		ErrorMessage:    backup.ErrorMessage,
	}
}

// handlePruneBackups 按策略声明的保留规则清理备份。
//
// 这是本工具里**唯一**会删除备份内容的入口，因此三件事都在应用层兜住（见
// application.BackupService.Prune）：只删已完成且校验没失败的、正在被操作的跳过、
// 内容仍被别的备份引用时不删。这里只负责把结果原样交出去。
func (s *Server) handlePruneBackups(w http.ResponseWriter, r *http.Request) {
	if s.deps.Backups == nil {
		writeError(w, s.logger(), errBackupServiceMissing())
		return
	}

	var req v1.BackupPruneRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, s.logger(), err)
			return
		}
	}
	if strings.TrimSpace(req.Policy) == "" {
		writeError(w, s.logger(), domain.NewError(v1.CodeInvalidRequest, "必须给出 policy"))
		return
	}

	result, err := s.deps.Backups.Prune(r.Context(), req.Policy, req.DryRun)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.BackupPruneResponse{
		APIVersion:       v1.APIVersion,
		DryRun:           result.DryRun,
		Policy:           result.Policy,
		Removed:          emptyIfNil(result.Removed),
		Skipped:          pruneSkipsDTO(result.Skipped),
		Kept:             result.Kept,
		FreedBytes:       result.FreedBytes,
		IgnoredRetention: result.IgnoredRetention,
		Truncated:        result.Truncated,
	})
}

func pruneSkipsDTO(skips []application.PruneSkip) []v1.BackupPruneSkip {
	out := make([]v1.BackupPruneSkip, 0, len(skips))
	for _, skip := range skips {
		out = append(out, v1.BackupPruneSkip{BackupID: skip.BackupID, Reason: skip.Reason})
	}
	return out
}

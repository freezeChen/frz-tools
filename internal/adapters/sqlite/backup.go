package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

const backupPolicyColumns = `id, name, policy_json, created_at, updated_at, COALESCE(updated_by, '')`

const backupColumns = `id, policy_id, COALESCE(operation_id, ''), status, COALESCE(storage_digest, ''),
	COALESCE(logical_bytes, 0), COALESCE(stored_bytes, 0), COALESCE(compression, ''),
	COALESCE(encryption_key_id, ''), resource_kind, COALESCE(server_version, ''),
	COALESCE(client_version, ''), COALESCE(tool, ''), labels_json,
	started_at, finished_at, verified_at, verified_ok, COALESCE(error_code, ''), COALESCE(error_message, '')`

// SaveBackupPolicy 以 name 为键 upsert 策略，并返回落库后的形态（含 id）。
//
// id 只在**首次插入**时使用：命中冲突时保留原有 id，否则每次覆盖都会换一个 id，
// 而 backups.policy_id 是指向它的外键——换 id 等于把历史备份的归属改掉。
func (s *Store) SaveBackupPolicy(ctx context.Context, policy *domain.BackupPolicy, policyID string, now time.Time, updatedBy string) (*domain.BackupPolicy, error) {
	encoded, err := json.Marshal(policy)
	if err != nil {
		return nil, domain.NewError(v1.CodeInternal, "无法序列化备份策略: %v", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO backup_policies (id, name, policy_json, created_at, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			policy_json = excluded.policy_json,
			updated_at  = excluded.updated_at,
			updated_by  = excluded.updated_by`,
		policyID, policy.Name, string(encoded), formatTime(now), formatTime(now), nullString(updatedBy)); err != nil {
		return nil, err
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		EventType: domain.EventBackupPolicySaved,
		Actor:     updatedBy,
		Resource:  policy.Name,
		Result:    "saved",
		Time:      now,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetBackupPolicy(ctx, policy.Name)
}

// GetBackupPolicy 按 id 或 name 取策略。它与 applications 一样两者都认——
// CLI 与 API 里用户写的是名字，而 backups 表存的是 id。
func (s *Store) GetBackupPolicy(ctx context.Context, ref string) (*domain.BackupPolicy, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+backupPolicyColumns+` FROM backup_policies WHERE id = ? OR name = ?`, ref, ref)

	var (
		id, name, raw, createdAt, updatedAt string
		updatedBy                           string
	)
	err := row.Scan(&id, &name, &raw, &createdAt, &updatedAt, &updatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NewError(v1.CodeBackupNotFound, "备份策略 %q 不存在", ref)
	}
	if err != nil {
		return nil, err
	}

	policy := &domain.BackupPolicy{}
	if err := json.Unmarshal([]byte(raw), policy); err != nil {
		// 落库的 JSON 由本进程写出，解不开说明库被外部改过或写坏，属内部不一致。
		return nil, domain.NewError(v1.CodeInternal, "备份策略 %q 的存储内容无法解析: %v", name, err)
	}
	policy.ID = id
	return policy, nil
}

func (s *Store) ListBackupPolicies(ctx context.Context) ([]domain.BackupPolicy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+backupPolicyColumns+` FROM backup_policies ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.BackupPolicy
	for rows.Next() {
		var (
			id, name, raw, createdAt, updatedAt string
			updatedBy                           string
		)
		if err := rows.Scan(&id, &name, &raw, &createdAt, &updatedAt, &updatedBy); err != nil {
			return nil, err
		}
		policy := domain.BackupPolicy{}
		if err := json.Unmarshal([]byte(raw), &policy); err != nil {
			return nil, domain.NewError(v1.CodeInternal, "备份策略 %q 的存储内容无法解析: %v", name, err)
		}
		policy.ID = id
		out = append(out, policy)
	}
	return out, rows.Err()
}

func (s *Store) CreateBackup(ctx context.Context, backup *domain.Backup) error {
	labels, err := encodeLabels(backup.Labels)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO backups (
			id, policy_id, operation_id, status, storage_digest, logical_bytes, stored_bytes,
			compression, encryption_key_id, resource_kind, server_version, client_version, tool,
			labels_json, started_at, finished_at, verified_at, verified_ok, error_code, error_message
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		backup.ID, backup.PolicyID, nullString(backup.OperationID), string(backup.Status),
		nullDigest(backup.StorageDigest), backup.LogicalBytes, backup.StoredBytes,
		nullString(string(backup.Compression)), nullString(backup.EncryptionKeyID),
		string(backup.ResourceKind), nullString(backup.ServerVersion), nullString(backup.ClientVersion),
		nullString(backup.Tool), labels, formatTime(backup.StartedAt),
		nullTime(backup.FinishedAt), nullTime(backup.VerifiedAt), nullBool(backup.VerifiedOK),
		nullString(backup.ErrorCode), nullString(backup.ErrorMessage))
	return err
}

// FinishBackup 把一次备份推进到终态。若它已经不在 pending/running，则为空操作——
// 与 Operation 的 Finish 同样的语义，避免「重复收尾」把终态改回去。
func (s *Store) FinishBackup(ctx context.Context, in domain.FinishBackupInput, now time.Time) (*domain.Backup, error) {
	labels, err := encodeLabels(in.Labels)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 审计事件的 operation_id 有指向 operations(id) 的外键，因此这里要填**发起这次备份
	// 的那个 Operation**，而不是备份自己的 ID。备份 ID 进 details，两个方向都可查。
	var operationID sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT operation_id FROM backups WHERE id = ?`, in.BackupID).Scan(&operationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.NewError(v1.CodeBackupNotFound, "备份 %q 不存在", in.BackupID)
		}
		return nil, err
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE backups SET
			status = ?, storage_digest = ?, logical_bytes = ?, stored_bytes = ?, compression = ?,
			encryption_key_id = ?, server_version = ?, client_version = ?, tool = ?,
			labels_json = ?, finished_at = ?, error_code = ?, error_message = ?
		WHERE id = ? AND status IN ('pending', 'running')`,
		string(in.Status), nullDigest(in.StorageDigest), in.LogicalBytes, in.StoredBytes,
		nullString(string(in.Compression)), nullString(in.EncryptionKeyID),
		nullString(in.ServerVersion), nullString(in.ClientVersion), nullString(in.Tool),
		labels, formatTime(now), nullString(in.ErrorCode), nullString(in.ErrorMessage),
		in.BackupID)
	if err != nil {
		return nil, err
	}
	if affected, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if affected == 0 {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return s.GetBackup(ctx, in.BackupID)
	}

	// errorDetail 在没有错误码时返回 nil（成功路径就是这种），直接赋值会 panic。
	details := errorDetail(in.ErrorCode)
	if details == nil {
		details = map[string]string{}
	}
	details["backupId"] = in.BackupID
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		EventType:   backupEventType(in.Status),
		OperationID: operationID.String,
		Result:      string(in.Status),
		Time:        now,
		Details:     details,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetBackup(ctx, in.BackupID)
}

func (s *Store) GetBackup(ctx context.Context, id string) (*domain.Backup, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+backupColumns+` FROM backups WHERE id = ?`, id)
	backup, err := scanBackup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NewError(v1.CodeBackupNotFound, "备份 %q 不存在", id)
	}
	if err != nil {
		return nil, err
	}
	return backup, nil
}

// ListBackups 按完成时间倒序列出备份；policyRef 为空表示不限策略。
// limit <= 0 时用默认上限，避免一次把整张表读出来。
func (s *Store) ListBackups(ctx context.Context, policyRef string, limit int) ([]domain.Backup, error) {
	if limit <= 0 {
		limit = 100
	}

	query := `SELECT ` + backupColumns + ` FROM backups`
	var args []any
	if policyRef != "" {
		query += ` WHERE policy_id = (SELECT id FROM backup_policies WHERE id = ? OR name = ?)`
		args = append(args, policyRef, policyRef)
	}
	query += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Backup
	for rows.Next() {
		backup, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *backup)
	}
	return out, rows.Err()
}

// FailStaleBackups 把上一个守护进程实例遗留的 running/pending 备份标记为失败。
//
// 没有这一步的话，被杀死的那次备份会永远停在 running——它不会被当成有效备份
// （Usable 要求 succeeded），但会一直挂在列表里，让运维分不清「在跑」还是「早就死了」。
func (s *Store) FailStaleBackups(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE backups SET status = ?, error_code = ?, error_message = ?, finished_at = ?
		WHERE status IN ('pending', 'running')`,
		string(domain.BackupFailed), string(v1.CodeDaemonRestarted),
		"daemon restarted while the backup was running", formatTime(now))
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// MarkBackupVerified 记录一次校验的结果。校验**不通过**也要落库：
// 「这份备份校验过、但没通过」与「从没校验过」是两件完全不同的事。
func (s *Store) MarkBackupVerified(ctx context.Context, id string, ok bool, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE backups SET verified_at = ?, verified_ok = ? WHERE id = ?`,
		formatTime(now), boolToInt(ok), id)
	return err
}

func scanBackup(sc scanner) (*domain.Backup, error) {
	var (
		backup       domain.Backup
		status       string
		digest       sql.NullString
		compression  sql.NullString
		labels       sql.NullString
		startedAt    string
		finishedAt   sql.NullString
		verifiedAt   sql.NullString
		verifiedOK   sql.NullInt64
		errorCode    sql.NullString
		errorMessage sql.NullString
	)
	if err := sc.Scan(
		&backup.ID, &backup.PolicyID, &backup.OperationID, &status, &digest,
		&backup.LogicalBytes, &backup.StoredBytes, &compression, &backup.EncryptionKeyID,
		&backup.ResourceKind, &backup.ServerVersion, &backup.ClientVersion, &backup.Tool,
		&labels, &startedAt, &finishedAt, &verifiedAt, &verifiedOK, &errorCode, &errorMessage,
	); err != nil {
		return nil, err
	}

	backup.Status = domain.BackupStatus(status)
	if digest.Valid {
		backup.StorageDigest = domain.Digest(digest.String)
	}
	backup.Compression = domain.Compression(compression.String)
	backup.ErrorCode = errorCode.String
	backup.ErrorMessage = errorMessage.String

	decodedLabels, err := decodeLabels(labels)
	if err != nil {
		return nil, domain.NewError(v1.CodeInternal, "备份 %q 的 labels 无法解析: %v", backup.ID, err)
	}
	backup.Labels = decodedLabels

	if backup.StartedAt, err = parseTime(startedAt); err != nil {
		return nil, err
	}
	if finishedAt.Valid {
		t, err := parseTime(finishedAt.String)
		if err != nil {
			return nil, err
		}
		backup.FinishedAt = &t
	}
	if verifiedAt.Valid {
		t, err := parseTime(verifiedAt.String)
		if err != nil {
			return nil, err
		}
		backup.VerifiedAt = &t
	}
	if verifiedOK.Valid {
		value := verifiedOK.Int64 != 0
		backup.VerifiedOK = &value
	}
	return &backup, nil
}

func nullDigest(d domain.Digest) any {
	if d == "" {
		return nil
	}
	return d.String()
}

func nullBool(b *bool) any {
	if b == nil {
		return nil
	}
	return boolToInt(*b)
}

func backupEventType(status domain.BackupStatus) string {
	switch status {
	case domain.BackupSucceeded:
		return domain.EventBackupSucceeded
	case domain.BackupFailed:
		return domain.EventBackupFailed
	case domain.BackupPruned:
		return domain.EventBackupPruned
	default:
		return domain.EventBackupFinished
	}
}

// ListBackupsForRetention 取一份策略下的备份，**按完成时刻倒序**，供保留计算使用。
//
// 三处刻意的选择：
//
//  1. **包含非 succeeded 的行**。过滤交给领域层的 `Usable()`——「谁参与保留计算」只有
//     一处判据，也才测得出来。在 SQL 里再写一遍那个条件，就成了第二份会漂移的真相。
//  2. 按**完成时刻**排而不是开始时刻：保留策略的判据是「这份备份什么时候备好的」。
//  3. `started_at` 那个 LIMIT 的默认值不适用：prune 要看**全部**候选，截断会漏掉最老的
//     那些（漏掉的只是不删，方向保守，但必须由调用方知道——见 limit 参数与返回值）。
func (s *Store) ListBackupsForRetention(ctx context.Context, policyRef string, limit int) ([]domain.Backup, error) {
	if limit <= 0 {
		limit = 100
	}

	query := `SELECT ` + backupColumns + ` FROM backups
		WHERE policy_id = (SELECT id FROM backup_policies WHERE id = ? OR name = ?)
		ORDER BY finished_at DESC, id DESC LIMIT ?`

	rows, err := s.db.QueryContext(ctx, query, policyRef, policyRef, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Backup
	for rows.Next() {
		backup, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *backup)
	}
	return out, rows.Err()
}

// CountBackupReferencesByDigest 数**还有多少份未删除的备份**指向同一个 digest。
//
// prune 在删内容之前必须问这一句：内容寻址是按内容去重的，加密关闭时两份不同的备份
// 可能共享同一个 blob。少了这道检查，删 A 策略的一份备份会顺手毁掉 B 策略的一份——
// 而且是静默的，只在真要恢复时才发现。
//
// 数的是**未被 pruned** 的行：调用方先标记自己那一份，再问这一句，于是"还剩谁指着它"
// 天然把刚标记的那份排除在外。
func (s *Store) CountBackupReferencesByDigest(ctx context.Context, digest domain.Digest) (int, error) {
	if digest == "" {
		return 0, nil
	}
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM backups
		WHERE storage_digest = ? AND status <> 'pruned'`, digest.String()).Scan(&count)
	return count, err
}

// HasIncompleteOperation 报告某个资源上还有没有未完成的 Operation。
//
// 直接复用 CreateOperation 判 LOCK_BUSY 用的那个助手：prune 要问的正是同一个问题
// ——「这个资源现在被占着吗」，而两套判据迟早会漂移出第三种行为。
func (s *Store) HasIncompleteOperation(ctx context.Context, resource string) (bool, error) {
	return hasIncompleteOperation(ctx, s.db, resource)
}

// MarkBackupPruned 把一份备份标记为已清理，并写下审计事件。
//
// 只改元数据、不碰内容：调用方随后单独判断内容能不能删（可能还有别的记录指着它）。
// 两步分开是 D9 的取舍——中断在最坏的情况下留下一个无人认领的 blob，而不是
// 「元数据说内容在、内容没了」。
//
// 幂等：行已经不是 succeeded（例如并发下已经被别的 prune 标记过）时返回 nil 且不写审计。
// prune 必须可重跑，把「已经被清理过」当成错误会让一次中断卡住后续所有 prune。
func (s *Store) MarkBackupPruned(ctx context.Context, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 审计事件的 operation_id 指向 operations(id)，而 prune 不是 Operation，因此留空。
	// 备份 ID 进 details，两个方向都可查（与 FinishBackup 同一套做法）。
	res, err := tx.ExecContext(ctx,
		`UPDATE backups SET status = 'pruned' WHERE id = ? AND status = 'succeeded'`, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return nil
	}
	if err := insertAudit(ctx, tx, domain.AuditEvent{
		EventType: domain.EventBackupPruned,
		Resource:  id,
		Result:    string(domain.BackupPruned),
		Time:      now,
		Details:   map[string]string{"backupId": id},
	}); err != nil {
		return err
	}
	return tx.Commit()
}

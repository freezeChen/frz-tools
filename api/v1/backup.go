package v1

import "time"

// PutBackupPolicyRequest 提交一份 BackupPolicy manifest。
//
// manifest 以**原文**传送而不是结构化字段，与 `PUT /applications/{id}/spec` 同样的理由：
// 严格解码（KnownFields）与迁移链是唯一能挡住「未知字段被静默丢弃」的入口，
// 因此解析必须发生在服务端、由 domain 那一份校验规则负责。
type PutBackupPolicyRequest struct {
	Manifest  string `json:"manifest"`
	UpdatedBy string `json:"updatedBy,omitempty"`
}

// BackupPolicy 是策略的对外形态，供 `backup policy get/list` 展示。
// 它逐字段对应 manifest，只是把 YAML 的形式换成 camelCase 的 JSON。
type BackupPolicy struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Resource  BackupPolicySource `json:"resource"`
	Encoding  BackupEncodingSpec `json:"encoding"`
	Retention BackupRetention    `json:"retention"`
	Timeout   BackupTimeoutSpec  `json:"timeout"`
}

type BackupPolicySource struct {
	Kind      string     `json:"kind"`
	Paths     []string   `json:"paths,omitempty"`
	Exclude   []string   `json:"exclude,omitempty"`
	Symlinks  string     `json:"symlinks,omitempty"`
	DSNSecret *SecretRef `json:"dsnSecret,omitempty"`
	Database  string     `json:"database,omitempty"`
}

type BackupEncodingSpec struct {
	Compression string               `json:"compression"`
	Encryption  BackupEncryptionSpec `json:"encryption"`
}

// BackupEncryptionSpec 里只有**引用**，没有密钥本身——连形态上都没有地方放明文。
type BackupEncryptionSpec struct {
	Enabled   bool       `json:"enabled"`
	KeySecret *SecretRef `json:"keySecret,omitempty"`
}

type BackupRetention struct {
	KeepLast int       `json:"keepLast,omitempty"`
	KeepDays int       `json:"keepDays,omitempty"`
	GFS      BackupGFS `json:"gfs,omitempty"`
}

type BackupGFS struct {
	Daily   int `json:"daily,omitempty"`
	Weekly  int `json:"weekly,omitempty"`
	Monthly int `json:"monthly,omitempty"`
}

type BackupTimeoutSpec struct {
	BackupSeconds  int `json:"backupSeconds"`
	RestoreSeconds int `json:"restoreSeconds"`
}

type BackupPolicyResponse struct {
	APIVersion string       `json:"apiVersion"`
	Policy     BackupPolicy `json:"policy"`
}

type BackupPolicyListResponse struct {
	APIVersion string         `json:"apiVersion"`
	Items      []BackupPolicy `json:"items"`
}

// Backup 是一次备份的对外形态。
//
// logicalBytes 与 storedBytes 都给出，因为它们回答的是两个不同的问题
// ——「压缩比多少」与「占了多少盘」。
type Backup struct {
	ID              string            `json:"id"`
	PolicyID        string            `json:"policyId"`
	OperationID     string            `json:"operationId,omitempty"`
	Status          string            `json:"status"`
	StorageDigest   string            `json:"storageDigest,omitempty"`
	LogicalBytes    int64             `json:"logicalBytes"`
	StoredBytes     int64             `json:"storedBytes"`
	Compression     string            `json:"compression,omitempty"`
	EncryptionKeyID string            `json:"encryptionKeyId,omitempty"`
	ResourceKind    string            `json:"resourceKind"`
	ServerVersion   string            `json:"serverVersion,omitempty"`
	ClientVersion   string            `json:"clientVersion,omitempty"`
	Tool            string            `json:"tool,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	VerifiedOK      *bool             `json:"verifiedOk,omitempty"`
	StartedAt       time.Time         `json:"startedAt"`
	FinishedAt      *time.Time        `json:"finishedAt,omitempty"`
	VerifiedAt      *time.Time        `json:"verifiedAt,omitempty"`
	ErrorCode       string            `json:"errorCode,omitempty"`
	ErrorMessage    string            `json:"errorMessage,omitempty"`
}

type BackupResponse struct {
	APIVersion string `json:"apiVersion"`
	Backup     Backup `json:"backup"`
}

type BackupListResponse struct {
	APIVersion string   `json:"apiVersion"`
	Items      []Backup `json:"items"`
}

// BackupRunRequest 触发一次备份。Retry 省略即不自动重试（与 operation 提交一致）。
//
// 策略名在 body 里而不是路径上：`POST /api/v1/backups` 这一条没有路径参数
// （另两个端点的路径参数是备份 ID，不是策略）。
type BackupRunRequest struct {
	Policy         string     `json:"policy"`
	IdempotencyKey string     `json:"idempotencyKey,omitempty"`
	CreatedBy      string     `json:"createdBy,omitempty"`
	Retry          *RetrySpec `json:"retry,omitempty"`
}

// BackupVerifyRequest 校验一份备份。
type BackupVerifyRequest struct {
	IdempotencyKey string     `json:"idempotencyKey,omitempty"`
	CreatedBy      string     `json:"createdBy,omitempty"`
	Retry          *RetrySpec `json:"retry,omitempty"`
}

// BackupRestoreRequest 恢复一份备份。
//
// Confirm 是原地恢复的**显式确认**：它是本工具破坏性最强的动作，会覆盖真实数据
// （迭代 2 规格 D7）。服务端在**创建期**就检查它。
type BackupRestoreRequest struct {
	Mode           string     `json:"mode"`
	Confirm        bool       `json:"confirm,omitempty"`
	IdempotencyKey string     `json:"idempotencyKey,omitempty"`
	CreatedBy      string     `json:"createdBy,omitempty"`
	Retry          *RetrySpec `json:"retry,omitempty"`
}

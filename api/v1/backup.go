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

// BackupPruneRequest 触发一次按保留策略的清理。
//
// 它是**同步**用例而不是 Operation（与 1a 的制品 GC 一致，见规格决定 5），因此响应里
// 直接给结果，不返回 operation id。
type BackupPruneRequest struct {
	// Policy 是保留策略名（必填）。保留规则是每份策略自己的声明，因此清理也按策略进行——
	// 一次跨策略的清扫不该长在一个名字看起来只作用于一份策略的命令里。
	Policy string `json:"policy"`
	DryRun bool   `json:"dryRun,omitempty"`
}

// BackupPruneSkip 是一份应当删、但本轮没删的备份及其原因。
//
// 它必须出现在响应里而不是只写日志：运维看到「该删 200 份、实际删了 198 份」时，
// 第一个问题就是那两份去哪了。
type BackupPruneSkip struct {
	BackupID string `json:"backupId"`
	Reason   string `json:"reason"`
}

// BackupPruneResponse 是一次清理的结果，形状与制品 GC 的响应同源。
type BackupPruneResponse struct {
	APIVersion string `json:"apiVersion"`
	DryRun     bool   `json:"dryRun"`
	Policy     string `json:"policy"`
	// Removed 是**本轮标记为已清理**的备份 ID；dry-run 时是「将要标记」的那些。
	Removed []string `json:"removed"`
	// Skipped 是应当删但没删的那些（正在被操作、内容仍被引用）。
	Skipped []BackupPruneSkip `json:"skipped"`
	// Kept 是保留策略保下来的份数。
	Kept int `json:"kept"`
	// FreedBytes 是实际释放（dry-run 时是预计释放）的字节数。
	FreedBytes int64 `json:"freedBytes"`
	// IgnoredRetention 是策略里声明了、而这一版没有实现的保留项（当前只可能是 gfs）。
	// 它是「这个策略里有一部分规则没生效」的唯一信号，因此不能只写日志。
	IgnoredRetention []string `json:"ignoredRetention,omitempty"`
	// Truncated 表示扫描撞到了上限，**更老的**备份可能没被看到（因此没被删）。
	Truncated bool `json:"truncated,omitempty"`
}

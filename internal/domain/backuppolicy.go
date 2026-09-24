package domain

import (
	"path"
	"regexp"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// BackupPolicyAPIVersion 与 Kind 是 BackupPolicy manifest 的版本信封。
// 与 ApplicationSpec 用同一套做法（见 internal/adapters/manifest）：先非严格解码读出
// 信封，按版本走迁移链，再用严格解码，好让「新增可选字段」保持兼容、而删改语义必须
// 提升 apiVersion。
const (
	BackupPolicyAPIVersion = "ops.frz.io/v1alpha1"
	BackupPolicyKind       = "BackupPolicy"
)

// BackupResourceKind 是备份资源的种类。2a 只实现 files；postgres / mysql 由 2b 加入，
// 走同一个 BackupAdapter 接口，因此新增数据库不需要改调度器与存储层。
type BackupResourceKind string

const (
	BackupResourceFiles    BackupResourceKind = "files"
	BackupResourcePostgres BackupResourceKind = "postgres"
	BackupResourceMySQL    BackupResourceKind = "mysql"
)

// SymlinkPolicy 是备份遇到符号链接时的行为。
//
// 默认刻意是 skip：follow 会让备份顺着链接跑出 paths 声明的范围（可能读到 /etc 或
// 别的应用的目录），那是**显式承担风险**的选择，必须由用户写下来。
type SymlinkPolicy string

const (
	SymlinkSkip   SymlinkPolicy = "skip"
	SymlinkFollow SymlinkPolicy = "follow"
	SymlinkError  SymlinkPolicy = "error"
)

// Compression 是传输编码里的压缩方式。
//
// 压缩由**应用层**统一执行，不是适配器的事：用数据库原生归档格式（如 pg_dump -Fc）时
// 压缩已由工具完成，那时声明 none 由应用层跳过，避免压两遍。
type Compression string

const (
	CompressionNone Compression = "none"
	CompressionGzip Compression = "gzip"
)

// RestoreMode 决定恢复到哪儿。默认只允许隔离恢复：inPlace 会覆盖真实数据，
// 是本工具里破坏性最强的动作。
type RestoreMode string

const (
	RestoreIsolated RestoreMode = "isolated"
	RestoreInPlace  RestoreMode = "inPlace"
)

func (m RestoreMode) Valid() bool { return m == RestoreIsolated || m == RestoreInPlace }

// 备份的默认值。与 1d 的退避一样，取值冻结在规格里，改动要先改规格。
const (
	DefaultBackupTimeout  = time.Hour
	DefaultRestoreTimeout = 2 * time.Hour
	DefaultCompression    = CompressionGzip
)

type BackupPolicy struct {
	// ID 由仓储在首次落库时分配；提交上来的 manifest 里没有它（与 Application 一致）。
	ID         string
	APIVersion string
	Kind       string
	Name       string
	Resource   BackupResource
	Encoding   BackupEncoding
	Retention  BackupRetention
	Timeout    BackupTimeout
}

type BackupResource struct {
	Kind BackupResourceKind

	// kind=files
	Paths    []string
	Exclude  []string
	Symlinks SymlinkPolicy

	// kind=postgres | mysql（2b 起）
	DSNSecret SecretRef
	Database  string
}

type BackupEncoding struct {
	Compression Compression
	Encryption  BackupEncryption
}

// BackupEncryption 的密钥**只以引用形式出现**：字段是 SecretRef（kind + name），
// 结构上就没有地方可以内联明文，因此「密钥被写进数据库与审计」这件事不是靠字符串校验
// 拦住的，而是没有表达方式。规格第 4 节的 YAML 把它简写成裸名字，这里补上 kind，
// 理由与 Decision 4「凭据只经 SecretRef」一致。
type BackupEncryption struct {
	Enabled   bool
	KeySecret SecretRef
}

// BackupRetention 是 GFS 保留策略。至少要给出一项，否则备份会无限增长。
type BackupRetention struct {
	KeepLast int
	KeepDays int
	GFS      BackupGFS
}

type BackupGFS struct {
	Daily   int
	Weekly  int
	Monthly int
}

// Empty 报告保留策略是否什么都没说。
func (r BackupRetention) Empty() bool {
	return r.KeepLast <= 0 && r.KeepDays <= 0 &&
		r.GFS.Daily <= 0 && r.GFS.Weekly <= 0 && r.GFS.Monthly <= 0
}

type BackupTimeout struct {
	Backup  time.Duration
	Restore time.Duration
}

func (p *BackupPolicy) Validate() error {
	// name 沿用 application 的字符集，理由相同：它是安全边界而不是格式偏好——
	// 会进入存储前缀、审计与日志。
	if !applicationNamePattern.MatchString(p.Name) {
		return NewError(v1.CodeManifestInvalid,
			"name 只允许字母、数字、点、下划线与连字符，且必须以字母或数字开头（got %q）", p.Name)
	}
	if p.Kind != BackupPolicyKind {
		return NewError(v1.CodeManifestInvalid,
			"kind 必须是 %q，got %q", BackupPolicyKind, p.Kind)
	}

	if err := p.Resource.validate(); err != nil {
		return err
	}
	if err := p.Encoding.validate(); err != nil {
		return err
	}
	if err := p.Retention.validate(); err != nil {
		return err
	}
	if err := p.Timeout.validate(); err != nil {
		return err
	}

	p.applyDefaults()
	return nil
}

func (r *BackupResource) validate() error {
	switch r.Kind {
	case BackupResourceFiles:
		if len(r.Paths) == 0 {
			return NewError(v1.CodeManifestInvalid, "resource.paths 不能为空")
		}
		for i, p := range r.Paths {
			if err := validateBackupPath(p); err != nil {
				return NewError(v1.CodeManifestInvalid, "resource.paths[%d]: %v", i, err)
			}
		}
		for i, pattern := range r.Exclude {
			if path.IsAbs(pattern) {
				return NewError(v1.CodeManifestInvalid,
					"resource.exclude[%d] 必须是相对模式，不能是绝对路径（got %q）", i, pattern)
			}
		}
		switch r.Symlinks {
		case SymlinkSkip, SymlinkFollow, SymlinkError, "":
		default:
			return NewError(v1.CodeManifestInvalid, "resource.symlinks 取值非法: %q", r.Symlinks)
		}
	case BackupResourcePostgres, BackupResourceMySQL:
		if err := r.DSNSecret.Validate(); err != nil {
			// 刻意固定成 MANIFEST_INVALID：用户提交的是 manifest，出错的也是 manifest，
			// 透传 SecretRef 的内部错误码会让调用方看到与提交内容不对应的错误类型。
			return NewError(v1.CodeManifestInvalid, "resource.dsnSecret: %v", err)
		}
		if strings.TrimSpace(r.Database) == "" {
			return NewError(v1.CodeManifestInvalid, "resource.database 不能为空")
		}
		if !databaseNamePattern.MatchString(r.Database) {
			return NewError(v1.CodeManifestInvalid,
				"resource.database 只能是字母、数字、下划线与 $，且不以数字开头（got %q）", r.Database)
		}
		if len(r.Paths) > 0 || len(r.Exclude) > 0 || r.Symlinks != "" {
			return NewError(v1.CodeManifestInvalid,
				"resource.kind=%s 时不能提供 paths/exclude/symlinks（那些只属于 files）", r.Kind)
		}
	default:
		return NewError(v1.CodeManifestInvalid, "resource.kind 取值非法: %q", r.Kind)
	}

	// files 与数据库资源不得混填，避免「看起来配了但一半没生效」。
	if r.Kind == BackupResourceFiles {
		if r.DSNSecret.Name != "" || strings.TrimSpace(r.Database) != "" {
			return NewError(v1.CodeManifestInvalid,
				"resource.kind=files 时不能提供 dsnSecret/database")
		}
	}
	return nil
}

// databaseNamePattern 限定库名的字符集。
//
// 这是**安全边界**而不是格式偏好：库名会进命令行（mysql 的位置参数）与连接串
// （postgres 的 URI 路径）。一个以 `-` 开头的库名会被 MySQL 客户端当成旗标解析——
// 那是一条从 manifest 通往任意客户端选项的注入路径。执行器只接受 argv、永不经过 shell，
// 因此它挡得住 shell 注入，但**挡不住旗标注入**，这里补上。
// 同时排除了点号，免得出现 `db.table` 这类在各家客户端里含义不同的写法。
var databaseNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

// validateBackupPath 拒绝相对路径与包含 .. 的路径。
//
// 这是安全边界：备份会把 paths 下的内容读出来写成归档，一个 `..` 就意味着能读到
// 声明范围之外的东西（例如 /etc/shadow）。所以只接受干净的绝对路径。
func validateBackupPath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return NewError(v1.CodeManifestInvalid, "必须是绝对路径（got %q）", p)
	}
	if !path.IsAbs(path.Clean(p)) {
		return NewError(v1.CodeManifestInvalid, "路径非法（got %q）", p)
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." {
			return NewError(v1.CodeManifestInvalid, "路径不得包含 ..（got %q）", p)
		}
	}
	if path.Clean(p) == "/" {
		return NewError(v1.CodeManifestInvalid, "不允许把整个根目录作为备份范围")
	}
	return nil
}

func (e *BackupEncoding) validate() error {
	switch e.Compression {
	case CompressionNone, CompressionGzip, "":
	default:
		return NewError(v1.CodeManifestInvalid, "encoding.compression 取值非法: %q", e.Compression)
	}
	if e.Encryption.Enabled {
		// 密钥解析失败时**绝不降级为不加密**：静默产出明文备份是最坏的结果。
		// 因此这里就把「开了加密却没给密钥」挡在提交期。
		if err := e.Encryption.KeySecret.Validate(); err != nil {
			return NewError(v1.CodeManifestInvalid, "encoding.encryption.keySecret: %v", err)
		}
	} else if e.Encryption.KeySecret.Name != "" {
		return NewError(v1.CodeManifestInvalid,
			"encoding.encryption.enabled=false 时不应提供 keySecret（给了也不会被使用）")
	}
	return nil
}

func (r *BackupRetention) validate() error {
	if r.Empty() {
		return NewError(v1.CodeManifestInvalid,
			"retention 至少要给出一项（keepLast / keepDays / gfs），否则备份会无限增长")
	}
	for _, item := range []struct {
		name  string
		value int
	}{
		{"keepLast", r.KeepLast},
		{"keepDays", r.KeepDays},
		{"gfs.daily", r.GFS.Daily},
		{"gfs.weekly", r.GFS.Weekly},
		{"gfs.monthly", r.GFS.Monthly},
	} {
		if item.value < 0 {
			return NewError(v1.CodeManifestInvalid, "retention.%s 不能为负数", item.name)
		}
	}
	return nil
}

func (t *BackupTimeout) validate() error {
	if t.Backup < 0 || t.Restore < 0 {
		return NewError(v1.CodeManifestInvalid, "timeout 必须为正数")
	}
	return nil
}

func (p *BackupPolicy) applyDefaults() {
	if p.Encoding.Compression == "" {
		p.Encoding.Compression = DefaultCompression
	}
	if p.Resource.Kind == BackupResourceFiles && p.Resource.Symlinks == "" {
		p.Resource.Symlinks = SymlinkSkip
	}
	if p.Timeout.Backup == 0 {
		p.Timeout.Backup = DefaultBackupTimeout
	}
	if p.Timeout.Restore == 0 {
		p.Timeout.Restore = DefaultRestoreTimeout
	}
}

func (p *BackupPolicy) String() string {
	return "BackupPolicy(" + p.Name + "/" + string(p.Resource.Kind) + ")"
}

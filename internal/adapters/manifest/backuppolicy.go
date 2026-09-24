package manifest

import (
	"bytes"
	"io"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// backupPolicyMigrations 是 BackupPolicy 自己的迁移链，与 ApplicationSpec 的分开登记——
// 两种 manifest 的演进节奏彼此无关，共用一张表会让「为 A 登记迁移」意外影响 B。
// 当前只有一版，因此为空。
var backupPolicyMigrations = map[string]func(doc map[string]any) error{}

type wireBackupPolicy struct {
	APIVersion string              `yaml:"apiVersion"`
	Kind       string              `yaml:"kind"`
	Name       string              `yaml:"name"`
	Resource   wireBackupResource  `yaml:"resource"`
	Encoding   wireBackupEncoding  `yaml:"encoding"`
	Retention  wireBackupRetention `yaml:"retention"`
	Timeout    wireBackupTimeout   `yaml:"timeout"`
}

type wireBackupResource struct {
	Kind      string         `yaml:"kind"`
	Paths     []string       `yaml:"paths"`
	Exclude   []string       `yaml:"exclude"`
	Symlinks  string         `yaml:"symlinks"`
	DSNSecret *wireSecretRef `yaml:"dsnSecret"`
	Database  string         `yaml:"database"`
}

type wireBackupEncoding struct {
	Compression string               `yaml:"compression"`
	Encryption  wireBackupEncryption `yaml:"encryption"`
}

type wireBackupEncryption struct {
	// Enabled 用指针而不是 bool：**加密默认开启**（迭代 2 规格决定 3），
	// 而 bool 的零值是「关闭」，会把「没写」静默解释成「不加密」——那正好是
	// 最危险的方向。指针才能区分「没写」（默认开）与「写了 false」（显式关）。
	Enabled   *bool          `yaml:"enabled"`
	KeySecret *wireSecretRef `yaml:"keySecret"`
}

type wireBackupRetention struct {
	KeepLast int           `yaml:"keepLast"`
	KeepDays int           `yaml:"keepDays"`
	GFS      wireBackupGFS `yaml:"gfs"`
}

type wireBackupGFS struct {
	Daily   int `yaml:"daily"`
	Weekly  int `yaml:"weekly"`
	Monthly int `yaml:"monthly"`
}

// wireBackupTimeout 用**秒数**而不是 `1h` 这样的时长字符串，与 api/v1 既有的
// `intervalSeconds` / `startTimeoutSeconds` / `baseDelaySeconds` 保持一致。
// 仓库里同一种概念只该有一种 wire 写法，否则调用方要为每个字段记一套规则。
type wireBackupTimeout struct {
	BackupSeconds  int `yaml:"backupSeconds"`
	RestoreSeconds int `yaml:"restoreSeconds"`
}

// ParseBackupPolicy 解析 BackupPolicy manifest（`kind: BackupPolicy`）。
//
// 与 ApplicationSpec 走同一套解码流程（信封 → 迁移链 → 严格解码），见 decode.go。
func ParseBackupPolicy(data []byte) (*domain.BackupPolicy, error) {
	return ParseBackupPolicyReader(bytes.NewReader(data))
}

func ParseBackupPolicyReader(r io.Reader) (*domain.BackupPolicy, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, domain.NewError(v1.CodeManifestInvalid, "读取 BackupPolicy 失败: %v", err)
	}

	var wire wireBackupPolicy
	if _, err := decodeManifest(raw, domain.BackupPolicyKind, domain.BackupPolicyAPIVersion,
		backupPolicyMigrations, &wire); err != nil {
		return nil, err
	}

	policy := convertBackupPolicy(&wire)
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return policy, nil
}

// encryptionEnabled 落实「加密默认开启」：没写就是开。
//
// 它带来的摩擦是刻意的——未配置密钥的部署必须显式写 `enabled: false` 才能用，
// 于是「不加密」成为一个被写下来的决定，而不是默认发生的事。
func encryptionEnabled(declared *bool) bool {
	if declared == nil {
		return true
	}
	return *declared
}

func convertBackupPolicy(wire *wireBackupPolicy) *domain.BackupPolicy {
	policy := &domain.BackupPolicy{
		APIVersion: wire.APIVersion,
		Kind:       wire.Kind,
		Name:       strings.TrimSpace(wire.Name),
		Resource: domain.BackupResource{
			Kind:     domain.BackupResourceKind(wire.Resource.Kind),
			Paths:    wire.Resource.Paths,
			Exclude:  wire.Resource.Exclude,
			Symlinks: domain.SymlinkPolicy(wire.Resource.Symlinks),
			Database: strings.TrimSpace(wire.Resource.Database),
		},
		Encoding: domain.BackupEncoding{
			Compression: domain.Compression(wire.Encoding.Compression),
			Encryption: domain.BackupEncryption{
				Enabled: encryptionEnabled(wire.Encoding.Encryption.Enabled),
			},
		},
		Retention: domain.BackupRetention{
			KeepLast: wire.Retention.KeepLast,
			KeepDays: wire.Retention.KeepDays,
			GFS: domain.BackupGFS{
				Daily:   wire.Retention.GFS.Daily,
				Weekly:  wire.Retention.GFS.Weekly,
				Monthly: wire.Retention.GFS.Monthly,
			},
		},
		Timeout: domain.BackupTimeout{
			Backup:  seconds(wire.Timeout.BackupSeconds),
			Restore: seconds(wire.Timeout.RestoreSeconds),
		},
	}
	if ref := wire.Resource.DSNSecret; ref != nil {
		policy.Resource.DSNSecret = domain.SecretRef{
			Kind: domain.SecretKind(ref.Kind),
			Name: ref.Name,
		}
	}
	if ref := wire.Encoding.Encryption.KeySecret; ref != nil {
		policy.Encoding.Encryption.KeySecret = domain.SecretRef{
			Kind: domain.SecretKind(ref.Kind),
			Name: ref.Name,
		}
	}
	return policy
}

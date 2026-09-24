package domain

import (
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// validFilesPolicy 返回一份合法的最小 files 策略，各用例在它之上做单点破坏。
func validFilesPolicy() *BackupPolicy {
	return &BackupPolicy{
		APIVersion: BackupPolicyAPIVersion,
		Kind:       BackupPolicyKind,
		Name:       "orders-nightly",
		Resource: BackupResource{
			Kind:  BackupResourceFiles,
			Paths: []string{"/var/lib/orders/data"},
		},
		Retention: BackupRetention{KeepLast: 7},
	}
}

func TestBackupPolicyValidate(t *testing.T) {
	valid := validFilesPolicy()
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法规格被拒: %v", err)
	}
	// 默认值必须被填上，而不是留空让下游各自猜。
	if valid.Encoding.Compression != DefaultCompression {
		t.Fatalf("compression 应当取默认值 %q, got %q", DefaultCompression, valid.Encoding.Compression)
	}
	if valid.Resource.Symlinks != SymlinkSkip {
		t.Fatalf("symlinks 默认应当是 %q, got %q", SymlinkSkip, valid.Resource.Symlinks)
	}
	if valid.Timeout.Backup != DefaultBackupTimeout || valid.Timeout.Restore != DefaultRestoreTimeout {
		t.Fatalf("timeout 应当取默认值, got %+v", valid.Timeout)
	}

	mutate := func(f func(p *BackupPolicy)) *BackupPolicy {
		p := validFilesPolicy()
		f(p)
		return p
	}

	cases := map[string]*BackupPolicy{
		"name 为空":            mutate(func(p *BackupPolicy) { p.Name = "" }),
		"name 以连字符开头":        mutate(func(p *BackupPolicy) { p.Name = "-bad" }),
		"name 含斜杠":           mutate(func(p *BackupPolicy) { p.Name = "a/b" }),
		"name 含空格":           mutate(func(p *BackupPolicy) { p.Name = "a b" }),
		"kind 不对":            mutate(func(p *BackupPolicy) { p.Kind = "ApplicationSpec" }),
		"resource.kind 非法":   mutate(func(p *BackupPolicy) { p.Resource.Kind = "oracle" }),
		"paths 为空":           mutate(func(p *BackupPolicy) { p.Resource.Paths = nil }),
		"paths 含相对路径":        mutate(func(p *BackupPolicy) { p.Resource.Paths = []string{"var/lib/x"} }),
		"paths 含 ..":         mutate(func(p *BackupPolicy) { p.Resource.Paths = []string{"/var/../etc"} }),
		"paths 是根目录":         mutate(func(p *BackupPolicy) { p.Resource.Paths = []string{"/"} }),
		"exclude 是绝对路径":      mutate(func(p *BackupPolicy) { p.Resource.Exclude = []string{"/etc/passwd"} }),
		"symlinks 取值非法":      mutate(func(p *BackupPolicy) { p.Resource.Symlinks = "ignore" }),
		"retention 全空":       mutate(func(p *BackupPolicy) { p.Retention = BackupRetention{} }),
		"retention 为负":       mutate(func(p *BackupPolicy) { p.Retention.KeepLast = -1 }),
		"gfs 为负":             mutate(func(p *BackupPolicy) { p.Retention.GFS.Weekly = -2 }),
		"compression 非法":     mutate(func(p *BackupPolicy) { p.Encoding.Compression = "zstd" }),
		"timeout 为负":         mutate(func(p *BackupPolicy) { p.Timeout.Backup = -time.Second }),
		"files 却给了 database": mutate(func(p *BackupPolicy) { p.Resource.Database = "orders" }),
		"files 却给了 dsnSecret": mutate(func(p *BackupPolicy) {
			p.Resource.DSNSecret = SecretRef{Kind: SecretKindFile, Name: "/etc/opsd/secrets/db"}
		}),
		// 开了加密却没给密钥：必须在提交期就挡下，绝不能等到运行时降级成明文备份。
		"加密开启但缺 keySecret": mutate(func(p *BackupPolicy) { p.Encoding.Encryption.Enabled = true }),
		// keySecret 是 SecretRef（kind + name），缺任意一半都拒绝——结构上就没有
		// 「内联明文」这个表达方式。
		"keySecret 缺 kind": mutate(func(p *BackupPolicy) {
			p.Encoding.Encryption.Enabled = true
			p.Encoding.Encryption.KeySecret = SecretRef{Name: "backup-key"}
		}),
		"keySecret 缺 name": mutate(func(p *BackupPolicy) {
			p.Encoding.Encryption.Enabled = true
			p.Encoding.Encryption.KeySecret = SecretRef{Kind: SecretKindFile}
		}),
		"关闭加密却仍给 keySecret": mutate(func(p *BackupPolicy) {
			p.Encoding.Encryption.KeySecret = SecretRef{Kind: SecretKindFile, Name: "/etc/opsd/secrets/k"}
		}),
	}
	for name, policy := range cases {
		t.Run(name, func(t *testing.T) {
			err := policy.Validate()
			if err == nil {
				t.Fatal("非法策略必须被拒绝")
			}
			if got := CodeOf(err); got != v1.CodeManifestInvalid {
				t.Fatalf("错误码 want %s, got %s", v1.CodeManifestInvalid, got)
			}
		})
	}
}

// 数据库类资源的资源字段与 files 不通用，必须互斥，避免「看起来配了但一半没生效」。
func TestBackupPolicyResourceKindFields(t *testing.T) {
	dbPolicy := func() *BackupPolicy {
		return &BackupPolicy{
			Kind: BackupPolicyKind,
			Name: "orders-db",
			Resource: BackupResource{
				Kind:      BackupResourcePostgres,
				DSNSecret: SecretRef{Kind: SecretKindFile, Name: "/etc/opsd/secrets/orders-dsn"},
				Database:  "orders",
			},
			Retention: BackupRetention{KeepLast: 3},
		}
	}

	if err := dbPolicy().Validate(); err != nil {
		t.Fatalf("合法的 postgres 策略被拒: %v", err)
	}

	withFiles := dbPolicy()
	withFiles.Resource.Paths = []string{"/var/lib/x"}
	if err := withFiles.Validate(); err == nil {
		t.Fatal("kind=postgres 时不该接受 files 的字段")
	}

	missingDB := dbPolicy()
	missingDB.Resource.Database = ""
	if err := missingDB.Validate(); err == nil {
		t.Fatal("database 不能为空")
	}

	missingDSN := dbPolicy()
	missingDSN.Resource.DSNSecret = SecretRef{}
	if err := missingDSN.Validate(); err == nil {
		t.Fatal("dsnSecret 是必填")
	}
}

func TestRestoreModeValid(t *testing.T) {
	for _, mode := range []RestoreMode{RestoreIsolated, RestoreInPlace} {
		if !mode.Valid() {
			t.Fatalf("%q 应当是合法取值", mode)
		}
	}
	for _, mode := range []RestoreMode{"", "inplace", "in-place", "isolate"} {
		if mode.Valid() {
			t.Fatalf("%q 不该是合法取值", mode)
		}
	}
}

func TestBackupRetentionEmpty(t *testing.T) {
	if !(BackupRetention{}).Empty() {
		t.Fatal("零值 retention 应当报告为空")
	}
	cases := []BackupRetention{
		{KeepLast: 1},
		{KeepDays: 1},
		{GFS: BackupGFS{Daily: 1}},
		{GFS: BackupGFS{Weekly: 1}},
		{GFS: BackupGFS{Monthly: 1}},
	}
	for _, r := range cases {
		if r.Empty() {
			t.Fatalf("%+v 不该报告为空", r)
		}
	}
}

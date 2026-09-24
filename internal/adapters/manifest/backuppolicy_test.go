package manifest

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

const filesPolicyYAML = `apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: orders-nightly
resource:
  kind: files
  paths:
    - /var/lib/orders/data
  exclude:
    - "*.tmp"
  symlinks: skip
encoding:
  compression: gzip
  encryption:
    enabled: true
    keySecret:
      kind: file
      name: /etc/opsd/secrets/backup-key
retention:
  keepLast: 14
  keepDays: 30
  gfs:
    daily: 7
    weekly: 4
    monthly: 6
timeout:
  backupSeconds: 3600
  restoreSeconds: 7200
`

func TestParseBackupPolicy(t *testing.T) {
	policy, err := ParseBackupPolicy([]byte(filesPolicyYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if policy.Name != "orders-nightly" || policy.Kind != domain.BackupPolicyKind {
		t.Fatalf("信封字段不对: %+v", policy)
	}
	if policy.Resource.Kind != domain.BackupResourceFiles {
		t.Fatalf("resource.kind want files, got %q", policy.Resource.Kind)
	}
	if len(policy.Resource.Paths) != 1 || policy.Resource.Paths[0] != "/var/lib/orders/data" {
		t.Fatalf("paths 不对: %+v", policy.Resource.Paths)
	}
	if policy.Resource.Symlinks != domain.SymlinkSkip {
		t.Fatalf("symlinks want skip, got %q", policy.Resource.Symlinks)
	}
	if !policy.Encoding.Encryption.Enabled {
		t.Fatal("encryption.enabled 应当为 true")
	}
	if policy.Encoding.Encryption.KeySecret.Kind != domain.SecretKindFile ||
		policy.Encoding.Encryption.KeySecret.Name != "/etc/opsd/secrets/backup-key" {
		t.Fatalf("keySecret 不对: %+v", policy.Encoding.Encryption.KeySecret)
	}
	if policy.Retention.GFS.Monthly != 6 {
		t.Fatalf("gfs.monthly want 6, got %d", policy.Retention.GFS.Monthly)
	}
	// 秒数被转成 Duration。
	if policy.Timeout.Backup != time.Hour || policy.Timeout.Restore != 2*time.Hour {
		t.Fatalf("timeout 不对: %+v", policy.Timeout)
	}
}

// 加密默认开启（规格决定 3）：没写 encoding 就是「要加密」，因此缺密钥的 manifest
// 必须被拒——未配置密钥的部署要显式把加密关掉才能用。
func TestParseBackupPolicyEncryptionDefaultsOn(t *testing.T) {
	noEncoding := `apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: minimal
resource:
  kind: files
  paths:
    - /srv/data
retention:
  keepLast: 1
`
	if _, err := ParseBackupPolicy([]byte(noEncoding)); err == nil {
		t.Fatal("未声明 encryption 即视为开启，缺 keySecret 必须被拒")
	}
}

// 显式关掉加密之后，省略的可选字段才落成默认值。
func TestParseBackupPolicyDefaults(t *testing.T) {
	disabled := `apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: minimal
resource:
  kind: files
  paths:
    - /srv/data
encoding:
  encryption:
    enabled: false
retention:
  keepLast: 1
`
	policy, err := ParseBackupPolicy([]byte(disabled))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if policy.Encoding.Encryption.Enabled {
		t.Fatal("显式写 enabled: false 之后不该还是开启")
	}
	if policy.Encoding.Compression != domain.DefaultCompression {
		t.Fatalf("compression want %q, got %q", domain.DefaultCompression, policy.Encoding.Compression)
	}
	if policy.Resource.Symlinks != domain.SymlinkSkip {
		t.Fatalf("symlinks want %q, got %q", domain.SymlinkSkip, policy.Resource.Symlinks)
	}
	if policy.Timeout.Backup != domain.DefaultBackupTimeout ||
		policy.Timeout.Restore != domain.DefaultRestoreTimeout {
		t.Fatalf("timeout 应当取默认值, got %+v", policy.Timeout)
	}
}

func TestParseBackupPolicyRejections(t *testing.T) {
	cases := map[string]string{
		"空内容":          "",
		"缺 apiVersion": "kind: BackupPolicy\nname: x\n",
		"kind 不对": `apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
name: x
resource: {kind: files, paths: [/srv]}
encoding: {encryption: {enabled: false}}
retention: {keepLast: 1}
`,
		"未知字段": `apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: x
resource: {kind: files, paths: [/srv]}
encoding: {encryption: {enabled: false}}
retention: {keepLast: 1}
bogus: 1
`,
		"未提升 apiVersion 的旧版本": `apiVersion: ops.frz.io/v1alpha0
kind: BackupPolicy
name: x
resource: {kind: files, paths: [/srv]}
encoding: {encryption: {enabled: false}}
retention: {keepLast: 1}
`,
		"缺 retention": `apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: x
resource: {kind: files, paths: [/srv]}
`,
		"paths 含 ..": `apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: x
resource: {kind: files, paths: ["/srv/../etc"]}
encoding: {encryption: {enabled: false}}
encoding: {encryption: {enabled: false}}
retention: {keepLast: 1}
`,
		"加密开启但缺密钥": `apiVersion: ops.frz.io/v1alpha1
kind: BackupPolicy
name: x
resource: {kind: files, paths: [/srv]}
encoding: {encryption: {enabled: true}}
retention: {keepLast: 1}
`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseBackupPolicy([]byte(input))
			if err == nil {
				t.Fatal("非法 manifest 必须被拒绝")
			}
			if got := domain.CodeOf(err); got != v1.CodeManifestInvalid {
				t.Fatalf("错误码 want %s, got %s", v1.CodeManifestInvalid, got)
			}
		})
	}
}

// 两种 manifest 的 kind 必须各自认准，不能互相串门。
func TestManifestKindsAreDistinct(t *testing.T) {
	if _, err := Parse([]byte(filesPolicyYAML)); err == nil {
		t.Fatal("ApplicationSpec 解析器不该接受 BackupPolicy")
	}
	if _, err := ParseBackupPolicy([]byte(`apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: x
runtime: go
artifact: {id: art_1}
exec: {argv: [/usr/bin/true], workingDirectory: /srv, runUser: x}
health: {readiness: {type: tcp, target: "127.0.0.1:1"}}
logs: {directory: /var/log/x}
`)); err == nil {
		t.Fatal("BackupPolicy 解析器不该接受 ApplicationSpec")
	}
}

// 严格解码：未知字段必须报错，而不是被静默丢掉。
func TestParseBackupPolicyRejectsUnknownFieldPath(t *testing.T) {
	input := strings.Replace(filesPolicyYAML, "  keepLast: 14", "  keepLast: 14\n  keepForever: true", 1)
	_, err := ParseBackupPolicy([]byte(input))
	if err == nil {
		t.Fatal("retention 下的未知字段必须被拒绝")
	}
}

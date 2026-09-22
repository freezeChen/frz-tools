package config

import (
	"testing"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/domain"
)

const baseConfig = `apiVersion: ops.frz.io/v1alpha1
kind: OpsdConfig
socket:
  path: /run/opsd/opsd.sock
database:
  path: /var/lib/opsd/opsd.db
runtime:
  workDirectory: /var/lib/opsd/work
  logDirectory: /var/log/opsd
execution:
  allowedPaths:
    - /usr/bin
`

const artifactConfig = baseConfig + `artifactStore:
  root: /var/lib/opsd/artifacts
  maxUploadBytes: 1024
  quotaBytes: 4096
secrets:
  allowedFileDirectories:
    - /etc/opsd/secrets
`

func TestArtifactStoreIsOptional(t *testing.T) {
	cfg, err := LoadBytes([]byte(baseConfig))
	if err != nil {
		t.Fatalf("未配置制品存储时应当仍然有效: %v", err)
	}
	if cfg.ArtifactStoreEnabled() {
		t.Fatal("未配置 artifactStore.root 时应视为未启用")
	}
}

func TestArtifactStoreDefaults(t *testing.T) {
	cfg, err := LoadBytes([]byte(artifactConfig))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.ArtifactStoreEnabled() {
		t.Fatal("应当启用制品存储")
	}
	if cfg.ArtifactStore.MaxUploadBytes != 1024 || cfg.ArtifactStore.QuotaBytes != 4096 {
		t.Fatalf("显式配置未被保留：%+v", cfg.ArtifactStore)
	}
	mode, err := cfg.ArtifactFileMode()
	if err != nil {
		t.Fatalf("file mode: %v", err)
	}
	if mode != 0o640 {
		t.Fatalf("fileMode 默认值：期望 0640，实际 %04o", mode)
	}
	dirMode, err := cfg.ArtifactDirMode()
	if err != nil {
		t.Fatalf("dir mode: %v", err)
	}
	if dirMode != 0o750 {
		t.Fatalf("dirMode 默认值：期望 0750，实际 %04o", dirMode)
	}
}

func TestArtifactStoreValidation(t *testing.T) {
	cases := map[string]string{
		"root 非绝对路径": baseConfig + "artifactStore:\n  root: relative/artifacts\n",
		"上传上限超过配额":   baseConfig + "artifactStore:\n  root: /var/lib/opsd/artifacts\n  maxUploadBytes: 8192\n  quotaBytes: 4096\n",
		"文件模式非法":     baseConfig + "artifactStore:\n  root: /var/lib/opsd/artifacts\n  fileMode: nope\n",
		"允许目录非绝对路径":  baseConfig + "secrets:\n  allowedFileDirectories:\n    - secrets\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadBytes([]byte(body)); domain.CodeOf(err) != v1.CodeConfigInvalid {
				t.Fatalf("want CONFIG_INVALID, got %v", err)
			}
		})
	}
}

func TestUnknownFieldIsStillRejectedAfterMigration(t *testing.T) {
	body := baseConfig + "unexpectedField: true\n"
	if _, err := LoadBytes([]byte(body)); domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("严格解码必须在迁移之后仍然生效，got %v", err)
	}
}

func TestUnsupportedAPIVersionIsRejected(t *testing.T) {
	older := `apiVersion: ops.frz.io/v1alpha0
kind: OpsdConfig
socket:
  path: /run/opsd/opsd.sock
database:
  path: /var/lib/opsd/opsd.db
runtime:
  workDirectory: /var/lib/opsd/work
  logDirectory: /var/log/opsd
execution:
  allowedPaths:
    - /usr/bin
`
	if _, err := LoadBytes([]byte(older)); domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("未登记的 apiVersion 必须明确失败，got %v", err)
	}
}

// 迁移链机制本身必须可验证，否则等到真正需要迁移时才发现它是坏的。
func TestMigrationChainIsApplied(t *testing.T) {
	const legacyVersion = "ops.frz.io/v1alpha0"

	original, hadOriginal := migrations[legacyVersion]
	migrations[legacyVersion] = func(doc map[string]any) error {
		doc["apiVersion"] = APIVersion
		// 老版本用 socketPath，新版本改成 socket.path。
		if legacy, ok := doc["socketPath"].(string); ok {
			delete(doc, "socketPath")
			doc["socket"] = map[string]any{"path": legacy}
		}
		return nil
	}
	t.Cleanup(func() {
		if hadOriginal {
			migrations[legacyVersion] = original
			return
		}
		delete(migrations, legacyVersion)
	})

	legacy := `apiVersion: ops.frz.io/v1alpha0
kind: OpsdConfig
socketPath: /run/opsd/opsd.sock
database:
  path: /var/lib/opsd/opsd.db
runtime:
  workDirectory: /var/lib/opsd/work
  logDirectory: /var/log/opsd
execution:
  allowedPaths:
    - /usr/bin
`
	cfg, err := LoadBytes([]byte(legacy))
	if err != nil {
		t.Fatalf("迁移后应当有效: %v", err)
	}
	if cfg.Socket.Path != "/run/opsd/opsd.sock" {
		t.Fatalf("迁移未生效，socket.path = %q", cfg.Socket.Path)
	}
	if cfg.APIVersion != APIVersion {
		t.Fatalf("迁移后 apiVersion = %q", cfg.APIVersion)
	}
}

func TestMigrationThatDoesNotAdvanceVersionIsRejected(t *testing.T) {
	const legacyVersion = "ops.frz.io/v1alpha0"

	original, hadOriginal := migrations[legacyVersion]
	migrations[legacyVersion] = func(map[string]any) error { return nil }
	t.Cleanup(func() {
		if hadOriginal {
			migrations[legacyVersion] = original
			return
		}
		delete(migrations, legacyVersion)
	})

	body := `apiVersion: ops.frz.io/v1alpha0
kind: OpsdConfig
socket:
  path: /run/opsd/opsd.sock
database:
  path: /var/lib/opsd/opsd.db
runtime:
  workDirectory: /var/lib/opsd/work
  logDirectory: /var/log/opsd
execution:
  allowedPaths:
    - /usr/bin
`
	if _, err := LoadBytes([]byte(body)); domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("不推进版本的迁移必须被识别为配置错误，got %v", err)
	}
}

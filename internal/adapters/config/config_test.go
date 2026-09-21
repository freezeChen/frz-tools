package config

import (
	"testing"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/domain"
)

const validYAML = `
apiVersion: ops.frz.io/v1alpha1
kind: OpsdConfig
socket:
  path: /run/opsd/opsd.sock
  mode: "0660"
database:
  path: /var/lib/opsd/opsd.db
runtime:
  workDirectory: /var/lib/opsd/work
  logDirectory: /var/log/opsd
  workers: 3
execution:
  defaultTimeoutSeconds: 60
  maxOutputBytes: 4096
  allowedPaths:
    - /usr/bin
  sensitiveEnvKeys:
    - TOKEN
sudo:
  allowedCommands: []
`

func TestLoadValidConfig(t *testing.T) {
	cfg, err := LoadBytes([]byte(validYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Runtime.Workers != 3 {
		t.Fatalf("want 3 workers, got %d", cfg.Runtime.Workers)
	}
	if cfg.Socket.Mode != "0660" {
		t.Fatalf("unexpected socket mode %q", cfg.Socket.Mode)
	}
	mode, err := cfg.SocketFileMode()
	if err != nil {
		t.Fatalf("socket file mode: %v", err)
	}
	if mode != 0o660 {
		t.Fatalf("want 0660, got %o", mode)
	}
	if cfg.DefaultTimeout().Seconds() != 60 {
		t.Fatalf("unexpected default timeout: %s", cfg.DefaultTimeout())
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	body := validYAML + "\nunexpectedField: true\n"
	if _, err := LoadBytes([]byte(body)); domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("want CONFIG_INVALID for unknown fields, got %v", err)
	}
}

func TestMissingRequiredFieldsAreRejected(t *testing.T) {
	cases := map[string]string{
		"missing socket path":    "apiVersion: ops.frz.io/v1alpha1\nkind: OpsdConfig\ndatabase:\n  path: /var/lib/opsd/opsd.db\nruntime:\n  workDirectory: /w\n  logDirectory: /l\nexecution:\n  allowedPaths:\n    - /usr/bin\n",
		"relative database path": "apiVersion: ops.frz.io/v1alpha1\nkind: OpsdConfig\nsocket:\n  path: /run/opsd/opsd.sock\ndatabase:\n  path: relative.db\nruntime:\n  workDirectory: /w\n  logDirectory: /l\nexecution:\n  allowedPaths:\n    - /usr/bin\n",
		"empty allowed paths":    "apiVersion: ops.frz.io/v1alpha1\nkind: OpsdConfig\nsocket:\n  path: /run/opsd/opsd.sock\ndatabase:\n  path: /var/lib/opsd.db\nruntime:\n  workDirectory: /w\n  logDirectory: /l\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadBytes([]byte(body)); domain.CodeOf(err) != v1.CodeConfigInvalid {
				t.Fatalf("want CONFIG_INVALID, got %v", err)
			}
		})
	}
}

func TestWrongAPIVersionAndKind(t *testing.T) {
	for name, body := range map[string]string{
		"apiVersion": "apiVersion: ops.frz.io/v2\nkind: OpsdConfig\nsocket:\n  path: /run/opsd.sock\ndatabase:\n  path: /var/lib/opsd.db\nruntime:\n  workDirectory: /w\n  logDirectory: /l\nexecution:\n  allowedPaths:\n    - /usr/bin\n",
		"kind":       "apiVersion: ops.frz.io/v1alpha1\nkind: Other\nsocket:\n  path: /run/opsd.sock\ndatabase:\n  path: /var/lib/opsd.db\nruntime:\n  workDirectory: /w\n  logDirectory: /l\nexecution:\n  allowedPaths:\n    - /usr/bin\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadBytes([]byte(body)); domain.CodeOf(err) != v1.CodeConfigInvalid {
				t.Fatalf("want CONFIG_INVALID, got %v", err)
			}
		})
	}
}

func TestInvalidSocketMode(t *testing.T) {
	body := `apiVersion: ops.frz.io/v1alpha1
kind: OpsdConfig
socket:
  path: /run/opsd.sock
  mode: "not-octal"
database:
  path: /var/lib/opsd.db
runtime:
  workDirectory: /w
  logDirectory: /l
execution:
  allowedPaths:
    - /usr/bin
`
	if _, err := LoadBytes([]byte(body)); domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("want CONFIG_INVALID, got %v", err)
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	body := `apiVersion: ops.frz.io/v1alpha1
kind: OpsdConfig
socket:
  path: /run/opsd.sock
database:
  path: /var/lib/opsd.db
runtime:
  workDirectory: /w
  logDirectory: /l
execution:
  allowedPaths:
    - /usr/bin
`
	cfg, err := LoadBytes([]byte(body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Runtime.Workers != DefaultWorkers {
		t.Fatalf("want default workers %d, got %d", DefaultWorkers, cfg.Runtime.Workers)
	}
	if cfg.Execution.MaxOutputBytes != DefaultMaxOutputBytes {
		t.Fatalf("want default max output %d, got %d", DefaultMaxOutputBytes, cfg.Execution.MaxOutputBytes)
	}
	if cfg.Socket.Mode != DefaultSocketMode {
		t.Fatalf("want default socket mode %q, got %q", DefaultSocketMode, cfg.Socket.Mode)
	}
}

func TestExecutableAllowed(t *testing.T) {
	cfg, err := LoadBytes([]byte(validYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.ExecutableAllowed("/usr/bin/true") {
		t.Fatal("/usr/bin/true must be allowed via the /usr/bin directory entry")
	}
	if cfg.ExecutableAllowed("/tmp/evil") {
		t.Fatal("/tmp/evil must not be allowed")
	}
}

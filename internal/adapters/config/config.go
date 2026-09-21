package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/domain"

	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "ops.frz.io/v1alpha1"
	Kind       = "OpsdConfig"
)

const (
	DefaultWorkers          = 2
	DefaultTimeoutSeconds   = 300
	DefaultMaxOutputBytes   = 1 << 20
	DefaultSocketMode       = "0660"
	DefaultShutdownGraceSec = 10
)

type Config struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Socket     SocketConfig    `yaml:"socket"`
	Database   DatabaseConfig  `yaml:"database"`
	Runtime    RuntimeConfig   `yaml:"runtime"`
	Execution  ExecutionConfig `yaml:"execution"`
	Sudo       SudoConfig      `yaml:"sudo"`
}

type SocketConfig struct {
	Path string `yaml:"path"`
	Mode string `yaml:"mode"`
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

type RuntimeConfig struct {
	WorkDirectory     string `yaml:"workDirectory"`
	LogDirectory      string `yaml:"logDirectory"`
	Workers           int    `yaml:"workers"`
	ShutdownGraceSecs int    `yaml:"shutdownGraceSeconds"`
}

type ExecutionConfig struct {
	DefaultTimeoutSeconds int      `yaml:"defaultTimeoutSeconds"`
	MaxOutputBytes        int64    `yaml:"maxOutputBytes"`
	AllowedPaths          []string `yaml:"allowedPaths"`
	SensitiveEnvKeys      []string `yaml:"sensitiveEnvKeys"`
}

type SudoConfig struct {
	AllowedCommands []string `yaml:"allowedCommands"`
}

func Load(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, domain.NewError(v1.CodeConfigInvalid, "cannot read config %q: %v", path, err)
	}
	defer file.Close()
	return LoadReader(file)
}

func LoadBytes(data []byte) (*Config, error) {
	return LoadReader(bytes.NewReader(data))
}

func LoadReader(r io.Reader) (*Config, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return nil, domain.NewError(v1.CodeConfigInvalid, "invalid config: %v", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Socket.Mode == "" {
		c.Socket.Mode = DefaultSocketMode
	}
	if c.Runtime.Workers == 0 {
		c.Runtime.Workers = DefaultWorkers
	}
	if c.Runtime.ShutdownGraceSecs == 0 {
		c.Runtime.ShutdownGraceSecs = DefaultShutdownGraceSec
	}
	if c.Execution.DefaultTimeoutSeconds == 0 {
		c.Execution.DefaultTimeoutSeconds = DefaultTimeoutSeconds
	}
	if c.Execution.MaxOutputBytes == 0 {
		c.Execution.MaxOutputBytes = DefaultMaxOutputBytes
	}
}

func (c *Config) Validate() error {
	var problems []string

	if c.APIVersion != APIVersion {
		problems = append(problems, fmt.Sprintf("apiVersion must be %q, got %q", APIVersion, c.APIVersion))
	}
	if c.Kind != Kind {
		problems = append(problems, fmt.Sprintf("kind must be %q, got %q", Kind, c.Kind))
	}
	if err := requireAbsolute("socket.path", c.Socket.Path); err != nil {
		problems = append(problems, err.Error())
	}
	if err := requireAbsolute("database.path", c.Database.Path); err != nil {
		problems = append(problems, err.Error())
	}
	if err := requireAbsolute("runtime.workDirectory", c.Runtime.WorkDirectory); err != nil {
		problems = append(problems, err.Error())
	}
	if err := requireAbsolute("runtime.logDirectory", c.Runtime.LogDirectory); err != nil {
		problems = append(problems, err.Error())
	}
	if c.Runtime.Workers < 1 {
		problems = append(problems, "runtime.workers must be at least 1")
	}
	if c.Execution.DefaultTimeoutSeconds < 1 {
		problems = append(problems, "execution.defaultTimeoutSeconds must be at least 1")
	}
	if c.Execution.MaxOutputBytes < 1 {
		problems = append(problems, "execution.maxOutputBytes must be at least 1")
	}
	if len(c.Execution.AllowedPaths) == 0 {
		problems = append(problems, "execution.allowedPaths must list at least one directory or executable")
	}
	for i, p := range c.Execution.AllowedPaths {
		if err := requireAbsolute(fmt.Sprintf("execution.allowedPaths[%d]", i), p); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if _, err := c.SocketFileMode(); err != nil {
		problems = append(problems, err.Error())
	}

	if len(problems) > 0 {
		err := domain.NewError(v1.CodeConfigInvalid, "config is invalid")
		err.Details = map[string]any{"problems": problems}
		return err
	}
	return nil
}

func (c *Config) SocketFileMode() (os.FileMode, error) {
	mode, err := strconv.ParseUint(c.Socket.Mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("socket.mode %q is not an octal file mode", c.Socket.Mode)
	}
	if mode > 0o777 {
		return 0, fmt.Errorf("socket.mode %q must not exceed 0777", c.Socket.Mode)
	}
	return os.FileMode(mode), nil
}

func (c *Config) DefaultTimeout() time.Duration {
	return time.Duration(c.Execution.DefaultTimeoutSeconds) * time.Second
}

func (c *Config) ShutdownGrace() time.Duration {
	return time.Duration(c.Runtime.ShutdownGraceSecs) * time.Second
}

// ExecutableAllowed 判断 argv[0] 是否落在配置的 allowedPaths 内，既支持精确
// 匹配某个可执行文件，也支持匹配目录下的成员。
func (c *Config) ExecutableAllowed(path string) bool {
	cleanPath := filepath.Clean(path)
	for _, allowed := range c.Execution.AllowedPaths {
		cleanAllowed := filepath.Clean(allowed)
		if cleanPath == cleanAllowed {
			return true
		}
		if strings.HasPrefix(cleanPath, cleanAllowed+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func (c *Config) SensitiveEnvKeys() []string {
	return append([]string(nil), c.Execution.SensitiveEnvKeys...)
}

func requireAbsolute(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if !filepath.IsAbs(value) {
		return fmt.Errorf("%s must be an absolute path, got %q", field, value)
	}
	return nil
}

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

	DefaultArtifactFileMode  = "0640"
	DefaultArtifactDirMode   = "0750"
	DefaultMaxUploadBytes    = int64(2) << 30
	DefaultArtifactQuotaByte = int64(20) << 30
)

// maxMigrationSteps 防止迁移链配置错误导致死循环。
const maxMigrationSteps = 16

type Config struct {
	APIVersion    string              `yaml:"apiVersion"`
	Kind          string              `yaml:"kind"`
	Socket        SocketConfig        `yaml:"socket"`
	Database      DatabaseConfig      `yaml:"database"`
	Runtime       RuntimeConfig       `yaml:"runtime"`
	Execution     ExecutionConfig     `yaml:"execution"`
	ArtifactStore ArtifactStoreConfig `yaml:"artifactStore"`
	Secrets       SecretsConfig       `yaml:"secrets"`
	Sudo          SudoConfig          `yaml:"sudo"`
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

// ArtifactStoreConfig 为空（root 未设置）时表示不启用制品存储：相关端点返回
// CONFIG_INVALID 并说明缺失项，而不是让守护进程启动失败。这样迭代 0 的既有部署
// 升级后无需立即配置制品。
type ArtifactStoreConfig struct {
	Root           string `yaml:"root"`
	FileMode       string `yaml:"fileMode"`
	DirMode        string `yaml:"dirMode"`
	MaxUploadBytes int64  `yaml:"maxUploadBytes"`
	QuotaBytes     int64  `yaml:"quotaBytes"`
}

type SecretsConfig struct {
	AllowedFileDirectories []string `yaml:"allowedFileDirectories"`
}

type SudoConfig struct {
	AllowedCommands []string `yaml:"allowedCommands"`
}

// envelope 只用于在严格解码之前读出版本，以便决定是否需要迁移。
type envelope struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

// migrations 登记「从某个 apiVersion 升级到下一个版本」的函数。
//
// 兼容规则：同一 apiVersion 内新增可选字段是向后兼容的，无需登记；删除字段、改名、
// 改变语义或改变必填性，必须提升 apiVersion 并在此登记一段迁移。
var migrations = map[string]func(doc map[string]any) error{}

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
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, domain.NewError(v1.CodeConfigInvalid, "cannot read config: %v", err)
	}

	var env envelope
	if err := yaml.Unmarshal(raw, &env); err != nil {
		return nil, domain.NewError(v1.CodeConfigInvalid, "invalid config: %v", err)
	}
	if env.APIVersion == "" {
		return nil, domain.NewError(v1.CodeConfigInvalid, "config must declare apiVersion")
	}

	doc := map[string]any{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, domain.NewError(v1.CodeConfigInvalid, "invalid config: %v", err)
	}
	if err := migrate(doc, env.APIVersion); err != nil {
		return nil, err
	}

	normalized, err := yaml.Marshal(doc)
	if err != nil {
		return nil, domain.NewError(v1.CodeConfigInvalid, "cannot normalize config: %v", err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(normalized))
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

func migrate(doc map[string]any, from string) error {
	version := from
	for step := 0; version != APIVersion; step++ {
		if step >= maxMigrationSteps {
			return domain.NewError(v1.CodeConfigInvalid,
				"config migration starting at %q did not converge", from)
		}
		apply, ok := migrations[version]
		if !ok {
			return domain.NewError(v1.CodeConfigInvalid,
				"unsupported config apiVersion %q; this build understands %q", from, APIVersion)
		}
		if err := apply(doc); err != nil {
			return domain.NewError(v1.CodeConfigInvalid, "cannot migrate config from %q: %v", version, err)
		}
		next, _ := doc["apiVersion"].(string)
		if next == version {
			return domain.NewError(v1.CodeConfigInvalid,
				"migration from %q did not advance apiVersion", version)
		}
		version = next
	}
	return nil
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

	if c.ArtifactStore.Root != "" {
		if c.ArtifactStore.FileMode == "" {
			c.ArtifactStore.FileMode = DefaultArtifactFileMode
		}
		if c.ArtifactStore.DirMode == "" {
			c.ArtifactStore.DirMode = DefaultArtifactDirMode
		}
		if c.ArtifactStore.MaxUploadBytes == 0 {
			c.ArtifactStore.MaxUploadBytes = DefaultMaxUploadBytes
		}
		if c.ArtifactStore.QuotaBytes == 0 {
			c.ArtifactStore.QuotaBytes = DefaultArtifactQuotaByte
		}
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

	problems = append(problems, c.validateArtifactStore()...)
	for i, dir := range c.Secrets.AllowedFileDirectories {
		err := requireAbsolute(fmt.Sprintf("secrets.allowedFileDirectories[%d]", i), dir)
		if err != nil {
			problems = append(problems, err.Error())
		}
	}

	if len(problems) > 0 {
		err := domain.NewError(v1.CodeConfigInvalid, "config is invalid")
		err.Details = map[string]any{"problems": problems}
		return err
	}
	return nil
}

func (c *Config) validateArtifactStore() []string {
	if !c.ArtifactStoreEnabled() {
		return nil
	}
	var problems []string

	if err := requireAbsolute("artifactStore.root", c.ArtifactStore.Root); err != nil {
		problems = append(problems, err.Error())
	}
	if _, err := parseOctalMode("artifactStore.fileMode", c.ArtifactStore.FileMode); err != nil {
		problems = append(problems, err.Error())
	}
	if _, err := parseOctalMode("artifactStore.dirMode", c.ArtifactStore.DirMode); err != nil {
		problems = append(problems, err.Error())
	}
	if c.ArtifactStore.MaxUploadBytes < 1 {
		problems = append(problems, "artifactStore.maxUploadBytes must be at least 1")
	}
	if c.ArtifactStore.QuotaBytes < 1 {
		problems = append(problems, "artifactStore.quotaBytes must be at least 1")
	}
	if c.ArtifactStore.QuotaBytes > 0 && c.ArtifactStore.MaxUploadBytes > c.ArtifactStore.QuotaBytes {
		problems = append(problems, "artifactStore.maxUploadBytes must not exceed artifactStore.quotaBytes")
	}
	return problems
}

func (c *Config) ArtifactStoreEnabled() bool { return c.ArtifactStore.Root != "" }

func (c *Config) ArtifactFileMode() (os.FileMode, error) {
	return parseOctalMode("artifactStore.fileMode", c.ArtifactStore.FileMode)
}

func (c *Config) ArtifactDirMode() (os.FileMode, error) {
	return parseOctalMode("artifactStore.dirMode", c.ArtifactStore.DirMode)
}

func (c *Config) SocketFileMode() (os.FileMode, error) {
	return parseOctalMode("socket.mode", c.Socket.Mode)
}

func parseOctalMode(field, value string) (os.FileMode, error) {
	mode, err := strconv.ParseUint(value, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not an octal file mode", field, value)
	}
	if mode > 0o777 {
		return 0, fmt.Errorf("%s %q must not exceed 0777", field, value)
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

func (c *Config) AllowedSecretDirectories() []string {
	return append([]string(nil), c.Secrets.AllowedFileDirectories...)
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

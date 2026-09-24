package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	stdruntime "runtime"
	"syscall"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/backup/files"
	"github.com/freezeChen/frz-tools/internal/adapters/backup/mysql"
	"github.com/freezeChen/frz-tools/internal/adapters/backup/postgres"
	"github.com/freezeChen/frz-tools/internal/adapters/blob"
	"github.com/freezeChen/frz-tools/internal/adapters/config"
	"github.com/freezeChen/frz-tools/internal/adapters/executor"
	"github.com/freezeChen/frz-tools/internal/adapters/httpapi"
	"github.com/freezeChen/frz-tools/internal/adapters/runtime/systemd"
	"github.com/freezeChen/frz-tools/internal/adapters/secret"
	"github.com/freezeChen/frz-tools/internal/adapters/sqlite"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/cliutil"
	"github.com/freezeChen/frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:           "opsd",
		Short:         "opsd 是目标主机上的守护进程，负责执行需要权限的操作",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          run,
	}
	root.Flags().String("config", "/etc/opsd/config.yaml", "opsd 配置文件路径")
	root.Flags().String("log-level", "info", "日志级别：debug、info、warn、error")
	cliutil.LocalizeUsage(root)
	cliutil.LocalizeHelpFlag(root)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "opsd:", domain.MessageOf(err))
		os.Exit(v1.ExitCode(domain.CodeOf(err)))
	}
}

func run(cmd *cobra.Command, _ []string) error {
	configPath, _ := cmd.Flags().GetString("config")
	logLevel, _ := cmd.Flags().GetString("log-level")

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	if err := prepareDirectories(cfg); err != nil {
		return err
	}

	logger, closeLog, err := newLogger(cfg, logLevel)
	if err != nil {
		return err
	}
	defer closeLog()

	store, err := sqlite.Open(cfg.Database.Path)
	if err != nil {
		return domain.NewError(v1.CodeConfigInvalid, "无法打开数据库: %v", err)
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := store.Migrate(ctx); err != nil {
		return domain.NewError(v1.CodeInternal, "执行数据库迁移失败: %v", err)
	}

	recovered, err := application.Recover(ctx, store, logger, time.Now().UTC(), application.RecoveryOptions{})
	if err != nil {
		return domain.NewError(v1.CodeInternal, "启动时的恢复流程失败: %v", err)
	}
	logger.Info("opsd starting",
		"apiVersion", v1.APIVersion,
		"socket", cfg.Socket.Path,
		"database", cfg.Database.Path,
		"workers", cfg.Runtime.Workers,
		"recoveredOperations", recovered,
	)

	exec := executor.New(cfg.ExecutableAllowed, cfg.DefaultTimeout(), cfg.Execution.MaxOutputBytes, cfg.SensitiveEnvKeys())

	artifactStore, artifactPolicy, err := openArtifactStore(ctx, cfg, logger)
	if err != nil {
		return err
	}

	// 备份用的是**独立的**存储根：1a 的制品 GC 把「digest 不在 artifacts 表里」一律
	// 当作孤儿删除，共用根会让 artifact gc 删掉全部备份（迭代 2 规格 D5）。
	backupStore, err := openBackupStore(ctx, cfg, logger)
	if err != nil {
		return err
	}

	// 运行时适配器的平台选择只有这一处：Linux 上真的装配 systemd 适配器，其它平台
	// 刻意不注入——runtime.* 于是返回 RUNTIME_UNSUPPORTED，而不是让一个假适配器在生产
	// 里假装能用（也不引入让生产误选假适配器的配置开关）。secretResolver 两个用途
	// 共用同一份，避免「执行器看到的允许目录」与「适配器看到的」不一致。
	secretResolver := secret.NewResolver(cfg.AllowedSecretDirectories())
	var (
		runtimeAdapter  application.RuntimeAdapter
		prepareReporter application.RuntimePrepareReporter
	)
	if stdruntime.GOOS == "linux" {
		systemdAdapter := systemd.New("/", secretResolver, systemd.WithLogger(logger))
		runtimeAdapter = systemdAdapter
		prepareReporter = systemdReporter{adapter: systemdAdapter}
	} else {
		logger.Info("runtime adapter is unavailable on this platform, runtime.* will report RUNTIME_UNSUPPORTED",
			"goos", stdruntime.GOOS)
	}

	runtime := application.NewRuntime(application.Options{
		Repo:        store,
		Executor:    exec,
		Secrets:     secretResolver,
		Store:       artifactStore,
		BackupStore: backupStore,
		BackupAdapters: []application.BackupAdapter{
			files.New(),
			// 数据库适配器共用同一个执行器实例。它在**绝对路径**上做 allowedPaths
			// 校验，而 pg_dump/mysqldump 一般不在 executor 默认放行的目录里——
			// 运维需要在 execution.allowedPaths 里放行客户端工具所在的目录
			// （例如 /usr/lib/postgresql/16/bin）。适配器不绕过这道校验，
			// 它只负责把工具名解析成绝对路径。
			postgres.New(exec, secretResolver),
			mysql.New(exec, secretResolver),
		},
		RuntimeAdapter:  runtimeAdapter,
		PrepareReporter: prepareReporter,
		AllowExecutable: cfg.ExecutableAllowed,
		Defaults: application.Defaults{
			Timeout:          cfg.DefaultTimeout(),
			MaxOutputBytes:   cfg.Execution.MaxOutputBytes,
			SensitiveEnvKeys: cfg.SensitiveEnvKeys(),
		},
		ArtifactPolicy: artifactPolicy,
		Workers:        cfg.Runtime.Workers,
		Logger:         logger,
	})

	// 本机 Host 记录是「应用挂在哪台主机上」的锚点。每次启动都确保它存在；
	// 已存在时原样返回，不覆盖运维调整过的名字与标签。
	localHost, err := runtime.Hosts.EnsureLocalHost(ctx)
	if err != nil {
		return domain.NewError(v1.CodeInternal, "确保本机 Host 记录失败: %v", err)
	}
	logger.Info("local host is ready", "host", localHost.Name, "hostId", localHost.ID)

	mode, err := cfg.SocketFileMode()
	if err != nil {
		return domain.NewError(v1.CodeConfigInvalid, "%v", err)
	}
	listener, err := httpapi.ListenUnix(cfg.Socket.Path, mode)
	if err != nil {
		return domain.NewError(v1.CodeInternal, "无法监听 %s: %v", cfg.Socket.Path, err)
	}
	defer func() {
		listener.Close()
		os.Remove(cfg.Socket.Path)
	}()

	runtime.Pool.Start(ctx)
	// 调度器与 worker 池并行运行：前者只决定何时创建 Operation，后者负责执行。
	go runtime.Scheduler.Run(ctx)
	server := httpapi.NewServer(httpapi.Dependencies{
		Service:   runtime.Service,
		Artifacts: runtime.Artifacts,
		Catalogs:  runtime.Catalogs,
		Specs:     runtime.Specs,
		Hosts:     runtime.Hosts,
		Runtimes:  runtime.Runtimes,
		Schedules: runtime.Schedules,
		Backups:   runtime.Backups,
		Store:     store,
		Workers:   runtime.Pool.Workers(),
		Logger:    logger,
	})

	if err := server.Serve(ctx, listener, cfg.ShutdownGrace()); err != nil {
		return domain.NewError(v1.CodeInternal, "HTTP 服务启动失败: %v", err)
	}
	logger.Info("opsd stopped")
	return nil
}

func openArtifactStore(ctx context.Context, cfg *config.Config, logger *slog.Logger) (application.StorageBackend, application.ArtifactPolicy, error) {
	if !cfg.ArtifactStoreEnabled() {
		logger.Info("artifact store is disabled", "reason", "artifactStore.root is not configured")
		return nil, application.ArtifactPolicy{}, nil
	}

	fileMode, err := cfg.ArtifactFileMode()
	if err != nil {
		return nil, application.ArtifactPolicy{}, domain.NewError(v1.CodeConfigInvalid, "%v", err)
	}
	dirMode, err := cfg.ArtifactDirMode()
	if err != nil {
		return nil, application.ArtifactPolicy{}, domain.NewError(v1.CodeConfigInvalid, "%v", err)
	}

	local, err := blob.NewLocal(cfg.ArtifactStore.Root, fileMode, dirMode)
	if err != nil {
		return nil, application.ArtifactPolicy{}, err
	}

	removed, err := local.CleanupTemp(ctx)
	if err != nil {
		return nil, application.ArtifactPolicy{}, err
	}
	if removed > 0 {
		logger.Warn("removed stale artifact uploads left by a previous run", "count", removed)
	}

	logger.Info("artifact store ready",
		"root", cfg.ArtifactStore.Root,
		"fileMode", fmt.Sprintf("%04o", fileMode),
		"maxUploadBytes", cfg.ArtifactStore.MaxUploadBytes,
		"quotaBytes", cfg.ArtifactStore.QuotaBytes,
	)

	return local, application.ArtifactPolicy{
		MaxUploadBytes: cfg.ArtifactStore.MaxUploadBytes,
		QuotaBytes:     cfg.ArtifactStore.QuotaBytes,
	}, nil
}

// openBackupStore 打开备份存储。未配置 backupStore.root 时返回 nil：
// 备份相关端点会明确拒绝（CONFIG_INVALID），而不是让守护进程启动失败。
func openBackupStore(ctx context.Context, cfg *config.Config, logger *slog.Logger) (application.StorageBackend, error) {
	if !cfg.BackupStoreEnabled() {
		logger.Info("backup store is disabled", "reason", "backupStore.root is not configured")
		return nil, nil
	}

	fileMode, err := cfg.BackupFileMode()
	if err != nil {
		return nil, domain.NewError(v1.CodeConfigInvalid, "%v", err)
	}
	dirMode, err := cfg.BackupDirMode()
	if err != nil {
		return nil, domain.NewError(v1.CodeConfigInvalid, "%v", err)
	}

	local, err := blob.NewLocal(cfg.BackupStore.Root, fileMode, dirMode)
	if err != nil {
		return nil, err
	}

	// 上一次运行留下的半截备份要清掉，否则它们会一直占着盘。
	removed, err := local.CleanupTemp(ctx)
	if err != nil {
		return nil, err
	}
	if removed > 0 {
		logger.Warn("removed stale backup uploads left by a previous run", "count", removed)
	}

	logger.Info("backup store ready",
		"root", cfg.BackupStore.Root,
		"fileMode", fmt.Sprintf("%04o", fileMode),
		"quotaBytes", cfg.BackupStore.QuotaBytes,
	)
	return local, nil
}

func prepareDirectories(cfg *config.Config) error {
	for _, dir := range []string{
		filepath.Dir(cfg.Socket.Path),
		filepath.Dir(cfg.Database.Path),
		cfg.Runtime.WorkDirectory,
		cfg.Runtime.LogDirectory,
		cfg.BackupStore.Root,
	} {
		// 跳过未配置的项：backupStore.root 之类是可选的，空串会让 MkdirAll 直接失败，
		// 从而把一个「功能没启用」变成「守护进程起不来」。
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return domain.NewError(v1.CodeConfigInvalid, "无法创建目录 %q: %v", dir, err)
		}
	}
	return nil
}

func newLogger(cfg *config.Config, level string) (*slog.Logger, func(), error) {
	parsedLevel, err := parseLevel(level)
	if err != nil {
		return nil, nil, err
	}
	writer := io.Writer(os.Stdout)

	closeLog := func() {}
	if cfg.Runtime.LogDirectory != "" {
		logPath := filepath.Join(cfg.Runtime.LogDirectory, "opsd.log")
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, domain.NewError(v1.CodeConfigInvalid, "无法打开日志文件 %q: %v", logPath, err)
		}
		writer = io.MultiWriter(os.Stdout, file)
		closeLog = func() { file.Close() }
	}

	logger := slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: parsedLevel}))
	return logger, closeLog, nil
}

func parseLevel(level string) (slog.Level, error) {
	switch level {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, domain.NewError(v1.CodeConfigInvalid, "未知的日志级别 %q", level)
	}
}

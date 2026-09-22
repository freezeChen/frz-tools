package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/adapters/blob"
	"frz-tools/internal/adapters/config"
	"frz-tools/internal/adapters/executor"
	"frz-tools/internal/adapters/httpapi"
	"frz-tools/internal/adapters/secret"
	"frz-tools/internal/adapters/sqlite"
	"frz-tools/internal/application"
	"frz-tools/internal/cliutil"
	"frz-tools/internal/domain"

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

	recovered, err := application.Recover(ctx, store, logger, time.Now().UTC())
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

	runtime := application.NewRuntime(application.Options{
		Repo:            store,
		Executor:        exec,
		Secrets:         secret.NewResolver(cfg.AllowedSecretDirectories()),
		Store:           artifactStore,
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
	server := httpapi.NewServer(httpapi.Dependencies{
		Service:   runtime.Service,
		Artifacts: runtime.Artifacts,
		Catalogs:  runtime.Catalogs,
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

func prepareDirectories(cfg *config.Config) error {
	for _, dir := range []string{
		filepath.Dir(cfg.Socket.Path),
		filepath.Dir(cfg.Database.Path),
		cfg.Runtime.WorkDirectory,
		cfg.Runtime.LogDirectory,
	} {
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

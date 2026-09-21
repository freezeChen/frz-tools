package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/adapters/config"
	"frz-tools/internal/adapters/executor"
	"frz-tools/internal/adapters/httpapi"
	"frz-tools/internal/adapters/sqlite"
	"frz-tools/internal/application"
	"frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:           "opsd",
		Short:         "opsd runs privileged Linux operations requested through opsctl",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          run,
	}
	root.Flags().String("config", "/etc/opsd/config.yaml", "path to the opsd configuration file")
	root.Flags().String("log-level", "info", "log level: debug, info, warn, error")

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
		return domain.NewError(v1.CodeConfigInvalid, "cannot open database: %v", err)
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := store.Migrate(ctx); err != nil {
		return domain.NewError(v1.CodeInternal, "migration failed: %v", err)
	}

	recovered, err := application.Recover(ctx, store, logger, time.Now().UTC())
	if err != nil {
		return domain.NewError(v1.CodeInternal, "startup recovery failed: %v", err)
	}
	logger.Info("opsd starting",
		"apiVersion", v1.APIVersion,
		"socket", cfg.Socket.Path,
		"database", cfg.Database.Path,
		"workers", cfg.Runtime.Workers,
		"recoveredOperations", recovered,
	)

	exec := executor.New(cfg.ExecutableAllowed, cfg.DefaultTimeout(), cfg.Execution.MaxOutputBytes, cfg.SensitiveEnvKeys())
	runtime := application.NewRuntime(store, exec, cfg.ExecutableAllowed, application.Defaults{
		Timeout:          cfg.DefaultTimeout(),
		MaxOutputBytes:   cfg.Execution.MaxOutputBytes,
		SensitiveEnvKeys: cfg.SensitiveEnvKeys(),
	}, cfg.Runtime.Workers, 0, logger)

	mode, err := cfg.SocketFileMode()
	if err != nil {
		return domain.NewError(v1.CodeConfigInvalid, "%v", err)
	}
	listener, err := httpapi.ListenUnix(cfg.Socket.Path, mode)
	if err != nil {
		return domain.NewError(v1.CodeInternal, "cannot listen on %s: %v", cfg.Socket.Path, err)
	}
	defer func() {
		listener.Close()
		os.Remove(cfg.Socket.Path)
	}()

	runtime.Pool.Start(ctx)
	server := httpapi.NewServer(runtime.Service, store, runtime.Pool.Workers(), logger)

	if err := server.Serve(ctx, listener, cfg.ShutdownGrace()); err != nil {
		return domain.NewError(v1.CodeInternal, "http server failed: %v", err)
	}
	logger.Info("opsd stopped")
	return nil
}

func prepareDirectories(cfg *config.Config) error {
	for _, dir := range []string{
		filepath.Dir(cfg.Socket.Path),
		filepath.Dir(cfg.Database.Path),
		cfg.Runtime.WorkDirectory,
		cfg.Runtime.LogDirectory,
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return domain.NewError(v1.CodeConfigInvalid, "cannot create directory %q: %v", dir, err)
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
			return nil, nil, domain.NewError(v1.CodeConfigInvalid, "cannot open log file %q: %v", logPath, err)
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
		return 0, domain.NewError(v1.CodeConfigInvalid, "unknown log level %q", level)
	}
}

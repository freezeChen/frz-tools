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
	"runtime/debug"
	"strings"
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
	"github.com/freezeChen/frz-tools/internal/adapters/nginx"
	releaselocal "github.com/freezeChen/frz-tools/internal/adapters/release/local"
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
		nginxAdapter    application.NginxAdapter
	)
	if stdruntime.GOOS == "linux" {
		systemdAdapter := systemd.New("/", secretResolver, systemd.WithLogger(logger))
		runtimeAdapter = systemdAdapter
		prepareReporter = systemdReporter{adapter: systemdAdapter}
		// Nginx 适配器与 systemd 适配器同一个平台门槛：它要执行 nginx 二进制、写
		// /etc/nginx 下的受管文件。**缺席不等于部署能力缺失**——单槽应用照常部署，
		// 蓝绿应用会得到一句明确的「本部署未装配 Nginx 适配器」。
		nginxAdapter = nginx.New(nginx.Config{
			ConfDir:    cfg.Nginx.ConfDir,
			Binary:     cfg.Nginx.Binary,
			MainConfig: cfg.Nginx.MainConfig,
		}, nginx.WithLogger(logger))
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
		NginxAdapter:    nginxAdapter,
		// 发布适配器（解包、切换、清理）与运行时适配器**分开装配**：它只做文件系统与归档，
		// 任何平台都能用；而部署能力是否可用由 Configured() 一起判定（缺运行时就没有部署）。
		ReleaseAdapter:  releaselocal.New(),
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

	// 启动时的槽位对账（迭代 4c）：把库里那份 serving_slot 纠正到线上事实（Nginx 的
	// 受管配置），并按 systemd 的实际状态重算槽位状态。
	//
	// 放在这里、worker 池启动之前是刻意的：此刻不可能有部署在跑（部署只在池里执行），
	// 因此对账读到的是一个稳定的状态，不会与某次发布抢。
	//
	// **对账失败不影响 opsd 启动**：它是纠正动作，不是前置条件。一个应用的受管配置坏了，
	// 不该让整个守护进程起不来。
	reconciled, err := runtime.Slots.Reconcile(ctx)
	if err != nil {
		logger.Warn("启动时的槽位对账失败（不影响启动）", "error", err)
	} else if reconciled.Checked > 0 || len(reconciled.Errors) > 0 {
		logger.Info("槽位对账完成", "checked", reconciled.Checked,
			"changes", len(reconciled.Changes), "skipped", len(reconciled.Skipped),
			"errors", len(reconciled.Errors), "detail", reconciled.Summary())
	}

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

	// 远程身份白名单只在真的开了远程端口时装配。**没有配 remote 段就不装配**，
	// 从而让「没开远程却收到 mTLS 请求」变成一个明确的装配错误，而不是悄悄放行。
	var identities httpapi.RemoteIdentityLookup
	if cfg.RemoteEnabled() {
		identities = cfg
	}
	server := httpapi.NewServer(httpapi.Dependencies{
		Service:   runtime.Service,
		Artifacts: runtime.Artifacts,
		Catalogs:  runtime.Catalogs,
		Specs:     runtime.Specs,
		Hosts:     runtime.Hosts,
		Runtimes:  runtime.Runtimes,
		Schedules: runtime.Schedules,
		Backups:   runtime.Backups,
		Deploys:   runtime.Deploys,
		Slots:     runtime.Slots,
		Store:     store,
		Workers:   runtime.Pool.Workers(),
		Logger:    logger,

		RemoteIdentities: identities,
		Version:          buildVersion(),
	})

	// 远程监听（迭代 5a）。**先绑定再启动**：端口被占用是启动期就该报出来的问题，
	// 而一个「起来了但远程连不上」的守护进程最难排查。
	if cfg.RemoteEnabled() {
		remoteListener, err := httpapi.ListenTLS(
			cfg.Remote.Listen, cfg.Remote.CertFile, cfg.Remote.KeyFile, cfg.Remote.ClientCAFile)
		if err != nil {
			return err
		}
		defer remoteListener.Close()
		logger.Info("remote listener ready",
			"listen", cfg.Remote.Listen,
			"clients", len(cfg.Remote.Clients),
			"minTLSVersion", "1.2",
		)
		go func() {
			// 远程监听挂了**不让本机跟着停**：本机 socket 上的运维动作与远程是两件事，
			// 而远程只是这个守护进程的一个可选能力（与 Nginx 适配器可以缺席同一条）。
			// 失败在这里是 Error 级日志，而不是静默降级——静默的那一天没人知道
			// 远程已经连不上了。
			if err := server.Serve(ctx, remoteListener, cfg.ShutdownGrace()); err != nil {
				logger.Error("remote listener stopped serving",
					"listen", cfg.Remote.Listen, "error", err)
			}
		}()
	} else {
		logger.Info("remote listener is disabled", "reason", "remote.listen is not configured")
	}

	if err := server.Serve(ctx, listener, cfg.ShutdownGrace()); err != nil {
		return domain.NewError(v1.CodeInternal, "HTTP 服务启动失败: %v", err)
	}
	logger.Info("opsd stopped")
	return nil
}

// buildVersion 从构建信息里拼出版本号：模块版本 + VCS 修订。
//
// 刻意不加 -ldflags 变量：go build 在 git 仓库里默认会写入 vcs.revision，
// 因此「这个 opsd 是哪个提交编的」不需要额外的构建约定就能答出来。取不到时返回
// 空串——空串是诚实的事实（没有信息），不是 "unknown"（那会被读成「查过了、查不到」）。
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var parts []string
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		parts = append(parts, info.Main.Version)
	}
	for _, setting := range info.Settings {
		if setting.Key != "vcs.revision" || setting.Value == "" {
			continue
		}
		revision := setting.Value
		if len(revision) > 12 {
			revision = revision[:12]
		}
		parts = append(parts, revision)
		break
	}
	return strings.Join(parts, "+")
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

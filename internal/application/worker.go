package application

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/domain"
)

const defaultIdlePoll = 250 * time.Millisecond

type Pool struct {
	repo     Repository
	exec     Executor
	resolver SecretResolver
	defaults Defaults
	cancels  *cancelRegistry
	workers  int
	idle     time.Duration
	logger   *slog.Logger
	wake     chan struct{}
	now      func() time.Time
}

func newPool(repo Repository, exec Executor, resolver SecretResolver, defaults Defaults, cancels *cancelRegistry, workers int, logger *slog.Logger) *Pool {
	if workers < 1 {
		workers = 1
	}
	return &Pool{
		repo:     repo,
		exec:     exec,
		resolver: resolver,
		defaults: defaults,
		cancels:  cancels,
		workers:  workers,
		idle:     defaultIdlePoll,
		logger:   logger,
		wake:     make(chan struct{}, 1),
		now:      func() time.Time { return time.Now().UTC() },
	}
}

func (p *Pool) Workers() int { return p.workers }

func (p *Pool) Notify() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Pool) Start(ctx context.Context) {
	for i := 0; i < p.workers; i++ {
		go p.loop(ctx)
	}
}

func (p *Pool) loop(ctx context.Context) {
	ticker := time.NewTicker(p.idle)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-ticker.C:
		}

		for ctx.Err() == nil {
			worked, err := p.ProcessNext(ctx)
			if err != nil {
				p.logger.Error("worker failed to process operation", "error", err)
				break
			}
			if !worked {
				break
			}
		}
	}
}

// ProcessNext 最多领取并执行一个 pending 操作，并返回是否处理了操作。
func (p *Pool) ProcessNext(ctx context.Context) (bool, error) {
	op, err := p.repo.ClaimNextPending(ctx, p.now())
	if err != nil {
		return false, err
	}
	if op == nil {
		return false, nil
	}
	p.execute(ctx, op)
	return true, nil
}

func (p *Pool) execute(ctx context.Context, op *domain.Operation) {
	// 终态写入不能因为取消或关停而丢失，因此用不受取消影响的 context 落库。
	persistCtx := context.WithoutCancel(ctx)
	logger := p.logger.With("operationId", op.ID, "resource", op.Resource)

	execCtx, cancel := context.WithCancel(ctx)
	p.cancels.register(op.ID, cancel)
	defer func() {
		cancel()
		p.cancels.unregister(op.ID)
	}()

	spec, err := BuildCommandSpec(op.Kind, op.Spec, p.defaults)
	if err != nil {
		code := string(domain.CodeOf(err))
		p.appendLog(persistCtx, op, nil, "error", domain.PhaseValidate, "operation rejected: "+domain.MessageOf(err),
			map[string]string{"errorCode": code})
		p.finish(persistCtx, op, domain.StatusFailed, nil, code, domain.MessageOf(err))
		return
	}
	spec.DryRun = op.DryRun

	redactor, err := p.resolveSecrets(execCtx, &spec)
	if err != nil {
		code := string(domain.CodeOf(err))
		p.appendLog(persistCtx, op, redactor, "error", domain.PhaseValidate,
			"cannot resolve secret: "+domain.MessageOf(err), map[string]string{"errorCode": code})
		p.finish(persistCtx, op, domain.StatusFailed, nil, code, domain.MessageOf(err))
		return
	}

	sensitiveEnvKeys := append(append([]string(nil), p.defaults.SensitiveEnvKeys...), spec.SensitiveEnvKeys...)
	p.appendLog(persistCtx, op, redactor, "info", domain.PhaseExecute, "executing command", map[string]string{
		"argv":             strings.Join(domain.RedactArgv(spec.Argv, spec.SensitiveArgIndexes), " "),
		"workingDirectory": spec.WorkingDirectory,
		"dryRun":           strconv.FormatBool(op.DryRun),
		"environment":      formatEnv(domain.RedactEnv(spec.Environment, sensitiveEnvKeys)),
	})

	result, runErr := p.exec.Run(execCtx, spec)

	var exitCode *int
	status := domain.StatusSucceeded
	errorCode := ""
	errorMessage := ""
	if runErr != nil {
		errorCode = string(domain.CodeOf(runErr))
		errorMessage = domain.MessageOf(runErr)
		if domain.CodeOf(runErr) == v1.CodeExecCancelled {
			status = domain.StatusCancelled
		} else {
			status = domain.StatusFailed
		}
		if domain.CodeOf(runErr) == v1.CodeExecExitNonZero {
			code := result.ExitCode
			exitCode = &code
		}
	} else if result.Executed {
		code := result.ExitCode
		exitCode = &code
	}

	if result.Stdout != "" {
		p.appendLog(persistCtx, op, redactor, "info", domain.PhaseExecute, "command stdout",
			map[string]string{"stdout": result.Stdout})
	}
	if result.Stderr != "" {
		level := "warn"
		if status != domain.StatusSucceeded {
			level = "error"
		}
		p.appendLog(persistCtx, op, redactor, level, domain.PhaseExecute, "command stderr",
			map[string]string{"stderr": result.Stderr})
	}

	summary := map[string]string{
		"status":     string(status),
		"durationMs": strconv.FormatInt(result.DurationMS, 10),
		"truncated":  strconv.FormatBool(result.Truncated),
	}
	if exitCode != nil {
		summary["exitCode"] = strconv.Itoa(*exitCode)
	}
	if errorCode != "" {
		summary["errorCode"] = errorCode
	}
	level := "info"
	if status != domain.StatusSucceeded {
		level = "error"
	}
	p.appendLog(persistCtx, op, nil, level, domain.PhaseFinalize, "operation finished", summary)

	p.finish(persistCtx, op, status, exitCode, errorCode, errorMessage)
	logger.Info("operation finished", "status", string(status), "errorCode", errorCode)
}

// resolveSecrets 在使用时刻把 SecretRef 解析成明文并合并进命令环境。返回值是
// 按值脱敏用的 Redactor——只有按值替换才能拦住出现在 stdout、stderr 或错误信息
// 里的凭据。
func (p *Pool) resolveSecrets(ctx context.Context, spec *domain.CommandSpec) (*domain.Redactor, error) {
	values := make([]string, 0, len(spec.SecretEnv))
	if len(spec.SecretEnv) == 0 {
		return p.redactorFor(spec, values), nil
	}

	if p.resolver == nil {
		return nil, domain.NewError(v1.CodeSecretUnresolved, "spec.secretEnvironment is not supported by this daemon")
	}

	environment := make(map[string]string, len(spec.Environment)+len(spec.SecretEnv))
	for key, value := range spec.Environment {
		environment[key] = value
	}
	for name, ref := range spec.SecretEnv {
		value, err := p.resolver.Resolve(ctx, ref)
		if err != nil {
			// 解析失败时不得把已解析出的其他凭据写进日志，因此这里只返回错误。
			return domain.NewRedactor(values...), err
		}
		environment[name] = value
		values = append(values, value)
	}
	spec.Environment = environment
	spec.SecretEnv = nil

	return p.redactorFor(spec, values), nil
}

// redactorFor 同时覆盖两种脱敏来源：显式标记为敏感的变量值，以及解析出的凭据值。
func (p *Pool) redactorFor(spec *domain.CommandSpec, secretValues []string) *domain.Redactor {
	values := append([]string(nil), secretValues...)
	for _, key := range append(append([]string(nil), p.defaults.SensitiveEnvKeys...), spec.SensitiveEnvKeys...) {
		if value, ok := spec.Environment[key]; ok {
			values = append(values, value)
		}
	}
	return domain.NewRedactor(values...)
}

func (p *Pool) appendLog(ctx context.Context, op *domain.Operation, redactor *domain.Redactor, level, phase, message string, fields map[string]string) {
	if err := p.repo.AppendLog(ctx, domain.LogEntry{
		OperationID: op.ID,
		Level:       level,
		Phase:       phase,
		Message:     message,
		Fields:      redactor.RedactFields(fields),
		Time:        p.now(),
	}); err != nil {
		p.logger.Error("failed to append operation log", "operationId", op.ID, "error", err)
	}
}

func (p *Pool) finish(ctx context.Context, op *domain.Operation, status domain.Status, exitCode *int, errorCode, errorMessage string) {
	if _, err := p.repo.Finish(ctx, domain.FinishInput{
		OperationID:  op.ID,
		Status:       status,
		ExitCode:     exitCode,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
	}, p.now()); err != nil {
		p.logger.Error("failed to persist operation result", "operationId", op.ID, "error", err)
	}
}

func formatEnv(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+env[key])
	}
	return strings.Join(parts, " ")
}

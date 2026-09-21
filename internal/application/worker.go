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
	defaults Defaults
	cancels  *cancelRegistry
	workers  int
	idle     time.Duration
	logger   *slog.Logger
	wake     chan struct{}
	now      func() time.Time
}

func newPool(repo Repository, exec Executor, defaults Defaults, cancels *cancelRegistry, workers int, logger *slog.Logger) *Pool {
	if workers < 1 {
		workers = 1
	}
	return &Pool{
		repo:     repo,
		exec:     exec,
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
		p.appendLog(persistCtx, op, "error", domain.PhaseValidate, "operation rejected: "+domain.MessageOf(err),
			map[string]string{"errorCode": code})
		p.finish(persistCtx, op, domain.StatusFailed, nil, code, domain.MessageOf(err))
		return
	}
	spec.DryRun = op.DryRun

	sensitiveEnvKeys := append(append([]string(nil), p.defaults.SensitiveEnvKeys...), spec.SensitiveEnvKeys...)
	p.appendLog(persistCtx, op, "info", domain.PhaseExecute, "executing command", map[string]string{
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
		p.appendLog(persistCtx, op, "info", domain.PhaseExecute, "command stdout",
			map[string]string{"stdout": result.Stdout})
	}
	if result.Stderr != "" {
		level := "warn"
		if status != domain.StatusSucceeded {
			level = "error"
		}
		p.appendLog(persistCtx, op, level, domain.PhaseExecute, "command stderr",
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
	p.appendLog(persistCtx, op, level, domain.PhaseFinalize, "operation finished", summary)

	p.finish(persistCtx, op, status, exitCode, errorCode, errorMessage)
	logger.Info("operation finished", "status", string(status), "errorCode", errorCode)
}

func (p *Pool) appendLog(ctx context.Context, op *domain.Operation, level, phase, message string, fields map[string]string) {
	if err := p.repo.AppendLog(ctx, domain.LogEntry{
		OperationID: op.ID,
		Level:       level,
		Phase:       phase,
		Message:     message,
		Fields:      fields,
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

package application

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/idgen"
)

const defaultIdlePoll = 250 * time.Millisecond

type Pool struct {
	repo     Repository
	exec     Executor
	resolver SecretResolver
	defaults Defaults
	cancels  *cancelRegistry
	runtimes *RuntimeService
	backups  *BackupService
	deploys  *DeployService
	workers  int
	idle     time.Duration
	logger   *slog.Logger
	wake     chan struct{}
	now      func() time.Time
	// newID 为自动重试生成新的 Operation ID。与 Service 共用同一个生成器，
	// 因此重试链上的每一跳在日志与审计里看起来都是普通的 Operation。
	newID func(prefix string) string
	// jitter 返回 [0,1) 的均匀随机数，用于给退避加抖动。可注入是为了让退避序列
	// 在测试里可断言——不可注入的随机会让「退避是否正确」无法被测试钉住。
	jitter func() float64
}

func newPool(repo Repository, exec Executor, resolver SecretResolver, defaults Defaults, cancels *cancelRegistry, runtimes *RuntimeService, backups *BackupService, deploys *DeployService, workers int, logger *slog.Logger, newID func(string) string) *Pool {
	if workers < 1 {
		workers = 1
	}
	if newID == nil {
		newID = idgen.New
	}
	return &Pool{
		repo:     repo,
		exec:     exec,
		resolver: resolver,
		defaults: defaults,
		cancels:  cancels,
		runtimes: runtimes,
		backups:  backups,
		deploys:  deploys,
		workers:  workers,
		idle:     defaultIdlePoll,
		logger:   logger,
		wake:     make(chan struct{}, 1),
		now:      func() time.Time { return time.Now().UTC() },
		newID:    newID,
		jitter:   rand.Float64,
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

	if isRuntimeKind(op.Kind) {
		p.executeRuntime(persistCtx, execCtx, op)
		return
	}
	if isBackupKind(op.Kind) {
		p.executeBackup(persistCtx, execCtx, op)
		return
	}
	if isDeployKind(op.Kind) {
		p.executeDeploy(persistCtx, execCtx, op)
		return
	}

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

// executeRuntime 执行 runtime.* 操作。它与 executor 路径共用 Operation 的创建、锁、
// 状态机、取消与落库，只把「做什么」换成 RuntimeAdapter 调用：
//
//   - runtime.start 幂等地先 Prepare 再 Start。先 Prepare 不是可选的：适配器合约明确
//     要求「未 Prepare 直接 Start 必须被拒绝」，所以「把应用跑起来」这一个操作必须自带
//     准备步骤，否则在从未 prepare 的主机上 start 永远失败。
//   - runtime.stop 只 Stop——systemd 的 stop 对未运行的 unit 同样成功，天然幂等。
//
// 执行时读「应用的当前规格」而不是创建时的快照：这样 spec put 之后重试 runtime.start
// 启动的是最新 manifest，与「一个应用一份当前规格」的语义一致。
func (p *Pool) executeRuntime(persistCtx, execCtx context.Context, op *domain.Operation) {
	startedAt := p.now()

	if p.runtimes == nil {
		p.finishRuntimeFailure(persistCtx, op, errRuntimeUnsupported(), startedAt)
		return
	}
	app, spec, err := p.runtimes.Resolve(execCtx, op.Resource)
	if err != nil {
		p.finishRuntimeFailure(persistCtx, op, err, startedAt)
		return
	}
	adapter, err := p.runtimes.requireAdapter()
	if err != nil {
		p.finishRuntimeFailure(persistCtx, op, err, startedAt)
		return
	}

	target := map[string]string{"application": app.Name, "unit": spec.Systemd.UnitName}
	switch op.Kind {
	case v1.KindRuntimeStart:
		p.appendLog(persistCtx, op, nil, "info", domain.PhasePrepare, "开始准备运行时（幂等）", target)
		if err := adapter.Prepare(execCtx, spec); err != nil {
			p.finishRuntimeFailure(persistCtx, op, err, startedAt)
			return
		}
		p.recordRuntimeDecision(persistCtx, op, app, spec)
		if err := adapter.Start(execCtx, spec); err != nil {
			p.finishRuntimeFailure(persistCtx, op, err, startedAt)
			return
		}
		p.appendLog(persistCtx, op, nil, "info", domain.PhaseExecute, "应用已启动", target)
	case v1.KindRuntimeStop:
		if err := adapter.Stop(execCtx, spec); err != nil {
			p.finishRuntimeFailure(persistCtx, op, err, startedAt)
			return
		}
		p.appendLog(persistCtx, op, nil, "info", domain.PhaseExecute, "应用已停止", target)
	default:
		// 创建侧已经限制了 kind；走到这里说明有人绕过 Service 直接写了库。
		p.finishRuntimeFailure(persistCtx, op,
			domain.NewError(v1.CodeInvalidRequest, "unsupported runtime kind %q", op.Kind), startedAt)
		return
	}

	p.appendLog(persistCtx, op, nil, "info", domain.PhaseFinalize, "operation finished", map[string]string{
		"status":     string(domain.StatusSucceeded),
		"durationMs": strconv.FormatInt(time.Since(startedAt).Milliseconds(), 10),
	})
	p.finish(persistCtx, op, domain.StatusSucceeded, nil, "", "")
	p.logger.Info("runtime operation finished", "operationId", op.ID, "kind", op.Kind, "application", app.Name)
}

// recordRuntimeDecision 把 Prepare 的档位决策写进 Operation 日志与审计：同一份 manifest
// 在不同 systemd 版本的主机上会生成不同的 unit，这份记录是事后唯一的解释来源。
// 适配器没提供决策（例如假适配器、没有 reporter）时什么都不写，而不是编一条空档位。
func (p *Pool) recordRuntimeDecision(ctx context.Context, op *domain.Operation, app *domain.Application, spec *domain.ApplicationSpec) {
	decision, ok := p.runtimes.decision(ctx, spec)
	if !ok {
		return
	}

	fields := map[string]string{
		"application": app.Name,
		"unitName":    decision.UnitName,
		"unitPath":    decision.UnitPath,
		"tier":        decision.Tier,
	}
	if decision.SystemdVersion > 0 {
		fields["systemdVersion"] = strconv.Itoa(decision.SystemdVersion)
	}
	if len(decision.Degradations) > 0 {
		fields["degradations"] = strings.Join(decision.Degradations, "; ")
	}
	p.appendLog(ctx, op, nil, "info", domain.PhasePrepare, "运行时档位已确定", fields)

	if err := p.repo.AppendAudit(ctx, domain.AuditEvent{
		EventType:   domain.EventRuntimePrepared,
		OperationID: op.ID,
		Resource:    app.ID,
		Result:      "prepared",
		Time:        decision.DecidedAt,
		Details:     fields,
	}); err != nil {
		p.logger.Error("failed to append runtime prepare audit", "operationId", op.ID, "error", err)
	}
}

// finishRuntimeFailure 把适配器错误映射成终态。取消走 cancelled（与 executor 路径一致），
// 其余一律 failed，错误码原样保留——RUNTIME_UNSUPPORTED、RUNTIME_NOT_READY、
// SECRET_UNRESOLVED、MANIFEST_INVALID 都是调用方能据以行动的码，不该被抹成 INTERNAL。
func (p *Pool) finishRuntimeFailure(ctx context.Context, op *domain.Operation, err error, startedAt time.Time) {
	code := domain.CodeOf(err)
	status := domain.StatusFailed
	if errors.Is(err, context.Canceled) || code == v1.CodeExecCancelled {
		status = domain.StatusCancelled
		code = v1.CodeExecCancelled
	}

	p.appendLog(ctx, op, nil, "error", domain.PhaseExecute, "运行时操作失败: "+domain.MessageOf(err), map[string]string{
		"errorCode":  string(code),
		"durationMs": strconv.FormatInt(time.Since(startedAt).Milliseconds(), 10),
	})
	p.finish(ctx, op, status, nil, string(code), domain.MessageOf(err))
	p.logger.Info("runtime operation finished", "operationId", op.ID, "kind", op.Kind,
		"status", string(status), "errorCode", string(code))
}

// executeBackup 执行备份类操作。
//
// 与 executeRuntime 一样，它复用 Operation 的创建、锁、状态机、取消与落库，只把
// 「做什么」换成 BackupService 的调用。备份**执行时读当前的策略**（backup.run /
// backup.verify），因此改了策略之后重试用的是最新那份；只有恢复的模式是创建时
// 定下的，因为它在那时被确认过。
func (p *Pool) executeBackup(persistCtx, execCtx context.Context, op *domain.Operation) {
	if p.backups == nil || !p.backups.Configured() {
		p.finish(persistCtx, op, domain.StatusFailed, nil,
			string(v1.CodeConfigInvalid), domain.MessageOf(errBackupUnsupported()))
		return
	}

	logf := func(level, phase, message string, fields map[string]string) {
		p.appendLog(persistCtx, op, nil, level, phase, message, fields)
	}

	var err error
	switch op.Kind {
	case v1.KindBackupRun:
		var policy *domain.BackupPolicy
		policy, err = p.backups.GetPolicy(execCtx, op.Resource)
		if err == nil {
			err = p.backups.ExecuteRun(execCtx, policy, op.ID, logf)
		}
	case v1.KindBackupVerify:
		err = p.backups.ExecuteVerify(execCtx, op.Resource, logf)
	case v1.KindBackupRestore:
		var options RestoreOptions
		options, err = DecodeRestoreOptions(op.Spec)
		if err == nil {
			err = p.backups.ExecuteRestore(execCtx, op.Resource, options.Mode, logf)
		}
	default:
		// 创建侧已经限制了 kind；走到这里说明有人绕过 Service 直接写了库。
		err = domain.NewError(v1.CodeInvalidRequest, "unsupported backup kind %q", op.Kind)
	}

	if err != nil {
		code := string(domain.CodeOf(err))
		message := domain.MessageOf(err)
		status := domain.StatusFailed
		if domain.CodeOf(err) == v1.CodeExecCancelled || errors.Is(err, context.Canceled) {
			status = domain.StatusCancelled
			code = string(v1.CodeExecCancelled)
		}
		p.appendLog(persistCtx, op, nil, "error", domain.PhaseExecute, "备份操作失败: "+message,
			map[string]string{"errorCode": code})
		p.finish(persistCtx, op, status, nil, code, message)
		p.logger.Info("backup operation finished", "operationId", op.ID, "kind", op.Kind,
			"status", string(status), "errorCode", code)
		return
	}

	p.appendLog(persistCtx, op, nil, "info", domain.PhaseFinalize, "backup operation finished", map[string]string{
		"status": string(domain.StatusSucceeded),
	})
	p.finish(persistCtx, op, domain.StatusSucceeded, nil, "", "")
	p.logger.Info("backup operation finished", "operationId", op.ID, "kind", op.Kind, "status", "succeeded")
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

// finish 落库终态，并在应当重试时把下一次尝试**一并**提交。
//
// 重试计划作为 FinishInput 的一部分传下去，由仓储在同一个事务里创建——分成两步做的话，
// 两步之间崩溃会静默丢掉这次重试，而且没有任何迹象（失败的操作还在，只是不会重试）。
func (p *Pool) finish(ctx context.Context, op *domain.Operation, status domain.Status, exitCode *int, errorCode, errorMessage string) {
	input := domain.FinishInput{
		OperationID:  op.ID,
		Status:       status,
		ExitCode:     exitCode,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
		Retry:        p.planRetry(op, status, errorCode),
	}
	if _, err := p.repo.Finish(ctx, input, p.now()); err != nil {
		p.logger.Error("failed to persist operation result", "operationId", op.ID, "error", err)
		return
	}
	if input.Retry != nil {
		// 叫醒空闲的 worker：退避可能很短，没必要等下一次轮询。
		p.Notify()
	}
}

// planRetry 依据**这一次失败**决定要不要排下一次尝试；不需要重试时返回 nil。
//
// 判定发生在终态落库的这一刻，而不是等守护进程重启之后再重算——后者会让
// 「当时到底该不该重试」取决于重启之后的策略快照，同一个失败会有两种结论。
func (p *Pool) planRetry(op *domain.Operation, status domain.Status, errorCode string) *domain.RetryPlan {
	// 成功没有「下一次尝试」可言；取消更不能有——用户主动取消之后还排自动重试，
	// 等于违抗指令。
	if status != domain.StatusFailed {
		return nil
	}
	return planRetryFor(op, v1.ErrorCode(errorCode), p.now(), p.newID, p.jitter)
}

// planRetryFor 是「要不要排下一次尝试」的**唯一判定点**，由两个调用方共用：
// worker（执行过程中失败）与 Recover（守护进程重启后被中断的操作）。
// 共用一份逻辑，「当时该不该重试、该等多久」才不会有两个说法。
//
// 退避在这里算一次就写进 NotBefore，**不在读取时重算**：否则重启后同一个操作会算出
// 不同的时间，退避窗口漂移，测试也无法断言。
func planRetryFor(op *domain.Operation, failureCode v1.ErrorCode, now time.Time, newID func(string) string, jitter func() float64) *domain.RetryPlan {
	if !op.RetryPolicy.Enabled() {
		return nil
	}
	if !op.RetryPolicy.Retryable(failureCode) {
		return nil
	}

	attempt := op.Attempt
	if attempt < 1 {
		attempt = 1
	}
	// 已达尝试上限：不再排新的尝试。最后一跳的终态与错误码保持原始失败原因，
	// 让运维看到的是「为什么失败」而不是「重试用完了」。
	if attempt+1 > op.RetryPolicy.MaxAttempts {
		return nil
	}

	delay := op.RetryPolicy.NextDelay(attempt, jitter())
	return &domain.RetryPlan{
		OperationID:     newID("op"),
		NotBefore:       now.Add(delay),
		RetryPolicyJSON: op.RetryPolicyJSON,
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

// executeDeploy 执行一次部署或回滚。
//
// 与 runtime.* / backup.* 共用 Operation 的创建、锁、状态机、取消与落库，只把「做什么」
// 换成 DeployService 调用。执行时**不读"应用的当前规格"**：要跑的那个版本的配置在创建期
// 就随 Operation.spec 存下了（releaseId），回滚要靠它才回得干净。
func (p *Pool) executeDeploy(persistCtx, execCtx context.Context, op *domain.Operation) {
	if p.deploys == nil || !p.deploys.Configured() {
		p.finish(persistCtx, op, domain.StatusFailed, nil,
			string(v1.CodeRuntimeUnsupport), "本部署未启用应用部署能力")
		return
	}

	releaseID, err := decodeReleaseID(op.Spec)
	if err != nil {
		p.finish(persistCtx, op, domain.StatusFailed, nil,
			string(domain.CodeOf(err)), domain.MessageOf(err))
		return
	}

	logf := func(level, phase, message string, fields map[string]string) {
		p.appendLog(persistCtx, op, nil, level, phase, message, fields)
	}

	switch op.Kind {
	case v1.KindAppRollback:
		err = p.deploys.ExecuteRollback(execCtx, releaseID, logf)
	default:
		err = p.deploys.ExecuteDeploy(execCtx, releaseID, logf)
	}

	if err != nil {
		status := domain.StatusFailed
		if domain.CodeOf(err) == v1.CodeExecCancelled {
			status = domain.StatusCancelled
		}
		code := string(domain.CodeOf(err))
		p.appendLog(persistCtx, op, nil, "error", domain.PhaseFinalize,
			"operation failed: "+domain.MessageOf(err), map[string]string{"errorCode": code})
		p.finish(persistCtx, op, status, nil, code, domain.MessageOf(err))
		return
	}
	p.finish(persistCtx, op, domain.StatusSucceeded, nil, "", "")
}

// decodeReleaseID 从 Operation 的 spec 里取出创建期选定的 release ID。
func decodeReleaseID(raw json.RawMessage) (string, error) {
	var options struct {
		ReleaseID string `json:"releaseId"`
	}
	if len(raw) == 0 {
		return "", domain.NewError(v1.CodeInvalidRequest, "部署类操作缺少 releaseId")
	}
	if err := json.Unmarshal(raw, &options); err != nil {
		return "", domain.NewError(v1.CodeInvalidRequest, "部署类操作的参数无法解析: %v", err)
	}
	if options.ReleaseID == "" {
		return "", domain.NewError(v1.CodeInvalidRequest, "部署类操作的 releaseId 为空")
	}
	return options.ReleaseID, nil
}

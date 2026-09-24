package application

import (
	"context"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/sqlite"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// centerJitter 让抖动系数恰好为 1，于是退避就是干净的指数序列，测试可以断言精确值。
// 调用处直接传函数值（写成 centerJitter，不带括号）。
func centerJitter() float64 { return 0.5 }

// retryRequest 造一个声明了重试策略的提交请求。base 取 1 秒（API 以秒为单位，
// 再小就不是合法输入），因此「真的等到重试」的用例最多等几秒。
func retryRequest(resource string, maxAttempts int) v1.CreateOperationRequest {
	req := submitRequest(resource)
	req.Retry = &v1.RetrySpec{MaxAttempts: maxAttempts, BaseDelaySeconds: 1, MaxDelaySeconds: 2}
	return req
}

// opRow 是直接查库读出的操作行。用原始 SQL 而不是端口方法，是因为这里要断言的正是
// 「到底产生了几行」——只看单个操作的返回值证明不了没有多余的行。
type opRow struct {
	ID        string
	Status    string
	Attempt   int
	RetryOf   string
	ErrorCode string
	NotBefore *string
}

func operationsOn(t *testing.T, store *sqlite.Store, resource string) []opRow {
	t.Helper()
	rows, err := store.DB().QueryContext(context.Background(),
		`SELECT id, status, attempt, COALESCE(retry_of, ''), COALESCE(error_code, ''), not_before
		 FROM operations WHERE resource = ? ORDER BY created_at, id`, resource)
	if err != nil {
		t.Fatalf("query operations: %v", err)
	}
	defer rows.Close()

	var out []opRow
	for rows.Next() {
		var row opRow
		var notBefore *string
		if err := rows.Scan(&row.ID, &row.Status, &row.Attempt, &row.RetryOf, &row.ErrorCode, &notBefore); err != nil {
			t.Fatalf("scan: %v", err)
		}
		row.NotBefore = notBefore
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// 未声明重试的操作失败后必须**不产生**任何新行——这是本迭代最容易写反的一条。
func TestNoRetryWithoutPolicy(t *testing.T) {
	exec := &fakeExec{run: func(context.Context, domain.CommandSpec) (domain.Result, error) {
		return domain.Result{Executed: true, ExitCode: 1}, domain.NewError(v1.CodeExecExitNonZero, "boom")
	}}
	rt, store := newTestRuntimeWith(t, Options{Executor: exec, Jitter: centerJitter})
	ctx := context.Background()

	op, _, err := rt.Service.Create(ctx, submitRequest("no-policy"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("process: %v", err)
	}

	rows := operationsOn(t, store, "no-policy")
	if len(rows) != 1 {
		t.Fatalf("未声明重试时不得产生新操作，got %d 行", len(rows))
	}
	if rows[0].ID != op.ID || rows[0].Status != string(domain.StatusFailed) {
		t.Fatalf("状态不符: %+v", rows[0])
	}
	if rows[0].Attempt != 1 {
		t.Fatalf("attempt 应当恒为 1，got %d", rows[0].Attempt)
	}
	if rows[0].NotBefore != nil {
		t.Fatal("未声明重试的操作不该有 not_before")
	}
}

func TestAutoRetrySchedulesNextAttempt(t *testing.T) {
	// 第一次失败、第二次成功：链上应当有两跳，第二跳 succeeded。
	var calls int
	exec := &fakeExec{run: func(context.Context, domain.CommandSpec) (domain.Result, error) {
		calls++
		if calls == 1 {
			return domain.Result{Executed: true, ExitCode: 1}, domain.NewError(v1.CodeExecExitNonZero, "boom")
		}
		return domain.Result{Executed: true, ExitCode: 0}, nil
	}}
	rt, store := newTestRuntimeWith(t, Options{Executor: exec, Jitter: centerJitter})
	ctx := context.Background()

	first, _, err := rt.Service.Create(ctx, retryRequest("chain", 3))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("process: %v", err)
	}

	rows := operationsOn(t, store, "chain")
	if len(rows) != 2 {
		t.Fatalf("应当产生第二跳，got %d 行", len(rows))
	}
	second := rows[1]
	if second.RetryOf != first.ID {
		t.Fatalf("retryOf want %s, got %s", first.ID, second.RetryOf)
	}
	if second.Attempt != 2 {
		t.Fatalf("attempt want 2, got %d", second.Attempt)
	}
	if second.Status != string(domain.StatusPending) {
		t.Fatalf("下一跳应当处于 pending，got %s", second.Status)
	}
	if second.NotBefore == nil {
		t.Fatal("下一跳必须带 not_before，否则退避不生效")
	}

	// 退避未到期之前不得被领取——这就是「退避不占 worker」的判据：
	// 领取查询直接把它过滤掉，而不是领到手里再等。
	if worked, err := rt.Pool.ProcessNext(ctx); err != nil || worked {
		t.Fatalf("退避未到期时不该领到任何操作（worked=%v err=%v）", worked, err)
	}

	// 等退避过去，第二跳应当被执行且成功。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if worked, err := rt.Pool.ProcessNext(ctx); err != nil {
			t.Fatalf("process: %v", err)
		} else if worked {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	rows = operationsOn(t, store, "chain")
	if rows[1].Status != string(domain.StatusSucceeded) {
		t.Fatalf("第二跳应当成功，got %s", rows[1].Status)
	}
	if calls != 2 {
		t.Fatalf("命令应当被执行两次，got %d", calls)
	}
}

func TestRetryStopsAtMaxAttempts(t *testing.T) {
	exec := &fakeExec{run: func(context.Context, domain.CommandSpec) (domain.Result, error) {
		return domain.Result{Executed: true, ExitCode: 1}, domain.NewError(v1.CodeExecExitNonZero, "always fails")
	}}
	rt, store := newTestRuntimeWith(t, Options{Executor: exec, Jitter: centerJitter})
	ctx := context.Background()

	if _, _, err := rt.Service.Create(ctx, retryRequest("exhaust", 3)); err != nil {
		t.Fatalf("create: %v", err)
	}

	rows := processUntilSettled(t, rt, store, "exhaust")
	if len(rows) != 3 {
		t.Fatalf("maxAttempts=3 应当恰好产生 3 跳，got %d", len(rows))
	}
	last := rows[2]
	if last.Attempt != 3 {
		t.Fatalf("最后一跳 attempt want 3, got %d", last.Attempt)
	}
	// 最后一跳保留**原始失败原因**，而不是「重试用完了」这类不解释原因的状态。
	if last.ErrorCode != string(v1.CodeExecExitNonZero) {
		t.Fatalf("最后一跳应当保留原始错误码，got %q", last.ErrorCode)
	}
	// 「不再排下一次」的判据就是链上恰好三行：真排了第四跳的话，
	// processUntilSettled 会一直处理到它安定，行数就不是 3 了。
	if last.RetryOf != rows[1].ID {
		t.Fatalf("第三跳应当挂在第二跳之下，got retryOf=%s", last.RetryOf)
	}
}

func TestRetrySkipsNonRetryableCode(t *testing.T) {
	// PERMISSION_DENIED 不会因为重试而改变，即便声明了策略也不该重试。
	exec := &fakeExec{run: func(context.Context, domain.CommandSpec) (domain.Result, error) {
		return domain.Result{Executed: true}, domain.NewError(v1.CodePermissionDenied, "nope")
	}}
	rt, store := newTestRuntimeWith(t, Options{Executor: exec, Jitter: centerJitter})
	ctx := context.Background()

	if _, _, err := rt.Service.Create(ctx, retryRequest("denied", 3)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("process: %v", err)
	}

	rows := operationsOn(t, store, "denied")
	if len(rows) != 1 {
		t.Fatalf("不可重试的错误码不得产生新操作，got %d 行", len(rows))
	}
}

func TestRetryNarrowsWhitelist(t *testing.T) {
	// 请求把白名单缩到只剩 EXEC_TIMEOUT，那么 EXEC_EXIT_NONZERO 就不该重试。
	exec := &fakeExec{run: func(context.Context, domain.CommandSpec) (domain.Result, error) {
		return domain.Result{Executed: true, ExitCode: 1}, domain.NewError(v1.CodeExecExitNonZero, "boom")
	}}
	rt, store := newTestRuntimeWith(t, Options{Executor: exec, Jitter: centerJitter})
	ctx := context.Background()

	req := retryRequest("narrowed", 3)
	req.Retry.RetryableErrorCodes = []string{string(v1.CodeExecTimeout)}
	if _, _, err := rt.Service.Create(ctx, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("process: %v", err)
	}

	rows := operationsOn(t, store, "narrowed")
	if len(rows) != 1 {
		t.Fatalf("被移出白名单的错误码不得重试，got %d 行", len(rows))
	}
}

func TestRetryRejectsBadPolicy(t *testing.T) {
	rt, _ := newTestRuntime(t, &fakeExec{}, allowAll)
	ctx := context.Background()

	cases := map[string]*v1.RetrySpec{
		"maxAttempts 超过上限": {MaxAttempts: domain.MaxRetryAttempts + 1},
		"maxAttempts 为负":   {MaxAttempts: -1},
		"base 大于 maxDelay": {MaxAttempts: 2, BaseDelaySeconds: 60, MaxDelaySeconds: 5},
		"把取消加进白名单": {
			MaxAttempts:         2,
			RetryableErrorCodes: []string{string(v1.CodeExecCancelled)},
		},
		"白名单里有不可重试的码": {
			MaxAttempts:         2,
			RetryableErrorCodes: []string{string(v1.CodePermissionDenied)},
		},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			req := submitRequest("bad-policy")
			req.Retry = spec
			if _, _, err := rt.Service.Create(ctx, req); domain.CodeOf(err) != v1.CodeRetryPolicyInvalid {
				t.Fatalf("want RETRY_POLICY_INVALID, got %v", err)
			}
		})
	}
}

// D7：dryRun 没有真实副作用，也就没有「值得重试的失败」。
func TestRetryRejectsDryRun(t *testing.T) {
	rt, _ := newTestRuntime(t, &fakeExec{}, allowAll)

	req := retryRequest("dry", 3)
	req.DryRun = true
	_, _, err := rt.Service.Create(context.Background(), req)
	if domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
}

// 手动重试是运维的判断，不受策略的自动上限约束，但仍保持链上计数连续。
func TestManualRetryIgnoresMaxAttempts(t *testing.T) {
	exec := &fakeExec{run: func(context.Context, domain.CommandSpec) (domain.Result, error) {
		return domain.Result{Executed: true, ExitCode: 1}, domain.NewError(v1.CodeExecExitNonZero, "boom")
	}}
	rt, store := newTestRuntimeWith(t, Options{Executor: exec, Jitter: centerJitter})
	ctx := context.Background()

	req := retryRequest("manual", 2)
	if _, _, err := rt.Service.Create(ctx, req); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 等到链安定：maxAttempts=2 时链上恰好两跳，且第二跳也已经失败。
	rows := processUntilSettled(t, rt, store, "manual")
	if len(rows) != 2 {
		t.Fatalf("maxAttempts=2 应当恰好两跳，got %d", len(rows))
	}
	second := rows[1]

	// 现在对第二跳做**手动**重试：即便已达 maxAttempts，运维仍可以再来一次。
	manual, err := rt.Service.Retry(ctx, second.ID)
	if err != nil {
		t.Fatalf("manual retry: %v", err)
	}

	rows = operationsOn(t, store, "manual")
	if len(rows) != 3 {
		t.Fatalf("手动重试应当产生第三跳，got %d 行", len(rows))
	}
	if manual.Attempt != 3 {
		t.Fatalf("手动重试的 attempt 应当连续递增到 3，got %d", manual.Attempt)
	}
	if manual.RetryOf != second.ID {
		t.Fatalf("retryOf want %s, got %s", second.ID, manual.RetryOf)
	}
}

// 取消之后不得再排自动重试——用户主动取消之后还重试等于违抗指令。
func TestCancelledOperationIsNotRetried(t *testing.T) {
	exec := &fakeExec{run: func(context.Context, domain.CommandSpec) (domain.Result, error) {
		return domain.Result{Executed: true}, domain.NewError(v1.CodeExecCancelled, "cancelled")
	}}
	rt, store := newTestRuntimeWith(t, Options{Executor: exec, Jitter: centerJitter})
	ctx := context.Background()

	if _, _, err := rt.Service.Create(ctx, retryRequest("cancelled", 3)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("process: %v", err)
	}

	rows := operationsOn(t, store, "cancelled")
	if len(rows) != 1 {
		t.Fatalf("取消不得触发重试，got %d 行", len(rows))
	}
	if rows[0].Status != string(domain.StatusCancelled) {
		t.Fatalf("want cancelled, got %s", rows[0].Status)
	}
}

// 重试的尝试是**普通的 pending 操作**，必须等同一 resource 的锁释放，不插队。
func TestRetryWaitsForResourceLock(t *testing.T) {
	var calls int
	exec := &fakeExec{run: func(context.Context, domain.CommandSpec) (domain.Result, error) {
		calls++
		if calls == 1 {
			return domain.Result{Executed: true, ExitCode: 1}, domain.NewError(v1.CodeExecExitNonZero, "boom")
		}
		return domain.Result{Executed: true}, nil
	}}
	// 用可拨动的时钟：固定时钟做不到「让退避到期」，因为 not_before 永远等于 now+delay。
	// 手动把时钟推到退避之后，「没领到」就只可能是锁的原因。
	current := time.Now().UTC()
	rt, store := newTestRuntimeWith(t, Options{
		Executor: exec,
		Jitter:   centerJitter,
		Now:      func() time.Time { return current },
	})
	ctx := context.Background()

	if _, _, err := rt.Service.Create(ctx, retryRequest("locked", 3)); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 第一跳失败后会排出第二跳，而此时没有任何锁被持有。
	if _, err := rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("process: %v", err)
	}
	rows := operationsOn(t, store, "locked")
	if len(rows) != 2 {
		t.Fatalf("应当排出第二跳，got %d 行", len(rows))
	}

	// 手工占住同一资源的锁：第二跳即便已过退避也不得被领取。
	// 把时钟推到退避之后，让第二跳在时间上已经可以领取。
	current = current.Add(time.Minute)

	// 锁行是被 releaseLock **UPDATE** 释放的，行本身还在（resource 是主键），
	// 所以重新占用要 UPDATE 而不是 INSERT。
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE resource_locks SET owner_operation_id = ?, acquired_at = ?, released_at = NULL
		 WHERE resource = ?`, rows[0].ID, time.Now().UTC().Format(time.RFC3339Nano), "locked"); err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	if worked, err := rt.Pool.ProcessNext(ctx); err != nil || worked {
		t.Fatalf("资源被占用时不该领到重试（worked=%v err=%v）", worked, err)
	}

	// 释放锁之后它应当能被正常领取。
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE resource_locks SET released_at = ? WHERE resource = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), "locked"); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	if worked, err := rt.Pool.ProcessNext(ctx); err != nil || !worked {
		t.Fatalf("锁释放后应当能领到重试（worked=%v err=%v）", worked, err)
	}
	if calls != 2 {
		t.Fatalf("命令应当被执行两次，got %d", calls)
	}
}

// D5：重启恢复**只**对显式声明了重试策略的操作重放。
func TestRecoveryRetriesOnlyOperationsWithPolicy(t *testing.T) {
	t.Run("声明了重试的操作会被排一次重试", func(t *testing.T) {
		rt, store := newTestRuntimeWith(t, Options{Jitter: centerJitter})
		ctx := context.Background()

		op, _, err := rt.Service.Create(ctx, retryRequest("interrupted-policy", 3))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// 领取但不执行，模拟「正在跑的时候守护进程没了」。
		if _, err := store.ClaimNextPending(ctx, time.Now().UTC()); err != nil {
			t.Fatalf("claim: %v", err)
		}

		if _, err := Recover(ctx, store, discardLogger(), time.Now().UTC(), RecoveryOptions{Jitter: centerJitter}); err != nil {
			t.Fatalf("recover: %v", err)
		}

		rows := operationsOn(t, store, "interrupted-policy")
		if len(rows) != 2 {
			t.Fatalf("声明了重试的被中断操作应当排一次重试，got %d 行", len(rows))
		}
		if rows[0].ID != op.ID || rows[0].ErrorCode != string(v1.CodeDaemonRestarted) {
			t.Fatalf("第一跳应当标记为 DAEMON_RESTARTED，got %+v", rows[0])
		}
		if rows[1].RetryOf != op.ID || rows[1].Attempt != 2 {
			t.Fatalf("重试链不对: %+v", rows[1])
		}
	})

	t.Run("未声明重试的操作一字不变", func(t *testing.T) {
		rt, store := newTestRuntimeWith(t, Options{Jitter: centerJitter})
		ctx := context.Background()

		if _, _, err := rt.Service.Create(ctx, submitRequest("interrupted-plain")); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := store.ClaimNextPending(ctx, time.Now().UTC()); err != nil {
			t.Fatalf("claim: %v", err)
		}

		if _, err := Recover(ctx, store, discardLogger(), time.Now().UTC(), RecoveryOptions{Jitter: centerJitter}); err != nil {
			t.Fatalf("recover: %v", err)
		}

		rows := operationsOn(t, store, "interrupted-plain")
		if len(rows) != 1 {
			t.Fatalf("默认语义是「标记失败、不重放」，不得产生新行，got %d 行", len(rows))
		}
		if rows[0].Status != string(domain.StatusFailed) {
			t.Fatalf("want failed, got %s", rows[0].Status)
		}
	})
}

// processUntilSettled 反复领取执行，直到「最后一跳已经进入终态」。
//
// 只数行数是不够的：行数会在最后一跳还处于 pending 时就达到预期，
// 此时它的 error_code 还是空的，断言会看到半成品。
func processUntilSettled(t *testing.T, rt *Runtime, store *sqlite.Store, resource string) []opRow {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := rt.Pool.ProcessNext(context.Background()); err != nil {
			t.Fatalf("process: %v", err)
		}
		rows := operationsOn(t, store, resource)
		if len(rows) == 0 {
			t.Fatalf("资源 %s 上没有任何操作", resource)
		}
		switch last := rows[len(rows)-1]; last.Status {
		case string(domain.StatusPending), string(domain.StatusRunning):
			time.Sleep(20 * time.Millisecond)
		default:
			return rows
		}
	}
	t.Fatalf("资源 %s 上的操作始终没有安定下来", resource)
	return nil
}

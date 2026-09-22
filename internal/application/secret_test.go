package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/domain"
)

type fakeResolver struct {
	values map[string]string
	fail   bool
}

func (f *fakeResolver) Resolve(_ context.Context, ref domain.SecretRef) (string, error) {
	if f.fail {
		return "", domain.NewError(v1.CodeSecretUnresolved, "resolver is unavailable")
	}
	value, ok := f.values[ref.String()]
	if !ok {
		return "", domain.NewError(v1.CodeSecretUnresolved, "secret %q is not configured", ref.Name)
	}
	return value, nil
}

const secretValue = "s3cr3t-token-value"

func secretRequest(resource string, extraSpec string) v1.CreateOperationRequest {
	spec := `{"argv":["/usr/bin/true"],"secretEnvironment":{"DEPLOY_TOKEN":{"kind":"env","name":"DEPLOY_TOKEN"}}` + extraSpec + `}`
	return v1.CreateOperationRequest{
		Kind:     v1.KindExecutorCommand,
		Resource: resource,
		Spec:     json.RawMessage(spec),
	}
}

// 凭据值出现在 stdout 里时，必须按值脱敏——只按变量名脱敏拦不住这种情况。
func TestSecretValuesAreRedactedByValue(t *testing.T) {
	exec := &fakeExec{run: func(context.Context, domain.CommandSpec) (domain.Result, error) {
		return domain.Result{Executed: true, ExitCode: 0, Stdout: "deploying with " + secretValue}, nil
	}}
	rt, store := newTestRuntimeWith(t, Options{
		Executor: exec,
		Secrets:  &fakeResolver{values: map[string]string{"env:DEPLOY_TOKEN": secretValue}},
	})
	ctx := context.Background()

	op, _, err := rt.Service.Create(ctx, secretRequest("secret-demo", ""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("process: %v", err)
	}

	if len(exec.calls) != 1 {
		t.Fatalf("executor 调用次数：%d", len(exec.calls))
	}
	if got := exec.calls[0].Environment["DEPLOY_TOKEN"]; got != secretValue {
		t.Fatalf("解析出的凭据应当注入命令环境，got %q", got)
	}

	stored, err := store.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status != domain.StatusSucceeded {
		t.Fatalf("want succeeded, got %s (%s)", stored.Status, stored.ErrorCode)
	}

	logs, err := store.ListLogs(ctx, op.ID, 0, 100)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("应当有日志")
	}
	assertNoSecretLeak(t, logs, secretValue)
	if !containsAny(logs, domain.RedactedPlaceholder) {
		t.Fatalf("日志应当包含脱敏占位符 %q", domain.RedactedPlaceholder)
	}
}

// 解析失败时，操作必须以 SECRET_UNRESOLVED 失败，且日志里不能出现任何凭据。
func TestUnresolvedSecretFailsOperationWithoutLeaking(t *testing.T) {
	rt, store := newTestRuntimeWith(t, Options{
		Executor: &fakeExec{},
		Secrets:  &fakeResolver{values: map[string]string{}},
	})
	ctx := context.Background()

	op, _, err := rt.Service.Create(ctx, secretRequest("secret-missing", ""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("process: %v", err)
	}

	stored, err := store.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status != domain.StatusFailed || stored.ErrorCode != string(v1.CodeSecretUnresolved) {
		t.Fatalf("want failed/SECRET_UNRESOLVED, got %s/%s", stored.Status, stored.ErrorCode)
	}

	logs, err := store.ListLogs(ctx, op.ID, 0, 100)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	assertNoSecretLeak(t, logs, secretValue)
}

// 没有配置解析器时，带 SecretRef 的请求必须被明确拒绝，而不是静默跳过。
func TestSecretWithoutResolverIsRejected(t *testing.T) {
	rt, store := newTestRuntimeWith(t, Options{Executor: &fakeExec{}})
	ctx := context.Background()

	op, _, err := rt.Service.Create(ctx, secretRequest("no-resolver", ""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("process: %v", err)
	}

	stored, _ := store.GetOperation(ctx, op.ID)
	if stored.ErrorCode != string(v1.CodeSecretUnresolved) {
		t.Fatalf("want SECRET_UNRESOLVED, got %q", stored.ErrorCode)
	}
}

func TestExecuteRequestValidationForSecrets(t *testing.T) {
	rt, _ := newTestRuntimeWith(t, Options{Secrets: &fakeResolver{values: map[string]string{}}})
	ctx := context.Background()

	cases := map[string]string{
		"未知 secret 类型": `{"argv":["/usr/bin/true"],"secretEnvironment":{"T":{"kind":"vault","name":"x"}}}`,
		"缺少名称":         `{"argv":["/usr/bin/true"],"secretEnvironment":{"T":{"kind":"env","name":""}}}`,
		"与明文环境变量冲突":    `{"argv":["/usr/bin/true"],"environment":{"T":"plain"},"secretEnvironment":{"T":{"kind":"env","name":"x"}}}`,
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := rt.Service.Create(ctx, v1.CreateOperationRequest{
				Kind:     v1.KindExecutorCommand,
				Resource: "spec-" + name,
				Spec:     json.RawMessage(spec),
			})
			if domain.CodeOf(err) != v1.CodeInvalidRequest {
				t.Fatalf("want INVALID_REQUEST, got %v", err)
			}
		})
	}
}

// 显式声明为敏感的明文环境变量也应当按值脱敏。
func TestSensitiveEnvValuesAreRedactedByValue(t *testing.T) {
	exec := &fakeExec{run: func(context.Context, domain.CommandSpec) (domain.Result, error) {
		return domain.Result{Executed: true, ExitCode: 0, Stderr: "token=plain-sensitive"}, nil
	}}
	rt, store := newTestRuntimeWith(t, Options{Executor: exec})
	ctx := context.Background()

	spec := `{"argv":["/usr/bin/true"],"environment":{"API_KEY":"plain-sensitive"},"sensitiveEnvKeys":["API_KEY"]}`
	op, _, err := rt.Service.Create(ctx, v1.CreateOperationRequest{
		Kind:     v1.KindExecutorCommand,
		Resource: "sensitive-env",
		Spec:     json.RawMessage(spec),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := rt.Pool.ProcessNext(ctx); err != nil {
		t.Fatalf("process: %v", err)
	}

	logs, err := store.ListLogs(ctx, op.ID, 0, 100)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	assertNoSecretLeak(t, logs, "plain-sensitive")
}

func assertNoSecretLeak(t *testing.T, logs []domain.LogEntry, secret string) {
	t.Helper()
	for _, entry := range logs {
		if strings.Contains(entry.Message, secret) {
			t.Fatalf("日志消息泄漏了凭据：%s", entry.Message)
		}
		for key, value := range entry.Fields {
			if strings.Contains(value, secret) {
				t.Fatalf("日志字段 %s 泄漏了凭据：%s", key, value)
			}
		}
	}
}

func containsAny(logs []domain.LogEntry, needle string) bool {
	for _, entry := range logs {
		if strings.Contains(entry.Message, needle) {
			return true
		}
		for _, value := range entry.Fields {
			if strings.Contains(value, needle) {
				return true
			}
		}
	}
	return false
}

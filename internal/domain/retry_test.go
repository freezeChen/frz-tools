package domain

import (
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

func TestRetryPolicyValidate(t *testing.T) {
	valid := RetryPolicy{MaxAttempts: 3, BaseDelay: 5 * time.Second, MaxDelay: time.Minute}
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法规格被拒: %v", err)
	}

	cases := []struct {
		name   string
		policy RetryPolicy
	}{
		{"maxAttempts 为 0", RetryPolicy{MaxAttempts: 0}},
		{"maxAttempts 为负", RetryPolicy{MaxAttempts: -1}},
		{"maxAttempts 超过上限", RetryPolicy{MaxAttempts: MaxRetryAttempts + 1}},
		{"baseDelay 为负", RetryPolicy{MaxAttempts: 2, BaseDelay: -time.Second}},
		{"maxDelay 为负", RetryPolicy{MaxAttempts: 2, MaxDelay: -time.Second}},
		{"baseDelay 大于 maxDelay", RetryPolicy{MaxAttempts: 2, BaseDelay: time.Minute, MaxDelay: time.Second}},
		{
			"把取消加进白名单",
			RetryPolicy{MaxAttempts: 2, RetryableCodes: []v1.ErrorCode{v1.CodeExecCancelled}},
		},
		{
			"把权限拒绝加进白名单",
			RetryPolicy{MaxAttempts: 2, RetryableCodes: []v1.ErrorCode{v1.CodePermissionDenied}},
		},
		{
			"白名单里有一个不在默认可重试集合里的码",
			RetryPolicy{MaxAttempts: 2, RetryableCodes: []v1.ErrorCode{"SOMETHING_ELSE"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate()
			if err == nil {
				t.Fatal("非法策略必须被拒绝")
			}
			if got := CodeOf(err); got != v1.CodeRetryPolicyInvalid {
				t.Fatalf("错误码 want %s, got %s", v1.CodeRetryPolicyInvalid, got)
			}
		})
	}

	// 白名单可以**缩小**：只保留默认集合里的一个子集是合法的。
	narrowed := RetryPolicy{
		MaxAttempts:    2,
		RetryableCodes: []v1.ErrorCode{v1.CodeExecTimeout},
	}
	if err := narrowed.Validate(); err != nil {
		t.Fatalf("缩小白名单应当合法: %v", err)
	}
}

// Retryable 的判据是「默认白名单 ∩ 请求给出的子集」。
func TestRetryPolicyRetryable(t *testing.T) {
	t.Run("未声明重试时一律不可重试", func(t *testing.T) {
		policy := RetryPolicy{MaxAttempts: 1}
		for _, code := range defaultRetryableCodes() {
			if policy.Retryable(code) {
				t.Fatalf("maxAttempts=1 时 %s 不该可重试", code)
			}
		}
	})

	t.Run("默认白名单的每一行", func(t *testing.T) {
		policy := RetryPolicy{MaxAttempts: 3}

		retryable := []v1.ErrorCode{
			v1.CodeExecTimeout,
			v1.CodeExecExitNonZero,
			v1.CodeRuntimeNotReady,
			v1.CodeDaemonRestarted,
		}
		for _, code := range retryable {
			if !policy.Retryable(code) {
				t.Fatalf("%s 应当在默认白名单里", code)
			}
		}

		// 规格 D3 表格里明确不可重试的那些，逐条钉住。
		notRetryable := []v1.ErrorCode{
			v1.CodeExecCancelled,
			v1.CodePermissionDenied,
			v1.CodeLockBusy,
			v1.CodeManifestInvalid,
			v1.CodeInvalidRequest,
			v1.CodeConfigInvalid,
			v1.CodeSecretUnresolved,
			v1.CodeInternal,
		}
		for _, code := range notRetryable {
			if policy.Retryable(code) {
				t.Fatalf("%s 不该可重试", code)
			}
		}
	})

	t.Run("白名单被缩小之后只保留子集", func(t *testing.T) {
		policy := RetryPolicy{
			MaxAttempts:    3,
			RetryableCodes: []v1.ErrorCode{v1.CodeExecTimeout},
		}
		if !policy.Retryable(v1.CodeExecTimeout) {
			t.Fatal("子集内的码应当可重试")
		}
		if policy.Retryable(v1.CodeExecExitNonZero) {
			t.Fatal("被移出子集的码不该可重试")
		}
	})

	t.Run("不可重试的码无法被白名单救回来", func(t *testing.T) {
		// 即便调用方硬把它塞进 RetryableCodes，Retryable 也必须拒绝。
		policy := RetryPolicy{
			MaxAttempts:    3,
			RetryableCodes: []v1.ErrorCode{v1.CodeExecCancelled},
		}
		if policy.Retryable(v1.CodeExecCancelled) {
			t.Fatal("取消在任何情况下都不得重试")
		}
	})
}

// NextDelay 必须用注入的抖动算出确定的值，否则退避是否正确无法被测试钉住。
func TestRetryPolicyNextDelay(t *testing.T) {
	policy := RetryPolicy{MaxAttempts: 10, BaseDelay: 5 * time.Second, MaxDelay: 5 * time.Minute}

	t.Run("抖动居中时就是纯指数增长", func(t *testing.T) {
		// jitter = 0.5 时抖动系数为 1，得到不带抖动的基准序列。
		cases := []struct {
			attempt int
			want    time.Duration
		}{
			{1, 5 * time.Second},
			{2, 10 * time.Second},
			{3, 20 * time.Second},
			{4, 40 * time.Second},
			{5, 80 * time.Second},
			{6, 160 * time.Second},
			{7, 5 * time.Minute},  // 320s 被 maxDelay 截断
			{8, 5 * time.Minute},  // 之后一直停在上限
			{20, 5 * time.Minute}, // 远超上限也不溢出
		}
		for _, tc := range cases {
			if got := policy.NextDelay(tc.attempt, 0.5); got != tc.want {
				t.Fatalf("attempt %d: want %s, got %s", tc.attempt, tc.want, got)
			}
		}
	})

	t.Run("抖动落在 ±20% 内", func(t *testing.T) {
		if got := policy.NextDelay(1, 0); got != 4*time.Second {
			t.Fatalf("下界 want 4s, got %s", got)
		}
		if got := policy.NextDelay(1, 1); got != 6*time.Second {
			t.Fatalf("上界 want 6s, got %s", got)
		}
	})

	t.Run("maxDelay 是抖动之后的绝对上限", func(t *testing.T) {
		// 第 7 次的基准已是上限，再乘 1.2 也不得超过它。
		if got := policy.NextDelay(7, 1); got != 5*time.Minute {
			t.Fatalf("want 5m（上限）, got %s", got)
		}
	})

	t.Run("零值策略回落到默认基数与上限", func(t *testing.T) {
		empty := RetryPolicy{}
		if got := empty.NextDelay(1, 0.5); got != DefaultRetryBaseDelay {
			t.Fatalf("want %s, got %s", DefaultRetryBaseDelay, got)
		}
	})
}

func TestRetryPolicyFromSpecDefaults(t *testing.T) {
	t.Run("省略 maxAttempts 即不重试", func(t *testing.T) {
		policy := RetryPolicyFromSpec(v1.RetrySpec{})
		if policy.MaxAttempts != 1 || policy.Enabled() {
			t.Fatalf("want 不重试, got MaxAttempts=%d", policy.MaxAttempts)
		}
		if policy.BaseDelay != DefaultRetryBaseDelay || policy.MaxDelay != DefaultRetryMaxDelay {
			t.Fatalf("退避参数应当取默认值, got base=%s max=%s", policy.BaseDelay, policy.MaxDelay)
		}
	})

	t.Run("显式取值优先于默认值", func(t *testing.T) {
		policy := RetryPolicyFromSpec(v1.RetrySpec{
			MaxAttempts:      3,
			BaseDelaySeconds: 2,
			MaxDelaySeconds:  60,
		})
		if policy.MaxAttempts != 3 {
			t.Fatalf("want 3, got %d", policy.MaxAttempts)
		}
		if policy.BaseDelay != 2*time.Second || policy.MaxDelay != time.Minute {
			t.Fatalf("got base=%s max=%s", policy.BaseDelay, policy.MaxDelay)
		}
		if !policy.Enabled() {
			t.Fatal("MaxAttempts=3 应当启用重试")
		}
	})
}

// 读回持久化的策略必须与写入时的解释一致，否则「当时按什么策略重试」会有两个说法。
func TestParseRetryPolicyRoundTrip(t *testing.T) {
	raw := []byte(`{"maxAttempts":3,"baseDelaySeconds":5,"maxDelaySeconds":300}`)
	policy, err := ParseRetryPolicy(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if policy.MaxAttempts != 3 || policy.BaseDelay != 5*time.Second || policy.MaxDelay != 5*time.Minute {
		t.Fatalf("往返不一致: %+v", policy)
	}

	t.Run("空原文即不重试", func(t *testing.T) {
		policy, err := ParseRetryPolicy(nil)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if policy.Enabled() {
			t.Fatal("空策略不该启用重试")
		}
	})

	t.Run("原文坏掉时保守地不重试", func(t *testing.T) {
		policy, err := ParseRetryPolicy([]byte(`{not json`))
		if err == nil {
			t.Fatal("坏掉的原文应当报错")
		}
		if policy.Enabled() {
			t.Fatal("解析失败时必须退化成不重试，宁可少重试也不能乱重试")
		}
	})
}

func TestOperationRetryExhausted(t *testing.T) {
	policy := RetryPolicy{MaxAttempts: 3}

	cases := []struct {
		name string
		op   Operation
		want bool
	}{
		{"最后一跳失败", Operation{Status: StatusFailed, Attempt: 3, RetryPolicy: policy}, true},
		{"还有下一次", Operation{Status: StatusFailed, Attempt: 2, RetryPolicy: policy}, false},
		{"未声明重试", Operation{Status: StatusFailed, Attempt: 1}, false},
		{"成功", Operation{Status: StatusSucceeded, Attempt: 3, RetryPolicy: policy}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.op.RetryExhausted(); got != tc.want {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

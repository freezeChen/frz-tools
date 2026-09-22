package domain

import (
	"testing"
	"time"

	v1 "frz-tools/api/v1"
)

func cronSchedule(expr, tz string) *Schedule {
	return &Schedule{
		Name:            "demo",
		Kind:            ScheduleKindCron,
		Cron:            expr,
		Timezone:        tz,
		Resource:        "demo",
		Spec:            []byte(`{"argv":["/usr/bin/true"]}`),
		MissedRunPolicy: MissedRunSkip,
	}
}

func intervalSchedule(every time.Duration) *Schedule {
	return &Schedule{
		Name:            "demo",
		Kind:            ScheduleKindInterval,
		Interval:        every,
		Resource:        "demo",
		Spec:            []byte(`{"argv":["/usr/bin/true"]}`),
		MissedRunPolicy: MissedRunSkip,
	}
}

func TestScheduleValidation(t *testing.T) {
	cases := map[string]*Schedule{
		"缺少名称":        {Kind: ScheduleKindCron, Cron: "* * * * *", Resource: "r", Spec: []byte(`{}`)},
		"缺少 resource": {Name: "n", Kind: ScheduleKindCron, Cron: "* * * * *", Spec: []byte(`{}`)},
		"缺少 spec":     {Name: "n", Kind: ScheduleKindCron, Cron: "* * * * *", Resource: "r"},
		"未知类型":        {Name: "n", Kind: "hourly", Resource: "r", Spec: []byte(`{}`)},
		"cron 表达式为空":  {Name: "n", Kind: ScheduleKindCron, Resource: "r", Spec: []byte(`{}`)},
		"cron 表达式非法":  cronSchedule("not a cron", ""),
		"cron 带秒字段":   cronSchedule("0 0 2 * * *", ""),
		"cron 又给 interval": {
			Name: "n", Kind: ScheduleKindCron, Cron: "* * * * *", Interval: time.Hour,
			Resource: "r", Spec: []byte(`{}`),
		},
		"interval 为空": {Name: "n", Kind: ScheduleKindInterval, Resource: "r", Spec: []byte(`{}`)},
		"interval 过小": intervalSchedule(30 * time.Second),
		"interval 又给 cron": {
			Name: "n", Kind: ScheduleKindInterval, Interval: time.Hour, Cron: "* * * * *",
			Resource: "r", Spec: []byte(`{}`),
		},
		"时区无法解析": cronSchedule("0 2 * * *", "Mars/Olympus"),
		"错过策略非法": {
			Name: "n", Kind: ScheduleKindCron, Cron: "* * * * *", Resource: "r",
			Spec: []byte(`{}`), MissedRunPolicy: "maybe",
		},
	}
	for name, schedule := range cases {
		t.Run(name, func(t *testing.T) {
			if err := schedule.Validate(); domainCode(err) != v1.CodeScheduleInvalid {
				t.Fatalf("want SCHEDULE_INVALID, got %v", err)
			}
		})
	}
}

func TestScheduleValidationAcceptsValidPlans(t *testing.T) {
	valid := []*Schedule{
		cronSchedule("0 2 * * *", ""),
		cronSchedule("*/15 * * * *", "Asia/Shanghai"),
		intervalSchedule(time.Hour),
	}
	for _, schedule := range valid {
		if err := schedule.Validate(); err != nil {
			t.Fatalf("应当有效: %+v -> %v", schedule, err)
		}
	}
}

// 时区必须真的影响触发时刻，否则「按 Asia/Shanghai 的每天 2 点」会变成 UTC 的 2 点。
func TestCronTimezoneChangesFiringInstant(t *testing.T) {
	utcEval, err := NewEvaluator(cronSchedule("0 2 * * *", "UTC"))
	if err != nil {
		t.Fatalf("utc evaluator: %v", err)
	}
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("环境缺少时区数据: %v", err)
	}
	shanghaiEval, err := NewEvaluator(cronSchedule("0 2 * * *", "Asia/Shanghai"))
	if err != nil {
		t.Fatalf("shanghai evaluator: %v", err)
	}

	// 各自以自己时区的当地午夜为起点，比较「同一个当地 2 点」落在哪个绝对时刻。
	utcNext := utcEval.Next(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	shanghaiNext := shanghaiEval.Next(time.Date(2026, 3, 1, 0, 0, 0, 0, shanghai))

	if utcNext.In(time.UTC).Hour() != 2 {
		t.Fatalf("UTC 计划应当在 UTC 2 点触发，got %s", utcNext)
	}
	if shanghaiNext.In(shanghai).Hour() != 2 {
		t.Fatalf("上海计划应当在上海时间 2 点触发，got %s", shanghaiNext.In(shanghai))
	}
	// 同一个墙上时间的 2 点，两个时区的绝对时刻相差 8 小时。
	if diff := utcNext.Sub(shanghaiNext); diff != 8*time.Hour {
		t.Fatalf("两个时区的触发时刻应当相差 8 小时，got %s", diff)
	}
}

// 间隔计划以「上一次计划时刻」为基准递推，执行耗时不会把计划时间推后。
func TestIntervalAdvancesFromScheduledMoment(t *testing.T) {
	eval, err := NewEvaluator(intervalSchedule(10 * time.Minute))
	if err != nil {
		t.Fatalf("evaluator: %v", err)
	}
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	first := eval.Next(start)
	if !first.Equal(start.Add(10 * time.Minute)) {
		t.Fatalf("want %s, got %s", start.Add(10*time.Minute), first)
	}
	// 即使这次执行花了 7 分钟，下一次计划时刻仍然是 +20 分钟而不是 +27。
	second := eval.Next(first)
	if !second.Equal(start.Add(20 * time.Minute)) {
		t.Fatalf("want %s, got %s", start.Add(20*time.Minute), second)
	}
}

func TestDueMomentsEnumeratesAndReportsTruncation(t *testing.T) {
	eval, err := NewEvaluator(intervalSchedule(time.Hour))
	if err != nil {
		t.Fatalf("evaluator: %v", err)
	}
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	moments, truncated := DueMoments(eval, from, from.Add(3*time.Hour), 0)
	if truncated {
		t.Fatal("三个小时内不应触发截断")
	}
	if len(moments) != 3 {
		t.Fatalf("want 3 moments, got %d", len(moments))
	}
	for i := 1; i < len(moments); i++ {
		if !moments[i].After(moments[i-1]) {
			t.Fatalf("时刻必须严格递增: %v", moments)
		}
	}

	// 上限必须被如实报告，否则停机很久后调用方会以为「只有这么多」。
	_, truncated = DueMoments(eval, from, from.Add(100*time.Hour), 5)
	if !truncated {
		t.Fatal("超过上限时必须报告截断")
	}
}

func TestDueMomentsReturnsNothingWhenNotDue(t *testing.T) {
	eval, err := NewEvaluator(cronSchedule("0 2 * * *", "UTC"))
	if err != nil {
		t.Fatalf("evaluator: %v", err)
	}
	from := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)
	moments, truncated := DueMoments(eval, from, from.Add(time.Hour), 0)
	if len(moments) != 0 || truncated {
		t.Fatalf("未到期时不应产生时刻: %v truncated=%v", moments, truncated)
	}
}

// 夏令时切换前后必须既不重复触发、也不整天漏掉。这里不假设具体时刻落在哪一秒
// （被跳过的那一小时如何处理由 cron 库决定），只断言每个本地日期恰好触发一次。
func TestCronAcrossDaylightSavingBoundary(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("环境缺少时区数据: %v", err)
	}
	for _, tc := range []struct {
		name string
		from time.Time
	}{
		{"春季跳进", time.Date(2026, 3, 7, 0, 0, 0, 0, newYork)},
		{"秋季回拨", time.Date(2026, 10, 31, 0, 0, 0, 0, newYork)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eval, err := NewEvaluator(cronSchedule("0 2 * * *", "America/New_York"))
			if err != nil {
				t.Fatalf("evaluator: %v", err)
			}

			seen := map[string]bool{}
			cursor := tc.from
			for i := 0; i < 10; i++ {
				next := eval.Next(cursor)
				if !next.After(cursor) {
					t.Fatalf("触发时刻必须严格递增: %s 之后得到 %s", cursor, next)
				}
				day := next.In(newYork).Format("2006-01-02")
				if seen[day] {
					t.Fatalf("本地日期 %s 被触发两次", day)
				}
				seen[day] = true
				cursor = next
			}
			if len(seen) != 10 {
				t.Fatalf("十次触发应当落在十个不同的本地日期，got %d", len(seen))
			}
		})
	}
}

// domainCode 是本文件内的小工具，避免每处都写一遍类型断言。
func domainCode(err error) v1.ErrorCode { return CodeOf(err) }

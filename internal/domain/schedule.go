package domain

import (
	"encoding/json"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"

	"github.com/robfig/cron/v3"
)

type ScheduleKind string

const (
	ScheduleKindCron     ScheduleKind = "cron"
	ScheduleKindInterval ScheduleKind = "interval"
)

// MissedRunPolicy 决定 opsd 停机期间错过的触发时刻如何补偿。
//
// 只有两种取值：skip 全部记为 missed 且不执行；runOnce 只补跑最近一次。
// 之所以没有 runAll：同一 resource 上最多允许一个未完成 Operation，而 resource
// 是计划自带的，所以「把积压的时刻逐个补跑」必然被锁拒绝——承诺了却做不到的策略
// 比没有更糟。需要连续补跑多次的场景应改用更短的间隔或拆分 resource。
type MissedRunPolicy string

const (
	MissedRunSkip MissedRunPolicy = "skip"
	MissedRunOnce MissedRunPolicy = "runOnce"
)

func (p MissedRunPolicy) Valid() bool {
	switch p {
	case MissedRunSkip, MissedRunOnce:
		return true
	}
	return false
}

// ScheduleRunResult 是一次触发的最终结果。刻意没有 "skipped"：没有代码路径会产生它，
// 留着只会让人以为存在「跳过但不算错过」这种语义。真需要时再加迁移。
type ScheduleRunResult string

const (
	RunDispatched ScheduleRunResult = "dispatched"
	RunMissed     ScheduleRunResult = "missed"
	RunFailed     ScheduleRunResult = "failed"
)

// MinInterval 是 interval 类计划的下限：低于这个粒度更适合用 systemd timer
// 或外部监控，而不是在 opsd 里常驻高频唤醒。
const MinInterval = time.Minute

// maxScheduledMoments 限制一次补偿里枚举的时刻数量。定时任务停机很久之后，
// 每分钟的计划会积累出几万个时刻；这里将其截断，避免一次恢复把数据库写爆。
const maxScheduledMoments = 1000

type Schedule struct {
	ID       string
	Name     string
	Enabled  bool
	Kind     ScheduleKind
	Cron     string
	Interval time.Duration
	Timezone string
	Resource string
	// OperationKind 是到点要创建哪种操作，省略时是 executor.command。
	//
	// 加这个字段之前调度器写死了 executor.command，于是「按计划跑备份」从模型上就
	// 做不到（迭代 2 规格 D6）。
	OperationKind   string
	Spec            json.RawMessage
	MissedRunPolicy MissedRunPolicy
	NextRunAt       *time.Time
	LastRunAt       *time.Time
	LastResult      string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	CreatedBy       string
}

// schedulableKinds 是计划可以触发的操作类型白名单。
//
// 刻意列白名单而不是「什么都行」：计划会在无人值守时自动执行，一个写错的操作类型
// 不该等到半夜才被发现。
var schedulableKinds = map[string]bool{
	v1.KindExecutorCommand: true,
	v1.KindRuntimeStart:    true,
	v1.KindRuntimeStop:     true,
	v1.KindBackupRun:       true,
	v1.KindBackupVerify:    true,
	v1.KindBackupRestore:   true,
}

// OperationKindOrDefault 返回落库时要写的操作类型；空值归一为 executor.command，
// 避免把空串写进一个有 NOT NULL DEFAULT 的列（那会让「省略」与「写了空」在库里不可分）。
// 导出是因为仓储适配器也要用它。
func (s *Schedule) OperationKindOrDefault() string {
	if s.OperationKind == "" {
		return v1.KindExecutorCommand
	}
	return s.OperationKind
}

func (s *Schedule) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return NewError(v1.CodeScheduleInvalid, "计划名称不能为空")
	}
	if strings.TrimSpace(s.Resource) == "" {
		return NewError(v1.CodeScheduleInvalid, "计划必须指定 resource")
	}
	if len(s.Spec) == 0 {
		return NewError(v1.CodeScheduleInvalid, "计划必须带 spec")
	}
	switch {
	case s.OperationKind == "":
		s.OperationKind = v1.KindExecutorCommand
	case !schedulableKinds[s.OperationKind]:
		return NewError(v1.CodeScheduleInvalid,
			"operationKind 取值非法: %q", s.OperationKind)
	}

	if !s.MissedRunPolicy.Valid() {
		return NewError(v1.CodeScheduleInvalid, "missedRunPolicy 取值非法: %q", s.MissedRunPolicy)
	}
	if _, err := s.Location(); err != nil {
		return err
	}

	switch s.Kind {
	case ScheduleKindCron:
		if strings.TrimSpace(s.Cron) == "" {
			return NewError(v1.CodeScheduleInvalid, "cron 类计划必须提供 cron 表达式")
		}
		if s.Interval != 0 {
			return NewError(v1.CodeScheduleInvalid, "cron 类计划不能同时提供 interval")
		}
		if _, err := parseCronSpec(s.Cron); err != nil {
			return err
		}
	case ScheduleKindInterval:
		if s.Interval <= 0 {
			return NewError(v1.CodeScheduleInvalid, "interval 类计划必须提供正整数间隔")
		}
		if s.Interval < MinInterval {
			return NewError(v1.CodeScheduleInvalid, "间隔不能小于 %s", MinInterval)
		}
		if s.Cron != "" {
			return NewError(v1.CodeScheduleInvalid, "interval 类计划不能同时提供 cron 表达式")
		}
	default:
		return NewError(v1.CodeScheduleInvalid, "计划类型取值非法: %q", s.Kind)
	}
	return nil
}

// Location 解析计划使用的时区；未配置时按 UTC 处理。
func (s *Schedule) Location() (*time.Location, error) {
	if strings.TrimSpace(s.Timezone) == "" {
		return time.UTC, nil
	}
	location, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return nil, NewError(v1.CodeScheduleInvalid, "无法解析时区 %q: %v", s.Timezone, err)
	}
	return location, nil
}

// Evaluator 计算计划的下一次触发时刻。只提供 Next，因为错过时刻的枚举可以
// 从已知的上次触发时刻不断向前推，不需要 Previous。
type Evaluator interface {
	// Next 返回严格晚于 after 的下一个计划时刻。
	Next(after time.Time) time.Time
}

// NewEvaluator 按计划类型构造触发器。
func NewEvaluator(s *Schedule) (Evaluator, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	location, err := s.Location()
	if err != nil {
		return nil, err
	}

	switch s.Kind {
	case ScheduleKindCron:
		spec, err := parseCronSpec(s.Cron)
		if err != nil {
			return nil, err
		}
		// Location 决定 cron 表达式按哪个时区解释，因此夏令时切换前后
		// 的触发时刻由它负责，而不是靠调用方换算。
		spec.Location = location
		return &cronEvaluator{spec: spec}, nil
	case ScheduleKindInterval:
		return &intervalEvaluator{interval: s.Interval}, nil
	}
	return nil, NewError(v1.CodeScheduleInvalid, "计划类型取值非法: %q", s.Kind)
}

// parseCronSpec 只接受标准 5 字段表达式：分 时 日 月 周。
// 不接受秒字段与 @every 之外的别名，避免同一份配置在不同实现下语义漂移。
func parseCronSpec(expr string) (*cron.SpecSchedule, error) {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse(strings.TrimSpace(expr))
	if err != nil {
		return nil, NewError(v1.CodeScheduleInvalid, "cron 表达式非法 %q: %v", expr, err)
	}
	spec, ok := schedule.(*cron.SpecSchedule)
	if !ok {
		return nil, NewError(v1.CodeScheduleInvalid, "cron 表达式非法 %q", expr)
	}
	return spec, nil
}

type cronEvaluator struct {
	spec *cron.SpecSchedule
}

func (e *cronEvaluator) Next(after time.Time) time.Time { return e.spec.Next(after) }

type intervalEvaluator struct {
	interval time.Duration
}

// Next 以「上一次计划时刻」为基准递推，而不是以实际执行时刻为基准，
// 否则一次执行耗时较长就会把后续计划时间不断推后。
func (e *intervalEvaluator) Next(after time.Time) time.Time { return after.Add(e.interval) }

// DueMoments 返回区间 [first, until] 内所有应当触发的计划时刻，按时间升序。
//
// first 是已经确定应当触发的时刻（来自计划的 next_run_at），因此包含在结果里——
// 调用方负责先判断它是否已到期；若 first 晚于 until，说明还没到期，返回空。
//
// 返回的第二个值表示是否因为超出上限而被截断：停机很久时每分钟的计划会积累出
// 几万个时刻，调用方必须知道结果不完整，而不是以为「就这么多」。
func DueMoments(eval Evaluator, first, until time.Time, limit int) (moments []time.Time, truncated bool) {
	if first.After(until) {
		return nil, false
	}
	if limit <= 0 {
		limit = maxScheduledMoments
	}

	moments = []time.Time{first}
	cursor := first
	for len(moments) < limit {
		next := eval.Next(cursor)
		if next.IsZero() || next.After(until) {
			return moments, false
		}
		if !next.After(cursor) {
			// 触发器没有前进说明实现有问题，直接停止，避免死循环。
			return moments, false
		}
		moments = append(moments, next)
		cursor = next
	}
	return moments, true
}

// ScheduleRun 是一次触发的记录。ScheduledFor 是「计划时刻」而不是实际执行时刻，
// 它与 ScheduleID 一起构成唯一键，用来保证同一时刻不会被触发两次。
type ScheduleRun struct {
	ID           string
	ScheduleID   string
	ScheduledFor time.Time
	StartedAt    time.Time
	FinishedAt   *time.Time
	Result       ScheduleRunResult
	OperationID  string
	ErrorCode    string
	ErrorMessage string
}

// ScheduledDispatch 描述「某个计划时刻该触发了」这件事。存储层在同一事务内决定
// 最终结果：资源空闲则记为 dispatched 并创建 Operation，资源被占则记为 failed
// 并带上 LOCK_BUSY，不会排队等待。
type ScheduledDispatch struct {
	ScheduleID   string
	ScheduledFor time.Time
	RunID        string
	Operation    *Operation
	Now          time.Time
}

// ScheduleProgress 是计划在完成一轮处理后要写回的状态。
// NextRunAt 为 nil 表示没有下一次（例如计划已被停用）。
type ScheduleProgress struct {
	NextRunAt  *time.Time
	LastRunAt  *time.Time
	LastResult string
}

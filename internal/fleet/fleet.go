// Package fleet 做一件在**客户端**发生的事：把同一个版本按批次推到多台主机上。
//
// 它刻意不持有任何状态。每一台目标机上的 opsd 是它自己那次部署的唯一事实来源
// （锁、Operation 状态机、审计、日志都在那儿）；批次只是一个**视图**。因此这里既没有
// 数据库也没有「批次表」——批次号就是每台机上的幂等键，「继续」= 用同一个批次号重跑，
// 「重来」= 换一个批次号。客户端退出不会丢任何东西。
//
// 与 5a 的 host list --check 同一条纪律：不制造第二份会过期的真相。
package fleet

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// Action 是这个批次要做的动作。
type Action string

const (
	// ActionDeploy 部署一个新版本。
	ActionDeploy Action = "deploy"
	// ActionRollback 回滚到某个既有版本。它与部署共用同一套批次、准备、汇总逻辑——
	// 换的只是「打哪个端点」与准备阶段查什么。
	ActionRollback Action = "rollback"
)

// HostClient 是 fleet 从一台主机上需要的东西。
//
// 它是一组方法而不是具体的客户端类型：批次逻辑要在没有真实 opsd 的情况下被测到，
// 而「一台机的客户端」这个概念在这里只有这几个动作。
type HostClient interface {
	Identity(ctx context.Context) (*v1.IdentityResponse, error)
	GetApplication(ctx context.Context, ref string) (*v1.Application, error)
	GetArtifact(ctx context.Context, ref string) (*v1.Artifact, error)
	UploadArtifact(ctx context.Context, path, createdBy string) (*v1.Artifact, bool, error)
	ListReleases(ctx context.Context, app string, limit int) (*v1.ReleaseListResponse, error)
	Deploy(ctx context.Context, app, manifest, idempotencyKey, createdBy string) (*v1.DeployResponse, error)
	Rollback(ctx context.Context, app, to, idempotencyKey, createdBy string) (*v1.DeployResponse, error)
	GetOperation(ctx context.Context, id string) (*v1.Operation, error)
}

// Target 是一台目标主机。Address 为空表示本机——与 5a 的 host 记录同一条规则。
type Target struct {
	Name    string
	Address string
	Client  HostClient
}

// Status 是一台机器在这个批次里的结局。四种取值**互斥且穷尽**，因此
// 「成功 + 失败 + 跳过 + 未执行 = 总数」这条恒等式总能成立——一台都不许消失。
type Status string

const (
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	// StatusSkipped 是**准备阶段就没过**（不可达、没登记应用、缺制品）。它只在
	// --allow-partial 下出现——默认情况下准备阶段不过就一台都不动。
	StatusSkipped Status = "skipped"
	// StatusNotRun 是**轮不到它**：前面某一波失败了，批次停下。它与 skipped 分开，
	// 因为「这台机有问题」与「还没轮到它」要修的是两件事。
	StatusNotRun Status = "not-run"
)

// HostResult 是一台机器在这个批次里的结果。
//
// 字段名是 `--json` 的一部分，因此用与 api/v1 一致的 camelCase（见 AGENTS.md 的语言
// 约定：`--json` 的字段名保持英文且不翻译）。
type HostResult struct {
	Host    string `json:"host"`
	Address string `json:"address"`
	Status  Status `json:"status"`

	// Noop 表示这台机上**本来就已经是这个版本**，什么都没做（部署与回滚都可能这样）。
	Noop bool `json:"noop,omitempty"`
	// OperationID 是那台机上的 Operation；Noop 时为空。
	OperationID string `json:"operationId,omitempty"`
	Version     string `json:"version,omitempty"`
	Slot        string `json:"slot,omitempty"`
	// ErrorCode / ErrorMessage 是该机上失败的原因。跳过与未执行时，Detail 里
	// 放着「为什么它没被跑到」。
	ErrorCode    string `json:"errorCode,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

// Request 是一次批量发布。
type Request struct {
	Action      Action
	Application string
	// Manifest 是 deploy 用的 manifest **原文**。它必须按 artifact.digest 引用制品
	// （调用方负责校验）：`art_xxx` 只在某一台机上有效，跨主机唯一成立的是内容摘要。
	Manifest string
	// Version 是 rollback 的目标版本，空表示「上一个曾经激活过的版本」。
	Version string
	// Digest 是 manifest 里那个制品摘要，准备阶段用它问每台机「制品在不在」。
	Digest string
	// ArtifactPath 是本地制品文件。给了它，准备阶段会把缺这个制品的机器补齐。
	ArtifactPath string

	Targets []Target
	// BatchID 是这个批次的名字，也是每台机上的幂等键的一部分。
	BatchID string

	BatchSize   int
	Concurrency int
	// Pause 是一波结束后、下一波开始前的等待。**最后一波之后不等待。**
	Pause time.Duration
	// AllowPartial 允许在准备阶段没通过时继续（把那几台标成 skipped）。
	AllowPartial bool
	// KeepGoing 允许某一波失败之后继续下一波。默认失败即停。
	KeepGoing bool

	CreatedBy string

	// Logf 是进度输出；为 nil 时安静运行。
	Logf func(format string, args ...any)

	// PollInterval / PollTimeout 控制等一台机的部署跑完。零值走默认。
	PollInterval time.Duration
	PollTimeout  time.Duration

	// Upload 在准备阶段把本地制品送到缺它的机器上。为 nil 时用 Target.Client 的
	// UploadArtifact；测试用它替代真实文件传输。
	Upload func(ctx context.Context, target Target, digest string) error
}

const (
	defaultPollInterval = 500 * time.Millisecond
	defaultPollTimeout  = 15 * time.Minute
)

// Report 是一个批次的完整结果。它把**每一台**目标机都算进去。
//
// 四个计数是**算好之后写进去**的，不是让调用方自己数：这份报告的意义就是「每一台都有
// 结局」，那就该由产生它的人给出计数，而不是留给每个消费方各数一遍。
type Report struct {
	Action      Action       `json:"action"`
	Application string       `json:"application"`
	BatchID     string       `json:"batchId"`
	Results     []HostResult `json:"results"`
	// Duration 不直接进 JSON：time.Duration 会marshal 成纳秒整数，而人要看的是毫秒。
	Duration   time.Duration `json:"-"`
	DurationMS int64         `json:"durationMs"`
	// StoppedEarly 表示批次因为某一波失败而停下（后面那些是 not-run）。
	StoppedEarly bool `json:"stoppedEarly,omitempty"`

	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
	NotRun    int `json:"notRun"`
}

func (r *Report) count(status Status) int {
	n := 0
	for i := range r.Results {
		if r.Results[i].Status == status {
			n++
		}
	}
	return n
}

// 下面四个是给调用方用的读法。它们与报告里那四个计数同源（都从 Results 数出来），
// 因此不会出现「字段说 3、结果里有 2」这种不一致。
func (r *Report) SucceededCount() int { return r.count(StatusSucceeded) }
func (r *Report) FailedCount() int    { return r.count(StatusFailed) }
func (r *Report) SkippedCount() int   { return r.count(StatusSkipped) }
func (r *Report) NotRunCount() int    { return r.count(StatusNotRun) }

// Summary 是给人看的汇总行。
func (r *Report) Summary() string {
	return fmt.Sprintf("%d 台成功，%d 台失败，%d 台跳过（准备阶段没过），%d 台未执行（共 %d 台）",
		r.count(StatusSucceeded), r.count(StatusFailed), r.count(StatusSkipped), r.count(StatusNotRun),
		len(r.Results))
}

// Run 执行一个批次。
//
// 返回值：报告与一个错误。错误的两种形态对应两种完全不同的处境——
//
//   - BATCH_PREFLIGHT_FAILED：准备阶段没过，**一台都没动**（要改的是输入）；
//   - BATCH_FAILED：批次跑完了但有主机没成功，**线上处于混合版本**（要去看现场）。
//
// 两种情况下 Report 都有完整内容：哪一台、什么状态、为什么。
func Run(ctx context.Context, req Request) (*Report, error) {
	report := &Report{Action: req.Action, Application: req.Application, BatchID: req.BatchID}
	if err := validate(&req); err != nil {
		return report, err
	}

	started := time.Now()
	// 收口只在一处：无论从哪条路返回（拒绝开始、中途停下、正常跑完），报告里的计数与
	// 用时都必须已经填好——「有的路径忘了填」会让一份看起来完整的报告少算一台。
	defer func() {
		report.Duration = time.Since(started)
		report.DurationMS = report.Duration.Milliseconds()
		report.Succeeded = report.count(StatusSucceeded)
		report.Failed = report.count(StatusFailed)
		report.Skipped = report.count(StatusSkipped)
		report.NotRun = report.count(StatusNotRun)
	}()

	results := make([]HostResult, len(req.Targets))
	for i, target := range req.Targets {
		results[i] = HostResult{Host: target.Name, Address: target.Address}
	}

	// 阶段一：全员准备。任何部署之前完成，因为「前两台部署完了，第三台才发现制品
	// 没推」是一台已经切了流、另一台还没开始的混合状态——而它本来可以提前发现。
	req.logf("准备阶段：%d 台目标主机", len(req.Targets))
	prepareAll(ctx, &req, results)

	blocked := 0
	for i := range results {
		if results[i].Status == StatusSkipped {
			blocked++
			req.logf("  跳过 %s：%s", results[i].Host, results[i].Detail)
		}
	}
	if blocked > 0 && !req.AllowPartial {
		// 拒绝开始时，**其余每一台也要有明确的结局**。留着空状态会让汇总里的四个计数
		// 加起来不等于总数，而「有一台既不在成功里、也不在失败里、也不在跳过里」正是
		// 这份报告最不该出现的东西。
		for i := range results {
			if results[i].Status == "" {
				results[i].Status = StatusNotRun
				results[i].Detail = "准备阶段没通过，批次没有开始"
			}
		}
		report.Results = results
		return report, domain.NewError(v1.CodeBatchPreflightFailed,
			"准备阶段有 %d 台主机没过（%s），因此**一台都没有动**：修好它们，或加 --allow-partial 只发其余的主机",
			blocked, firstSkipReason(results))
	}

	// 阶段二：按波推进。
	pending := make([]int, 0, len(results))
	for i := range results {
		if results[i].Status == "" {
			pending = append(pending, i)
		}
	}

	for wave := 0; len(pending) > 0; wave++ {
		size := req.BatchSize
		if size > len(pending) {
			size = len(pending)
		}
		current, rest := pending[:size], pending[size:]
		req.logf("第 %d 波：%s", wave+1, hostNames(results, current))

		runWave(ctx, &req, results, current)
		pending = rest

		failedInWave := 0
		for _, index := range current {
			if results[index].Status == StatusFailed {
				failedInWave++
			}
		}
		if failedInWave > 0 && !req.KeepGoing && len(pending) > 0 {
			report.StoppedEarly = true
			reason := firstFailure(results, current)
			req.logf("第 %d 波有 %d 台失败（%s），批次停下；剩下 %d 台未执行。",
				wave+1, failedInWave, reason, len(pending))
			req.logf("（已经切过流的机器不会自动回退——跨主机事务回滚按规格不做；" +
				"要逐台重试请对这些机器跑 operation retry，要放行剩下的机器请加 --keep-going）")
			for _, index := range pending {
				results[index].Status = StatusNotRun
				results[index].Detail = "批次在前一波失败后停下"
			}
			pending = nil
			break
		}
		if len(pending) > 0 && req.Pause > 0 {
			req.logf("停顿 %s 后进入下一波", req.Pause)
			select {
			case <-ctx.Done():
				for _, index := range pending {
					results[index].Status = StatusNotRun
					results[index].Detail = "批次被取消"
				}
				report.Results = results
				return report, ctx.Err()
			case <-time.After(req.Pause):
			}
		}
	}

	report.Results = results
	// 用 count 而不是导出字段：导出字段是在 defer 里填的，而 defer 要等 return 之后
	// 才跑。用导出字段会让这条判断永远看到 0——一个「失败却报成功」的 bug，而且只在
	// 真的失败时才显形。
	if report.count(StatusFailed) > 0 {
		return report, domain.NewError(v1.CodeBatchFailed,
			"批次 %s 没有全部成功：%s", req.BatchID, report.Summary())
	}
	return report, nil
}

func validate(req *Request) error {
	if req.Action != ActionDeploy && req.Action != ActionRollback {
		return domain.NewError(v1.CodeInvalidRequest, "批次动作只能是 deploy 或 rollback，得到 %q", req.Action)
	}
	if strings.TrimSpace(req.Application) == "" {
		return domain.NewError(v1.CodeInvalidRequest, "应用名不能为空")
	}
	if len(req.Targets) == 0 {
		return domain.NewError(v1.CodeInvalidRequest, "批次至少要有一台目标主机")
	}
	if req.BatchID == "" {
		return domain.NewError(v1.CodeInternal, "批次号不能为空")
	}
	if req.Action == ActionDeploy && req.Digest == "" {
		// 部署的准备阶段要按摘要问每台机「制品在不在」。没有摘要就没法回答这个问题，
		// 而放过它等于让准备阶段变成一句空话。
		return domain.NewError(v1.CodeInternal,
			"部署批次必须带上制品摘要：manifest 要按 artifact.digest 引用制品")
	}
	if req.BatchSize <= 0 {
		req.BatchSize = len(req.Targets)
	}
	if req.Concurrency <= 0 || req.Concurrency > req.BatchSize {
		req.Concurrency = req.BatchSize
	}
	if req.PollInterval <= 0 {
		req.PollInterval = defaultPollInterval
	}
	if req.PollTimeout <= 0 {
		req.PollTimeout = defaultPollTimeout
	}
	return nil
}

// idempotencyKey 把「这个批次的这个动作的這個应用」拼成每台机上的幂等键。
//
// 带上动作与应用：同一个批次号下发两个不同的应用时，幂等键必须不同——否则第二台机
// 上的第二次提交会撞上「同一个键、不同的请求」而被拒（IDEMPOTENCY_CONFLICT），
// 而那个报错完全指不出真正的原因。
func (r *Request) idempotencyKey() string {
	return r.BatchID + ":" + string(r.Action) + ":" + r.Application
}

func (r *Request) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func hostNames(results []HostResult, indexes []int) string {
	names := make([]string, 0, len(indexes))
	for _, index := range indexes {
		names = append(names, results[index].Host)
	}
	return strings.Join(names, " ")
}

func firstSkipReason(results []HostResult) string {
	for i := range results {
		if results[i].Status == StatusSkipped {
			return fmt.Sprintf("%s：%s", results[i].Host, results[i].Detail)
		}
	}
	return "原因未知"
}

func firstFailure(results []HostResult, indexes []int) string {
	for _, index := range indexes {
		if results[index].Status == StatusFailed {
			return fmt.Sprintf("%s：%s", results[index].Host, results[index].ErrorMessage)
		}
	}
	return "原因未知"
}

// forEachConcurrent 以不超过 limit 的并发跑 indexes 里的每一项。
//
// 结果按**目标顺序**写回 results（每项写自己的下标，因此不需要锁）：并发是为了快，
// 但输出必须稳定——按完成顺序打印会让同一份配置每次跑出来的报告都不同。
func forEachConcurrent(ctx context.Context, limit int, indexes []int, fn func(ctx context.Context, index int)) {
	if limit < 1 {
		limit = 1
	}
	semaphore := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, index := range indexes {
		wg.Add(1)
		semaphore <- struct{}{}
		go func(index int) {
			defer wg.Done()
			defer func() { <-semaphore }()
			fn(ctx, index)
		}(index)
	}
	wg.Wait()
}

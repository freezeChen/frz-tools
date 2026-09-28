package fleet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// fakeHost 是一台假主机。批次逻辑要在没有真实 opsd 的情况下被完整测到：它真正的
// 逻辑全在「谁先谁后、失败之后怎么办、汇总算不算得平」上，而那些与网络无关。
type fakeHost struct {
	name string

	identityErr  error
	appErr       error
	artifactErr  error
	uploadErr    error
	deployErr    error
	rollbackErr  error
	releaseError error

	// 部署提交成功之后那台机上的走向。
	noop             bool
	operationStatus  string
	operationErrCode string
	operationErrMsg  string
	// operationNeverEnds 模拟「部署还在跑」：操作永远不是终态。
	operationNeverEnds bool

	mu             sync.Mutex
	deployCalls    int
	rollbackCalls  int
	uploadCalls    int
	lastKeys       []string
	releases       []string
	concurrent     int
	maxConcurrent  int
	operationReady bool
}

func (h *fakeHost) tick() {
	h.mu.Lock()
	h.concurrent++
	if h.concurrent > h.maxConcurrent {
		h.maxConcurrent = h.concurrent
	}
	h.mu.Unlock()
}

func (h *fakeHost) done() {
	h.mu.Lock()
	h.concurrent--
	h.mu.Unlock()
}

func (h *fakeHost) Identity(context.Context) (*v1.IdentityResponse, error) {
	if h.identityErr != nil {
		return nil, h.identityErr
	}
	return &v1.IdentityResponse{Hostname: h.name, Backend: "tls"}, nil
}

func (h *fakeHost) GetApplication(context.Context, string) (*v1.Application, error) {
	if h.appErr != nil {
		return nil, h.appErr
	}
	return &v1.Application{ID: "app_" + h.name, Name: "orders-api"}, nil
}

func (h *fakeHost) GetArtifact(_ context.Context, ref string) (*v1.Artifact, error) {
	if h.artifactErr != nil {
		return nil, h.artifactErr
	}
	return &v1.Artifact{ID: "art_" + h.name, Digest: ref}, nil
}

func (h *fakeHost) UploadArtifact(context.Context, string, string) (*v1.Artifact, bool, error) {
	h.mu.Lock()
	h.uploadCalls++
	h.mu.Unlock()
	if h.uploadErr != nil {
		return nil, false, h.uploadErr
	}
	return &v1.Artifact{ID: "art_" + h.name}, true, nil
}

func (h *fakeHost) ListReleases(context.Context, string, int) (*v1.ReleaseListResponse, error) {
	if h.releaseError != nil {
		return nil, h.releaseError
	}
	items := make([]v1.Release, 0, len(h.releases))
	for _, version := range h.releases {
		items = append(items, v1.Release{Version: version})
	}
	return &v1.ReleaseListResponse{Items: items}, nil
}

func (h *fakeHost) Deploy(_ context.Context, _, _, idempotencyKey, _ string) (*v1.DeployResponse, error) {
	h.mu.Lock()
	h.deployCalls++
	h.lastKeys = append(h.lastKeys, idempotencyKey)
	h.mu.Unlock()
	h.tick()
	defer h.done()

	if h.deployErr != nil {
		return nil, h.deployErr
	}
	return h.response(), nil
}

func (h *fakeHost) Rollback(_ context.Context, _, _, idempotencyKey, _ string) (*v1.DeployResponse, error) {
	h.mu.Lock()
	h.rollbackCalls++
	h.lastKeys = append(h.lastKeys, idempotencyKey)
	h.mu.Unlock()
	h.tick()
	defer h.done()

	if h.rollbackErr != nil {
		return nil, h.rollbackErr
	}
	return h.response(), nil
}

func (h *fakeHost) response() *v1.DeployResponse {
	response := &v1.DeployResponse{
		Release: v1.Release{ID: "rel_" + h.name, Version: "2.0.0", Slot: "green"},
		Slot:    "green",
	}
	if h.noop {
		response.Noop = true
		return response
	}
	response.Operation = &v1.Operation{ID: "op_" + h.name, Kind: v1.KindAppDeploy, Status: "running"}
	return response
}

func (h *fakeHost) GetOperation(context.Context, string) (*v1.Operation, error) {
	if h.operationNeverEnds {
		return &v1.Operation{ID: "op_" + h.name, Status: "running"}, nil
	}
	status := h.operationStatus
	if status == "" {
		status = string(domain.StatusSucceeded)
	}
	return &v1.Operation{
		ID:           "op_" + h.name,
		Status:       status,
		ErrorCode:    h.operationErrCode,
		ErrorMessage: h.operationErrMsg,
	}, nil
}

func targets(hosts ...*fakeHost) []Target {
	out := make([]Target, 0, len(hosts))
	for _, host := range hosts {
		out = append(out, Target{Name: host.name, Address: host.name + ":9443", Client: host})
	}
	return out
}

func baseRequest(hosts ...*fakeHost) Request {
	return Request{
		Action:       ActionDeploy,
		Application:  "orders-api",
		Manifest:     "kind: ApplicationSpec",
		Digest:       "sha256:abc",
		Targets:      targets(hosts...),
		BatchID:      "batch_test",
		BatchSize:    10,
		Concurrency:  10,
		PollInterval: time.Millisecond,
		PollTimeout:  2 * time.Second,
	}
}

// 汇总必须把**每一台**都算进去。这条恒等式是整个报告的地基：一台机器既不在成功里、
// 也不在失败里、也不在跳过里，就意味着它的状态谁也不知道。
func assertCountsAddUp(t *testing.T, report *Report) {
	t.Helper()
	sum := report.SucceededCount() + report.FailedCount() + report.SkippedCount() + report.NotRunCount()
	if sum != len(report.Results) {
		t.Fatalf("四个计数加起来不等于总数：%d != %d（%s）", sum, len(report.Results), report.Summary())
	}
	for i := range report.Results {
		if report.Results[i].Status == "" {
			t.Fatalf("第 %d 台没有结局：%+v", i, report.Results[i])
		}
	}
}

func TestPrepareFailureRefusesToStartAnything(t *testing.T) {
	down := &fakeHost{name: "web-1", identityErr: domain.NewError(v1.CodeHostUnreachable, "连不上")}
	ok1 := &fakeHost{name: "web-2"}
	ok2 := &fakeHost{name: "web-3"}

	report, err := Run(context.Background(), baseRequest(down, ok1, ok2))

	if domain.CodeOf(err) != v1.CodeBatchPreflightFailed {
		t.Fatalf("want BATCH_PREFLIGHT_FAILED, got %v", err)
	}
	assertCountsAddUp(t, report)
	if report.SkippedCount() != 1 || report.NotRunCount() != 2 {
		t.Fatalf("应当 1 台跳过、2 台未执行，得到 %s", report.Summary())
	}
	// 一台都没动：这是「准备阶段拒绝开始」的全部意义。
	for _, host := range []*fakeHost{ok1, ok2} {
		if host.deployCalls != 0 {
			t.Fatalf("%s 不该被部署过（部署了 %d 次）", host.name, host.deployCalls)
		}
	}
}

func TestAllowPartialDeploysTheRest(t *testing.T) {
	down := &fakeHost{name: "web-1", identityErr: domain.NewError(v1.CodeHostUnreachable, "连不上")}
	ok1 := &fakeHost{name: "web-2"}
	ok2 := &fakeHost{name: "web-3"}

	req := baseRequest(down, ok1, ok2)
	req.AllowPartial = true

	report, err := Run(context.Background(), req)

	if err != nil {
		t.Fatalf("有 --allow-partial 时不该失败：%v", err)
	}
	assertCountsAddUp(t, report)
	if report.SkippedCount() != 1 || report.SucceededCount() != 2 {
		t.Fatalf("应当 1 台跳过、2 台成功，得到 %s", report.Summary())
	}
	if ok1.deployCalls != 1 || ok2.deployCalls != 1 {
		t.Fatal("可达的两台都该被部署")
	}
}

func TestEachHostGetsItsOwnVerdict(t *testing.T) {
	good := &fakeHost{name: "web-1"}
	bad := &fakeHost{name: "web-2", operationStatus: string(domain.StatusFailed),
		operationErrCode: string(v1.CodeDeployRolledBack), operationErrMsg: "切流失败并已撤销"}

	report, err := Run(context.Background(), baseRequest(good, bad))

	if domain.CodeOf(err) != v1.CodeBatchFailed {
		t.Fatalf("want BATCH_FAILED, got %v", err)
	}
	assertCountsAddUp(t, report)
	if report.SucceededCount() != 1 || report.FailedCount() != 1 {
		t.Fatalf("应当一成功一失败，得到 %s", report.Summary())
	}
	if report.Results[1].ErrorCode != string(v1.CodeDeployRolledBack) {
		t.Fatalf("失败那台的原因要照搬那台机上的报错：%+v", report.Results[1])
	}
	if report.Results[0].OperationID != "op_web-1" {
		t.Fatalf("成功那台要带上操作 id：%+v", report.Results[0])
	}
}

// 失败即停是默认：下一波不开始，剩下的是 not-run 而**不是**从报告里消失。
func TestFailFastStopsTheNextWave(t *testing.T) {
	first := &fakeHost{name: "web-1", operationStatus: string(domain.StatusFailed), operationErrMsg: "炸了"}
	second := &fakeHost{name: "web-2"}
	third := &fakeHost{name: "web-3"}

	req := baseRequest(first, second, third)
	req.BatchSize = 1
	req.Concurrency = 1

	report, err := Run(context.Background(), req)

	if domain.CodeOf(err) != v1.CodeBatchFailed {
		t.Fatalf("want BATCH_FAILED, got %v", err)
	}
	assertCountsAddUp(t, report)
	if !report.StoppedEarly {
		t.Fatal("应当记下「批次提前停下」")
	}
	if report.NotRunCount() != 2 {
		t.Fatalf("剩下两台应当是 not-run，得到 %s", report.Summary())
	}
	if second.deployCalls != 0 || third.deployCalls != 0 {
		t.Fatal("失败之后的机器不该被部署")
	}
}

func TestKeepGoingContinuesAfterFailure(t *testing.T) {
	first := &fakeHost{name: "web-1", operationStatus: string(domain.StatusFailed), operationErrMsg: "炸了"}
	second := &fakeHost{name: "web-2"}

	req := baseRequest(first, second)
	req.BatchSize = 1
	req.Concurrency = 1
	req.KeepGoing = true

	report, err := Run(context.Background(), req)

	if domain.CodeOf(err) != v1.CodeBatchFailed {
		t.Fatalf("want BATCH_FAILED, got %v", err)
	}
	assertCountsAddUp(t, report)
	if second.deployCalls != 1 {
		t.Fatal("--keep-going 时后面的机器要继续")
	}
	if report.SucceededCount() != 1 || report.FailedCount() != 1 {
		t.Fatalf("得到 %s", report.Summary())
	}
}

// 批次号就是幂等键：同名重跑拼出来的键必须一模一样，否则「继续」会变成「重做」。
func TestBatchIdIsTheIdempotencyKey(t *testing.T) {
	first := &fakeHost{name: "web-1"}

	req := baseRequest(first)
	if _, err := Run(context.Background(), req); err != nil {
		t.Fatalf("第一次：%v", err)
	}
	if _, err := Run(context.Background(), req); err != nil {
		t.Fatalf("第二次：%v", err)
	}

	if len(first.lastKeys) != 2 {
		t.Fatalf("应当提交过两次，得到 %d", len(first.lastKeys))
	}
	if first.lastKeys[0] != first.lastKeys[1] {
		t.Fatalf("同一个批次的键必须一致：%q vs %q", first.lastKeys[0], first.lastKeys[1])
	}
	if !strings.Contains(first.lastKeys[0], req.BatchID) {
		t.Fatalf("键里要能看出批次号：%q", first.lastKeys[0])
	}
}

// 同一个批次号下发两个不同的应用，键必须不同——否则第二台机上会撞上
// 「同一个键、不同的请求」，而那个报错完全指不出真正的原因。
func TestDifferentApplicationsGetDifferentKeys(t *testing.T) {
	first := &fakeHost{name: "web-1"}

	req := baseRequest(first)
	if _, err := Run(context.Background(), req); err != nil {
		t.Fatalf("%v", err)
	}
	req.Application = "billing-api"
	if _, err := Run(context.Background(), req); err != nil {
		t.Fatalf("%v", err)
	}

	if first.lastKeys[0] == first.lastKeys[1] {
		t.Fatalf("不同应用的键不该相同：%q", first.lastKeys[0])
	}
}

func TestPrepareUploadsMissingArtifact(t *testing.T) {
	needsUpload := &fakeHost{name: "web-1", artifactErr: domain.NewError(v1.CodeArtifactNotFound, "没有")}
	hasIt := &fakeHost{name: "web-2"}

	req := baseRequest(needsUpload, hasIt)
	req.ArtifactPath = "/tmp/app.tar.gz"

	report, err := Run(context.Background(), req)
	if err != nil {
		t.Fatalf("%v", err)
	}
	assertCountsAddUp(t, report)
	if needsUpload.uploadCalls != 1 {
		t.Fatalf("缺制品的机器要被补齐，得到 %d 次上传", needsUpload.uploadCalls)
	}
	if hasIt.uploadCalls != 0 {
		t.Fatal("已经有这个制品的机器不该再传一遍")
	}
}

// 缺制品、又没给本地文件：准备阶段就该拦住，而不是让那台机上的部署去失败。
func TestMissingArtifactWithoutLocalFileIsAPreflightFailure(t *testing.T) {
	host := &fakeHost{name: "web-1", artifactErr: domain.NewError(v1.CodeArtifactNotFound, "没有")}

	report, err := Run(context.Background(), baseRequest(host))

	if domain.CodeOf(err) != v1.CodeBatchPreflightFailed {
		t.Fatalf("want BATCH_PREFLIGHT_FAILED, got %v", err)
	}
	assertCountsAddUp(t, report)
	if host.deployCalls != 0 {
		t.Fatal("准备阶段没过就不该部署")
	}
}

// 「问不出来」与「没有这个制品」必须分开：把前者当成缺制品，会去上传一份本来就在的
// 东西，而真正的问题（网络、认证）被掩盖。
func TestArtifactLookupErrorIsNotTreatedAsMissing(t *testing.T) {
	host := &fakeHost{name: "web-1", artifactErr: domain.NewError(v1.CodeHostTLSFailed, "证书不对")}

	req := baseRequest(host)
	req.ArtifactPath = "/tmp/app.tar.gz"

	_, err := Run(context.Background(), req)

	if domain.CodeOf(err) != v1.CodeBatchPreflightFailed {
		t.Fatalf("want BATCH_PREFLIGHT_FAILED, got %v", err)
	}
	if host.uploadCalls != 0 {
		t.Fatal("查不到制品的原因不是「缺制品」时，不该去上传")
	}
}

func TestRollbackPreflightChecksVersionOnlyWhenGiven(t *testing.T) {
	host := &fakeHost{name: "web-1", releases: []string{"1.0.0"}}

	req := baseRequest(host)
	req.Action = ActionRollback
	req.Digest = ""
	req.Version = "9.9.9"

	report, err := Run(context.Background(), req)
	if domain.CodeOf(err) != v1.CodeBatchPreflightFailed {
		t.Fatalf("这台机上没有 9.9.9，准备阶段该拦住：%v", err)
	}
	assertCountsAddUp(t, report)
	if host.rollbackCalls != 0 {
		t.Fatal("准备阶段没过就不该回滚")
	}

	// 不给版本（回到「上一个曾经激活过的版本」）时不查——那是那台机自己的定义。
	req.Version = ""
	report, err = Run(context.Background(), req)
	if err != nil {
		t.Fatalf("不给版本时不该在准备阶段失败：%v", err)
	}
	if host.rollbackCalls != 1 {
		t.Fatalf("应当回滚一次，得到 %d", host.rollbackCalls)
	}
	assertCountsAddUp(t, report)
}

// 等不到结果**不等于**部署失败。这条断言钉住那句话：它必须说清「我只知道我没等到」。
func TestPollTimeoutSaysItDidNotWaitRatherThanFailed(t *testing.T) {
	host := &fakeHost{name: "web-1", operationNeverEnds: true}

	req := baseRequest(host)
	req.PollInterval = time.Millisecond
	req.PollTimeout = 20 * time.Millisecond

	report, err := Run(context.Background(), req)

	if domain.CodeOf(err) != v1.CodeBatchFailed {
		t.Fatalf("want BATCH_FAILED, got %v", err)
	}
	assertCountsAddUp(t, report)
	result := report.Results[0]
	if result.OperationID != "op_web-1" {
		t.Fatalf("要留下操作 id，好让运维去确认：%+v", result)
	}
	if !strings.Contains(result.ErrorMessage, "我没等到结果") {
		t.Fatalf("这条失败必须说清它只说明「没等到」：%s", result.ErrorMessage)
	}
}

func TestNoopCountsAsSucceeded(t *testing.T) {
	host := &fakeHost{name: "web-1", noop: true}

	report, err := Run(context.Background(), baseRequest(host))
	if err != nil {
		t.Fatalf("%v", err)
	}
	assertCountsAddUp(t, report)
	if !report.Results[0].Noop || report.SucceededCount() != 1 {
		t.Fatalf("已经是这个版本应当算成功：%+v", report.Results[0])
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	hosts := []*fakeHost{}
	for _, name := range []string{"web-1", "web-2", "web-3", "web-4"} {
		hosts = append(hosts, &fakeHost{name: name})
	}

	req := baseRequest(hosts...)
	req.BatchSize = 4
	req.Concurrency = 2

	if _, err := Run(context.Background(), req); err != nil {
		t.Fatalf("%v", err)
	}
	for _, host := range hosts {
		if host.maxConcurrent > 2 {
			t.Fatalf("%s 上同时跑了 %d 个，超过了并发上限 2", host.name, host.maxConcurrent)
		}
	}
}

func TestValidationRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Request)
	}{
		{"没有目标", func(r *Request) { r.Targets = nil }},
		{"应用名为空", func(r *Request) { r.Application = "  " }},
		{"动作不认识", func(r *Request) { r.Action = "promote" }},
		{"部署没给摘要", func(r *Request) { r.Digest = "" }},
		{"批次号为空", func(r *Request) { r.BatchID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := baseRequest(&fakeHost{name: "web-1"})
			tc.mutate(&req)
			if _, err := Run(context.Background(), req); domain.CodeOf(err) != v1.CodeInvalidRequest &&
				domain.CodeOf(err) != v1.CodeInternal {
				t.Fatalf("want INVALID_REQUEST 或 INTERNAL，得到 %v", err)
			}
		})
	}
}

// 停顿只发生在**波与波之间**：一波发完（哪怕是全部一起发）之后不该再等，
// 多等一次没有意义，只会让脚本白慢。（波与波之间确实要停的那一半由
// TestPauseHappensBetweenWaves 钉住。）
func TestNoPauseAfterTheLastWave(t *testing.T) {
	first := &fakeHost{name: "web-1"}
	second := &fakeHost{name: "web-2"}

	req := baseRequest(first, second)
	req.BatchSize = 2 // 只有一波
	req.Concurrency = 2
	req.Pause = 5 * time.Second

	started := time.Now()
	if _, err := Run(context.Background(), req); err != nil {
		t.Fatalf("%v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("只有一波时不该停顿，实际用了 %s", elapsed)
	}
}

func TestPauseHappensBetweenWaves(t *testing.T) {
	first := &fakeHost{name: "web-1"}
	second := &fakeHost{name: "web-2"}

	req := baseRequest(first, second)
	req.BatchSize = 1
	req.Concurrency = 1
	req.Pause = 120 * time.Millisecond

	started := time.Now()
	if _, err := Run(context.Background(), req); err != nil {
		t.Fatalf("%v", err)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
		t.Fatalf("两波之间应当停顿，实际只用了 %s", elapsed)
	}
}

func TestContextCancellationIsNotSilentlySwallowed(t *testing.T) {
	host := &fakeHost{name: "web-1", operationNeverEnds: true}

	req := baseRequest(host)
	req.PollInterval = 5 * time.Millisecond
	req.PollTimeout = time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	report, err := Run(ctx, req)
	if err == nil {
		t.Fatal("被取消时不该报成功")
	}
	assertCountsAddUp(t, report)
	if !errors.Is(err, context.DeadlineExceeded) && domain.CodeOf(err) != v1.CodeBatchFailed {
		t.Fatalf("得到 %v", err)
	}
}

package fleet

import (
	"context"
	"fmt"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// runWave 推进一波：波内最多 Concurrency 台同时跑。
//
// 波内并发、波与波串行——两个旋钮回答两个不同的问题。「一次放几台」（并发）与
// 「放完这批要不要停下来看一眼」（批次 + 停顿）在真实发布里是两件事：前者关乎压力，
// 后者关乎风险。
func runWave(ctx context.Context, req *Request, results []HostResult, indexes []int) {
	forEachConcurrent(ctx, req.Concurrency, indexes, func(ctx context.Context, index int) {
		target := req.Targets[index]
		results[index] = runOne(ctx, req, target, results[index])
	})
}

func runOne(ctx context.Context, req *Request, target Target, result HostResult) HostResult {
	response, err := submit(ctx, req, target)
	if err != nil {
		result.Status = StatusFailed
		result.ErrorCode = string(domain.CodeOf(err))
		result.ErrorMessage = domain.MessageOf(err)
		req.logf("  失败 %s：%s", target.Name, result.ErrorMessage)
		return result
	}

	result.Version = response.Release.Version
	result.Slot = response.Slot
	if result.Slot == "" {
		result.Slot = response.Release.Slot
	}

	// Noop：这台机上本来就已经是这个版本。它不是错误，也不产生 Operation——
	// 重复提交一次部署就该是这个结果。
	if response.Noop || response.Operation == nil {
		result.Status = StatusSucceeded
		result.Noop = true
		result.Detail = "本来就是 " + result.Version + "，没有动作"
		req.logf("  已是最新 %s（%s）", target.Name, result.Version)
		return result
	}

	result.OperationID = response.Operation.ID
	operation, waitErr := waitForOperation(ctx, req, target, response.Operation.ID)
	if waitErr != nil {
		// 等不到结果**不等于**部署失败：那台机上的操作可能还在跑。因此这句话必须说清
		// 楚「我只知道我没等到」，并给出到哪儿去确认真相。
		result.Status = StatusFailed
		result.ErrorCode = string(v1.CodeBatchFailed)
		result.ErrorMessage = fmt.Sprintf(
			"没有等到操作结束（%v）：那台机上的部署可能还在跑，请到那台机上查 operation get %s ——这条失败只说我没等到结果",
			waitErr, response.Operation.ID)
		req.logf("  未等到 %s：%s", target.Name, result.ErrorMessage)
		return result
	}

	if operation.Status != string(domain.StatusSucceeded) {
		result.Status = StatusFailed
		result.ErrorCode = operation.ErrorCode
		result.ErrorMessage = operation.ErrorMessage
		if result.ErrorMessage == "" {
			result.ErrorMessage = "操作终态是 " + operation.Status
		}
		req.logf("  失败 %s：%s（%s）", target.Name, result.ErrorCode, result.ErrorMessage)
		return result
	}

	result.Status = StatusSucceeded
	req.logf("  成功 %s：%s（%s）", target.Name, result.Version, slotLabel(result.Slot))
	return result
}

func submit(ctx context.Context, req *Request, target Target) (*v1.DeployResponse, error) {
	key := req.idempotencyKey()
	if req.Action == ActionRollback {
		return target.Client.Rollback(ctx, req.Application, req.Version, key, req.CreatedBy)
	}
	return target.Client.Deploy(ctx, req.Application, req.Manifest, key, req.CreatedBy)
}

// waitForOperation 轮询一台机上的操作，直到它到终态。
//
// 查询失败**不当成失败**：网络抖一下就把一台已经部署成功的机器报成失败，会让运维
// 去查一台其实好好的机器，而下一次抖动又会换个机器报。因此查询出错只记下来，
// 继续重试到超时为止——超时那条路会明确说「我没等到」，而不是「它失败了」。
func waitForOperation(ctx context.Context, req *Request, target Target, id string) (*v1.Operation, error) {
	deadline := time.Now().Add(req.PollTimeout)
	var lastErr error

	for {
		operation, err := target.Client.GetOperation(ctx, id)
		if err != nil {
			lastErr = err
		} else {
			if terminal(operation.Status) {
				return operation, nil
			}
			lastErr = nil
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				return nil, fmt.Errorf("超过 %s，且最后一次查询失败：%v", req.PollTimeout, lastErr)
			}
			return nil, fmt.Errorf("超过 %s", req.PollTimeout)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(req.PollInterval):
		}
	}
}

// terminal 判定操作是否已经到终态。pending 与 running 之外都是终态——这与
// domain 的状态机一致，但它在这里只用来决定「还要不要等」，不参与任何判断。
func terminal(status string) bool {
	switch domain.Status(status) {
	case domain.StatusSucceeded, domain.StatusFailed, domain.StatusCancelled:
		return true
	}
	return false
}

func slotLabel(slot string) string {
	if slot == "" {
		return "单槽"
	}
	return "槽位 " + slot
}

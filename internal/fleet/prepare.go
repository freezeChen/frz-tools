package fleet

import (
	"context"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// prepareAll 在**任何一次部署之前**逐台确认三件事，并把没过的那几台标成 skipped。
//
// 三件事：能不能连上、这台机上有没有这个应用、以及（部署时）制品在不在。
// 为什么要提前：不然会出现「前两台部署完了，第三台才发现制品没推」——一台已经切了流、
// 另一台还没开始的混合状态，而它本来可以在动手之前就发现。这与 NGINX_CONFIG_INVALID
// （拒绝切流）和 DEPLOY_ROLLED_BACK（动过并撤销）是同一种区分：**能不做就不做**。
//
// 准备阶段可能有副作用：给了本地制品文件时，缺制品的机器会被补齐。这是刻意的——
// 制品上传是内容寻址的幂等动作，而它必须在任何一次部署之前完成。
func prepareAll(ctx context.Context, req *Request, results []HostResult) {
	indexes := make([]int, 0, len(req.Targets))
	for i := range req.Targets {
		results[i].Status = ""
		indexes = append(indexes, i)
	}

	forEachConcurrent(ctx, req.Concurrency, indexes, func(ctx context.Context, index int) {
		target := req.Targets[index]
		if err := prepareOne(ctx, req, target); err != nil {
			results[index].Status = StatusSkipped
			results[index].Detail = domain.MessageOf(err)
		} else {
			results[index].Detail = ""
		}
	})
}

func prepareOne(ctx context.Context, req *Request, target Target) error {
	// 一、连得上吗。用 /identity 而不是 /health：health 会去 ping 那台机的数据库，
	// 而这里要问的是「我能不能跟它说上话」——库有问题会在下一步以更准确的样子报出来。
	if _, err := target.Client.Identity(ctx); err != nil {
		return err
	}

	// 二、这台机上登记了这个应用吗。没登记时部署会失败，但失败在那台机上看起来是
	// 一次「应用不存在」的操作——把它提到准备阶段，运维一次就能看全哪些机器缺什么。
	if _, err := target.Client.GetApplication(ctx, req.Application); err != nil {
		return domain.NewError(domain.CodeOf(err),
			"这台机上没有应用 %q：%s", req.Application, domain.MessageOf(err))
	}

	switch req.Action {
	case ActionDeploy:
		return ensureArtifact(ctx, req, target)
	case ActionRollback:
		return ensureRollbackTarget(ctx, req, target)
	}
	return nil
}

// ensureArtifact 确认制品在这台机上；不在时补齐（如果调用方给了本地文件）。
func ensureArtifact(ctx context.Context, req *Request, target Target) error {
	if _, err := target.Client.GetArtifact(ctx, req.Digest); err == nil {
		return nil
	} else if domain.CodeOf(err) != v1.CodeArtifactNotFound {
		// 区分「没有这个制品」与「问不出来」：后者（网络、认证）不能当成缺制品，
		// 否则我们会去上传一份本来就在的东西，而真正的问题被掩盖。
		return err
	}

	switch {
	case req.Upload != nil:
		return req.Upload(ctx, target, req.Digest)
	case req.ArtifactPath != "":
		_, _, err := target.Client.UploadArtifact(ctx, req.ArtifactPath, req.CreatedBy)
		return err
	default:
		return domain.NewError(v1.CodeArtifactNotFound,
			"这台机上没有制品 %s，而这次调用没有给出可用来补齐的本地制品文件", req.Digest)
	}
}

// ensureRollbackTarget 确认要回滚到的那个版本在这台机上存在。
//
// 只在**显式给了版本**时查：版本为空表示「上一个曾经激活过的版本」，那是那台机自己
// 的定义，在这里推一遍等于把它的规则抄第二份——而两份规则迟早会不一致。
func ensureRollbackTarget(ctx context.Context, req *Request, target Target) error {
	if req.Version == "" {
		return nil
	}
	releases, err := target.Client.ListReleases(ctx, req.Application, 200)
	if err != nil {
		return err
	}
	for i := range releases.Items {
		if releases.Items[i].Version == req.Version {
			return nil
		}
	}
	return domain.NewError(v1.CodeReleaseNotFound,
		"这台机上没有版本 %s 的记录，回滚会在那台机上失败", req.Version)
}

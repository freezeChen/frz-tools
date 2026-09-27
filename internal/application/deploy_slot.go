package application

import (
	"context"
	"fmt"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// 迭代 4 的蓝绿流程：**让某个 release 在某个槽位上接流量**。
//
// 它与单槽那条路（deploy.go 的后半段）的差别正是蓝绿的全部意义：
//
//	单槽：停旧的 → 切指针 → 起新的   （发布期间有一段服务是停着的）
//	蓝绿：起新的 → 就绪 → **切流** → 观察 → 排空后停旧的   （全程有服务在接流量）
//
// 两条路共用同一套东西：同一个 ReleaseAdapter（按槽位切换指针）、同一个 RuntimeAdapter
// （按槽位 Prepare/Start/Stop/Health）、同一套失败语义（改动过线上就撤销，没动过就报原因码）。
// 差别只在顺序与「谁在接流量」这一件事上。

// nextSlot 算出「下一次部署会落到哪一侧」。
//
// 判据只有一条：**总是另一侧**。第一次部署没有 serving 槽位，用 blue——这个选择是刻意的
// （谁先谁后不重要，重要的是它有确定的答案，而不是看哪个槽位恰好有目录）。
//
// 它被两处用到：创建期算给调用方看的「目标槽位」（`DeployResponse.Slot`），
// 执行期算真正要用的那一侧。**必须是同一个函数**——两处各写一遍，
// 「响应里说 green、实际切到 blue」就从不可能变成了可能。
func nextSlot(serving domain.Slot) domain.Slot {
	if serving.Valid() {
		return serving.Opposite()
	}
	return domain.SlotBlue
}

// applySlot 是蓝绿的部署与回滚。
//
// 目标槽位由「现在谁在接流量」推出来：**总是切到另一侧**。第一次部署没有 serving 槽位，
// 用 blue——这个选择刻意的（谁先谁后不重要，重要的是它有确定的答案，而不是看哪个槽位
// 恰好有目录）。
func (s *DeployService) applySlot(
	ctx context.Context, release *domain.Release, spec *domain.ApplicationSpec, rollback bool,
	operationID string, logf DeployLogf,
) error {
	if s.nginx == nil {
		return domain.NewError(v1.CodeRuntimeUnsupport,
			"应用 %s 是蓝绿形态（exec.slots），但本部署未装配 Nginx 适配器：没有对外入口就没法切流",
			spec.Application)
	}
	// Nginx 只在这一条路上用得到，因此这台主机有没有 nginx 只在这条路上检查。
	// 放在最前：新槽位起来之后才发现 nginx 不可用，等于白折腾一趟。
	if err := s.nginx.Validate(ctx, spec); err != nil {
		return err
	}

	app, err := s.repo.GetApplication(ctx, release.ApplicationID)
	if err != nil {
		return err
	}
	serving, err := s.repo.ServingSlot(ctx, app.ID)
	if err != nil {
		return err
	}
	target := nextSlot(serving)
	// 记下这次部署落在哪一侧：`releases.slot` 是「这个版本是怎么上的」的一部分，
	// 排查与审计都要靠它回答「这一版当初上到了哪一侧」。
	if err := s.repo.SetReleaseSlot(ctx, release.ID, target); err != nil {
		return err
	}

	previous, err := s.repo.ActiveRelease(ctx, app.ID)
	if err != nil {
		return err
	}
	var previousSpec *domain.ApplicationSpec
	if previous != nil {
		if previousSpec, err = s.repo.GetApplicationSpecForRelease(ctx, previous.ApplicationID, previous.ID); err != nil {
			return err
		}
	}

	sub := string(target)
	action := "部署"
	if rollback {
		action = "回滚"
	}
	logf("info", domain.PhaseValidate, action+"开始（蓝绿）", map[string]string{
		"releaseId": release.ID, "version": release.Version,
		"slot": sub, "servingSlot": string(serving), "previousVersion": versionOf(previous),
	})

	// 失败收尾的三态（迭代 4 规格 D5，沿用 3c 定下的规则）：
	//
	//	流量从未动过  → 报**原因码**，并说明「流量未受影响」（顺手把新槽位收干净）
	//	流量动过、撤销成功 → DEPLOY_ROLLED_BACK
	//	流量动过、撤销失败 → 原因码 +「需要人工介入」
	var (
		started  bool // 目标槽位的进程已经起来
		switched bool // 流量已经切到目标槽位
	)
	// observedSamples 是观察窗口已经采样的次数。失败发生在窗口里时，这个数字要进时间线
	// ——「看了十几眼才发现它退化」与「刚切过去就发现」是两件不同的事。
	observedSamples := 0
	fail := func(cause error) error {
		var rollbackErr error
		undone := false
		switch {
		case switched && previous != nil:
			// 流量已经切过去了：把 upstream 切回来，并把旧槽位重新拉起来（正常情况下它
			// 还在跑——我们只在发布成功之后才停它，而失败恰好意味着没走到那一步）。
			undone = true
			rollbackErr = s.switchTraffic(ctx, app.ID, previousSpec, serving, previous, target, operationID, logf)
		case switched:
			// 第一次部署就切过去了再失败：没有旧版本可以回去。只能把这一侧停掉、撤掉
			// 对外入口——**这会儿这个应用没有任何版本在服务**，报错必须这么说。
			undone = true
			rollbackErr = s.teardownSlot(ctx, spec, target, logf)
		case started:
			// 还没切流就失败：停掉新槽位（清理，不是回滚）。流量一点没动。
			if cleanupErr := s.teardownSlot(ctx, spec, target, logf); cleanupErr != nil {
				logf("warn", domain.PhaseRollback, "停掉未接流量的新槽位失败（不影响线上）",
					map[string]string{"slot": sub, "error": domain.MessageOf(cleanupErr)})
			}
		}

		// 这一侧被记成 failed：运维要能一眼看出「上一次往这一侧推失败了」，
		// 而不是从「没有 serving 行」去推断。
		if slotErr := s.repo.PutApplicationSlot(ctx, app.ID, target, release.ID,
			domain.SlotFailed, nil, s.now()); slotErr != nil {
			s.logger.Warn("记录失败槽位状态失败", "slot", sub, "error", slotErr)
		}
		// 时间线里也留一条。**槽位状态会被对账按事实重算，历史不会**——这正是
		// 「上一次在这边推失败了」该待的地方（见 SlotService.Reconcile 的注释）。
		appendSlotEvent(ctx, s.repo, s.logger, app.ID, target, domain.SlotEventFailed,
			slotFailureDetail(cause, observedSamples), release, &operationID, s.now())
		if spec.Materializes() {
			// 本次新建的目录：撤销之后它没有任何用处（内容可以从制品重新解出来）。
			if err := s.releases.Remove(ctx, spec, release.ID); err != nil {
				s.logger.Warn("清理失败版本的目录失败", "releaseId", release.ID, "error", err)
			}
		}
		if markErr := s.repo.FailRelease(ctx, release.ID,
			string(domain.CodeOf(cause)), domain.MessageOf(cause), s.now()); markErr != nil {
			s.logger.Error("记录失败状态失败", "releaseId", release.ID, "error", markErr)
		}

		switch {
		case rollbackErr != nil:
			logf("error", domain.PhaseFinalize, "切回旧槽位失败，当前的服务状态不确定",
				map[string]string{"error": domain.MessageOf(rollbackErr)})
			return domain.NewError(domain.CodeOf(cause),
				"%s；**切回旧槽位也失败了**（%s），当前的服务状态不确定，需要人工介入",
				domain.MessageOf(cause), domain.MessageOf(rollbackErr))
		case undone:
			logf("warn", domain.PhaseFinalize, action+"失败，已切回旧槽位", nil)
			return domain.NewError(v1.CodeDeployRolledBack, "%s", domain.MessageOf(cause))
		case domain.CodeOf(cause) == v1.CodeNginxReloadFailed:
			// **切流本身出了事**：upstream 已被改过、reload 又没成功。这一层推不出流量
			// 现在在哪一侧（Nginx 那边可能已经加载了新配置，也可能没有），因此不能像
			// 别的失败那样说「流量未受影响」——那句话会在最需要准确的时候失真。
			logf("error", domain.PhaseFinalize, action+"失败于切流环节，服务状态不确定",
				map[string]string{"error": domain.MessageOf(cause)})
			return domain.NewError(domain.CodeOf(cause),
				"%s；**服务状态不确定**（切流环节失败），需要人工确认流量在哪一侧", domain.MessageOf(cause))
		default:
			logf("error", domain.PhaseFinalize, action+"失败，线上流量未受影响",
				map[string]string{"error": domain.MessageOf(cause)})
			return domain.NewError(domain.CodeOf(cause),
				"%s；**线上流量未受影响**（仍由槽位 %s 服务）", domain.MessageOf(cause), serving)
		}
	}

	// 1) 起新槽位：Prepare 写这一侧自己的 unit 与环境，再把它指到目标 release 并启动。
	//    **旧槽位一直没动**——它还在接流量，直到我们切过去并等它排空。
	if err := s.runtime.Prepare(ctx, spec, target); err != nil {
		return fail(err)
	}
	if spec.Materializes() {
		if err := s.materializeIfMissing(ctx, spec, release.ID, release.ArtifactID, logf); err != nil {
			return fail(err)
		}
		if err := s.releases.Activate(ctx, spec, target, release.ID); err != nil {
			return fail(err)
		}
	}
	if err := s.runtime.Start(ctx, spec, target); err != nil {
		return fail(err)
	}
	started = true
	if err := s.waitForReady(ctx, spec, target, logf); err != nil {
		return fail(err)
	}

	// 2) 切流：改 upstream + `nginx -t` + reload。这一步之前流量一点没动。
	if err := s.nginx.Apply(ctx, spec, target); err != nil {
		return fail(err)
	}
	switched = true
	logf("info", domain.PhaseExecute, "流量已切到新槽位",
		map[string]string{"slot": sub, "version": release.Version})
	appendSlotEvent(ctx, s.repo, s.logger, app.ID, target, domain.SlotEventSwitched,
		"流量切到这一侧（"+action+"）", release, &operationID, s.now())

	// 3) 观察窗口：切流之后盯着新槽位，退化了就按失败处理（切回去）。
	samples, err := s.observeSlot(ctx, spec, target, logf)
	observedSamples = samples
	if err != nil {
		return fail(err)
	}
	if spec.Nginx.ObservationSeconds > 0 {
		appendSlotEvent(ctx, s.repo, s.logger, app.ID, target, domain.SlotEventObserved,
			fmt.Sprintf("观察窗口通过：%d 次采样，窗口 %d 秒", samples, spec.Nginx.ObservationSeconds),
			release, &operationID, s.now())
	}

	// 4) promote：先落 serving_slot（Nginx 那边第 2 步已经改完了——**线上事实在前**，
	//    库里那份是它的镜像；反过来写会在中间崩溃时留下「库说 A、流量在 B」）。
	superseded, err := s.repo.ActivateRelease(ctx, release.ID, s.now())
	if err != nil {
		return fail(err)
	}
	now := s.now()
	if err := s.repo.SetServingSlot(ctx, app.ID, target, now); err != nil {
		return fail(err)
	}
	if err := s.repo.PutApplicationSlot(ctx, app.ID, target, release.ID, domain.SlotServing, &now, now); err != nil {
		return fail(err)
	}
	if serving.Valid() {
		// 旧槽位进入「排空」：它还在跑（在途请求要它处理完），但已经不再接新流量。
		if err := s.repo.PutApplicationSlot(ctx, app.ID, serving, releaseIDOf(previous), domain.SlotDraining, nil, now); err != nil {
			return fail(err)
		}
	}
	logf("info", domain.PhaseFinalize, action+"完成（已切流）", map[string]string{
		"releaseId": release.ID, "version": release.Version,
		"slot": sub, "superseded": itoa(superseded),
	})

	// 5) 排空旧槽位之后停掉它。**只停进程，不删目录**——回滚就是把它重新拉起来。
	if serving.Valid() && previousSpec != nil {
		s.drainAndStop(ctx, previousSpec, serving, previous, app.ID, operationID, logf)
	}

	s.pruneReleases(ctx, spec, release.ID, logf)
	return nil
}

// switchTraffic 把流量切回旧槽位，并确保它的进程在跑。
//
// 用**旧版本的规格**：切回去要连配置一起切回去（listen、serverName、槽位端口都取自那一版），
// 与 3b 的「回滚连配置一起回滚」是同一条要求。
func (s *DeployService) switchTraffic(
	ctx context.Context, applicationID string, previousSpec *domain.ApplicationSpec, serving domain.Slot,
	previous *domain.Release, failedSlot domain.Slot, operationID string, logf DeployLogf,
) error {
	if !serving.Valid() || previousSpec == nil {
		return domain.NewError(v1.CodeInternal, "没有可切回的槽位")
	}
	logf("warn", domain.PhaseRollback, "把流量切回旧槽位",
		map[string]string{"slot": string(serving), "version": versionOf(previous)})
	if err := s.nginx.Apply(ctx, previousSpec, serving); err != nil {
		return err
	}
	// 旧槽位在失败路径上通常还活着（我们只在发布成功后才停它）。这里仍然显式 Start 一次：
	// 它是幂等的，而「切回去了但那一侧的进程已经不在」是最不该发生的事。
	if err := s.runtime.Start(ctx, previousSpec, serving); err != nil {
		return err
	}
	if err := s.waitForReady(ctx, previousSpec, serving, logf); err != nil {
		return err
	}
	// 切回来也是一次真实的切流，时间线必须记上——否则 `app slot history` 会读成
	// 「流量一直在 blue」，而中间那一段其实跑在 green 上。
	appendSlotEvent(ctx, s.repo, s.logger, applicationID, serving, domain.SlotEventSwitched,
		"流量切回这一侧（槽位 "+string(failedSlot)+" 上的这次尝试失败）", previous, &operationID, s.now())
	return nil
}

// teardownSlot 把新槽位收干净：停进程、撤指针。用在「流量从未动过」与「第一次部署切过去
// 之后又失败」两种收尾里。
func (s *DeployService) teardownSlot(ctx context.Context, spec *domain.ApplicationSpec, slot domain.Slot, logf DeployLogf) error {
	logf("warn", domain.PhaseRollback, "停下新槽位并撤掉它的指针", map[string]string{"slot": string(slot)})
	stopErr := s.runtime.Stop(ctx, spec, slot)
	deactivateErr := s.releases.Deactivate(ctx, spec, slot)
	if stopErr != nil {
		return stopErr
	}
	return deactivateErr
}

// drainAndStop 等旧槽位把在途请求处理完，再停掉它。
//
// 「停不掉」不算发布失败：流量已经在新槽位上了，旧进程只是多占一点资源。因此这里只记日志，
// 让发布结果保持成功——把一次成功的发布报成失败，比多留一个空闲进程糟得多。
func (s *DeployService) drainAndStop(
	ctx context.Context, previousSpec *domain.ApplicationSpec, serving domain.Slot,
	previous *domain.Release, applicationID, operationID string, logf DeployLogf,
) {
	drain := previousSpec.Nginx.DrainSeconds
	if drain > 0 {
		logf("info", domain.PhaseExecute, "等待旧槽位排空",
			map[string]string{"slot": string(serving), "seconds": itoa(drain)})
		if err := sleepContext(ctx, time.Duration(drain)*time.Second); err != nil {
			// 被取消时也仍然要尝试停掉它：留下一个「已经不该接流量」的进程在跑，
			// 比中途停下这次收尾更糟。
			logf("warn", domain.PhaseExecute, "等待排空时被取消，直接停旧槽位", nil)
		}
	}
	if err := s.runtime.Stop(ctx, previousSpec, serving); err != nil {
		logf("warn", domain.PhaseFinalize, "停掉旧槽位失败（不影响发布结果，它会继续空闲运行）",
			map[string]string{"slot": string(serving), "error": domain.MessageOf(err)})
		return
	}
	if err := s.repo.PutApplicationSlot(ctx, applicationID, serving, releaseIDOf(previous),
		domain.SlotStopped, nil, s.now()); err != nil {
		s.logger.Warn("记录旧槽位状态失败", "slot", string(serving), "error", err)
	}
	// 只有走到了这里（进程真的停了）才落事件：Stop 失败时那一侧还在跑，
	// 时间线上写「已停止」就是一句假话。
	appendSlotEvent(ctx, s.repo, s.logger, applicationID, serving, domain.SlotEventStopped,
		"排空后停掉（它不再接流量）", previous, &operationID, s.now())
}

// observeSlot 是切流之后的观察窗口（迭代 4 规格 D4 第 6 步、D11 第 3 条）。
//
// 它只采样**确定性的事实**：新槽位是不是还就绪。刻意不看错误率——那要读 Nginx 日志、
// 要定阈值，而阈值定错的自动回滚比不自动回滚更危险（用户 2026-09-25 拍板：本迭代不做
// 自动回滚，只做「窗口内发现退化就按失败处理」）。
//
// 窗口长度 0 表示不观察；默认 30 秒（domain.DefaultObservationSeconds）。
func (s *DeployService) observeSlot(ctx context.Context, spec *domain.ApplicationSpec, slot domain.Slot, logf DeployLogf) (int, error) {
	seconds := spec.Nginx.ObservationSeconds
	if seconds <= 0 {
		return 0, nil
	}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	logf("info", domain.PhaseExecute, "观察窗口开始", map[string]string{
		"slot": string(slot), "seconds": itoa(seconds),
	})
	// samples 数的是**拿到结果的探测**次数（探测自己报错的那次不算——那说明我们没看见
	// 那一侧的状况，而不是看见它不健康）。它最终要进时间线：窗口结束时记「看了几眼」，
	// 窗口内退化时记「看了几眼才发现」。
	samples := 0
	for {
		health, err := s.runtime.Health(ctx, spec, slot)
		if err != nil {
			return samples, err
		}
		samples++
		if !health.Ready {
			return samples, domain.NewError(v1.CodeRuntimeNotReady,
				"观察窗口内槽位 %s 不再就绪（已采样 %d 次；%s）", slot, samples, health.Detail)
		}
		if !time.Now().Before(deadline) {
			logf("info", domain.PhaseExecute, "观察窗口通过", map[string]string{
				"slot": string(slot), "samples": itoa(samples)})
			return samples, nil
		}
		select {
		case <-ctx.Done():
			return samples, domain.NewError(v1.CodeExecCancelled, "观察窗口被取消")
		case <-time.After(readyPollInterval):
		}
	}
}

// slotFailureDetail 把一次失败写成时间线上的一行说明。
//
// 错误码在最前面：时间线是**扫**着看的，而「是配置问题还是起不来」这个区分
// 比后面那串自然语言更先被需要。
func slotFailureDetail(cause error, samples int) string {
	detail := fmt.Sprintf("%s: %s", domain.CodeOf(cause), domain.MessageOf(cause))
	if samples > 0 {
		detail += fmt.Sprintf("（观察窗口里已采样 %d 次）", samples)
	}
	return detail
}

// sleepContext 是可取消的等待。取消时立刻返回，让「排空」这一步不至于把取消拖住。
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func versionOf(release *domain.Release) string {
	if release == nil {
		return ""
	}
	return release.Version
}

func releaseIDOf(release *domain.Release) string {
	if release == nil {
		return ""
	}
	return release.ID
}

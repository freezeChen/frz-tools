package application

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/idgen"
)

// DeployLogf 与 BackupLogf 同形：由 worker 提供，因为「写进哪条 Operation」是任务引擎的事。
type DeployLogf func(level, phase, message string, fields map[string]string)

// DeployService 编排一次部署或回滚（迭代 3 规格 D6）。
//
// 它与 RuntimeService 的关系是**上下层**而不是并列：部署会用 RuntimeAdapter 把进程管起来，
// 而 RuntimeService 管的是「已经部署好的应用怎么启停」。因此部署不重写 1c 的那套
// Prepare/Start/就绪检查，只是把它们按正确的顺序串起来，并在失败时把状态收回去。
type DeployService struct {
	repo      Repository
	artifacts *ArtifactService
	releases  ReleaseAdapter
	runtime   RuntimeAdapter

	newID  func(prefix string) string
	now    func() time.Time
	logger *slog.Logger
}

func newDeployService(
	repo Repository,
	artifacts *ArtifactService,
	releases ReleaseAdapter,
	runtime RuntimeAdapter,
	newID func(prefix string) string,
	now func() time.Time,
	logger *slog.Logger,
) *DeployService {
	if newID == nil {
		newID = idgen.New
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &DeployService{
		repo: repo, artifacts: artifacts, releases: releases, runtime: runtime,
		newID: newID, now: now, logger: logger,
	}
}

// Configured 报告部署能力是否可用。没装 ReleaseAdapter 时部署端点必须明确拒绝，
// 而不是让每一次部署都跑到解包那一步才失败。
func (s *DeployService) Configured() bool {
	return s != nil && s.releases != nil && s.artifacts != nil && s.runtime != nil
}

// DeployTarget 是一次部署请求在**创建期**解析出来的结果。
type DeployTarget struct {
	Release *domain.Release
	// AlreadyActive 表示这个版本**已经是当前激活的版本**：部署是幂等的，不产生任何 Operation。
	// 重复提交一次部署不该有任何副作用，而"重新物化一遍"既做不到（目录已存在会被拒）
	// 也没有意义。
	AlreadyActive bool
}

// PrepareDeploy 在创建 Operation **之前**完成所有能提前做的事（规格 D6 第 1 步）。
//
// 提前做的意义是反馈速度：让「制品不存在」「版本号已被别的制品占用」「应用名对不上」
// 这类错误在提交时就报出来，而不是先建一条注定失败的 Operation 再让它失败。
func (s *DeployService) PrepareDeploy(
	ctx context.Context, appRef string, spec *domain.ApplicationSpec, updatedBy string,
) (*DeployTarget, error) {
	if !s.Configured() {
		return nil, domain.NewError(v1.CodeRuntimeUnsupport, "本部署未启用应用部署能力")
	}
	if spec == nil {
		return nil, domain.NewError(v1.CodeManifestInvalid, "部署需要一份应用规格")
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	app, err := s.repo.GetApplication(ctx, appRef)
	if err != nil {
		return nil, err
	}
	if spec.Application != app.Name {
		return nil, domain.NewError(v1.CodeManifestInvalid,
			"manifest 里的 application 是 %q，与目标应用 %q 不一致", spec.Application, app.Name)
	}

	artifact, err := s.repo.GetArtifact(ctx, artifactRef(spec.Artifact))
	if err != nil {
		return nil, err
	}
	// manifest 声明了摘要就必须与实际一致：制品不可变，摘要不符说明声明的和要跑的不是一回事。
	if spec.Artifact.Digest != "" && artifact.Digest != domain.Digest(spec.Artifact.Digest) {
		return nil, domain.NewError(v1.CodeArtifactChecksum,
			"manifest 声明的摘要 %s 与制品 %s 的实际摘要 %s 不一致",
			spec.Artifact.Digest, artifact.ID, artifact.Digest)
	}

	version := spec.Artifact.VersionOrDerived()
	existing, err := s.repo.GetReleaseByVersion(ctx, app.ID, version)
	if err != nil {
		return nil, err
	}

	release := existing
	switch {
	case existing == nil:
		release, err = s.repo.CreateRelease(ctx, &domain.Release{
			ID:            s.newID("rel"),
			ApplicationID: app.ID,
			ArtifactID:    artifact.ID,
			Version:       version,
			CreatedAt:     s.now(),
			CreatedBy:     updatedBy,
			Status:        domain.ReleaseCreated,
		})
		if err != nil {
			return nil, err
		}
	case existing.ArtifactID != artifact.ID:
		// 同一个版本号下换内容是最难查的一类漂移：跑的是哪个版本的制品，谁也说不清。
		return nil, domain.NewError(v1.CodeReleaseConflict,
			"版本 %q 已经对应制品 %s，不能改成 %s（制品不可变，请换一个版本号）",
			version, existing.ArtifactID, artifact.ID)
	}

	// 把这一版采用的规格写下来（1c 决定 3）：回滚要连配置一起回滚。
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, domain.NewError(v1.CodeInternal, "无法序列化应用规格: %v", err)
	}
	if err := s.repo.PutApplicationSpecForRelease(ctx, app.ID, release.ID, encoded, s.now(), updatedBy); err != nil {
		return nil, err
	}

	return &DeployTarget{Release: release, AlreadyActive: release.Status == domain.ReleaseActive}, nil
}

// RollbackTarget 是一次回滚请求在创建期解析出来的结果。
type RollbackTarget struct {
	Release *domain.Release
	From    *domain.Release
}

// PrepareRollback 选出回滚的目标版本。
//
// **在创建期选**：让「没有可回滚的版本」这件事立刻报出来，而不是先建一条注定失败的
// Operation。目标可以是显式指定的版本，也可以是「上一个曾经激活过的版本」。
func (s *DeployService) PrepareRollback(ctx context.Context, appRef, toVersion string) (*RollbackTarget, error) {
	if !s.Configured() {
		return nil, domain.NewError(v1.CodeRuntimeUnsupport, "本部署未启用应用部署能力")
	}
	app, err := s.repo.GetApplication(ctx, appRef)
	if err != nil {
		return nil, err
	}
	current, err := s.repo.ActiveRelease(ctx, app.ID)
	if err != nil {
		return nil, err
	}

	var target *domain.Release
	if toVersion != "" {
		target, err = s.repo.GetReleaseByVersion(ctx, app.ID, toVersion)
		if err != nil {
			return nil, err
		}
		if target == nil {
			return nil, domain.NewError(v1.CodeReleaseNotFound, "应用 %s 没有版本 %q", app.Name, toVersion)
		}
	} else {
		target, err = s.repo.PreviousRelease(ctx, app.ID)
		if err != nil {
			return nil, err
		}
		if target == nil {
			return nil, domain.NewError(v1.CodeReleaseNotFound,
				"应用 %s 没有可回滚的版本（它只部署过一次）", app.Name)
		}
	}

	if !target.Status.Rollbackable() {
		// failed 从没跑成功、removed 的目录已经没了、created 还没部署过。
		return nil, domain.NewError(v1.CodeReleaseConflict,
			"版本 %s 的状态是 %s，不能回滚到它", target.Version, target.Status)
	}
	if current != nil && current.ID == target.ID {
		return nil, domain.NewError(v1.CodeReleaseConflict, "版本 %s 已经是当前版本", target.Version)
	}
	return &RollbackTarget{Release: target, From: current}, nil
}

// ResolveDeployOperation 是 Service 与 DeployService 之间的接缝，与 runtime / backup 两个
// resolver 同形：把 kind 与引用解析成规范化的 Operation 资源与要随操作存下的 spec。
func (s *DeployService) ResolveDeployOperation(ctx context.Context, kind, appRef string, raw json.RawMessage) (string, json.RawMessage, error) {
	if !s.Configured() {
		return "", nil, domain.NewError(v1.CodeRuntimeUnsupport, "本部署未启用应用部署能力")
	}
	// app.deploy / app.rollback 也一样拒绝 dryRun：适配器端口没有这个语义，接受它就会变成
	// 「以为只是预演、其实真的换了版本」——而换版本会重启正在服务的进程。
	var options struct {
		ReleaseID string `json:"releaseId"`
	}
	if len(raw) == 0 {
		return "", nil, domain.NewError(v1.CodeInvalidRequest, "%s 缺少 releaseId", kind)
	}
	if err := json.Unmarshal(raw, &options); err != nil {
		return "", nil, domain.NewError(v1.CodeInvalidRequest, "%s 的参数无法解析: %v", kind, err)
	}

	app, err := s.repo.GetApplication(ctx, appRef)
	if err != nil {
		return "", nil, err
	}
	release, err := s.repo.GetRelease(ctx, options.ReleaseID)
	if err != nil {
		return "", nil, err
	}
	if release.ApplicationID != app.ID {
		return "", nil, domain.NewError(v1.CodeReleaseNotFound,
			"版本 %s 不属于应用 %s", release.ID, app.Name)
	}
	// 资源用**应用**：同一应用的两次部署天然互斥，也与 runtime.start/stop 互斥——它们是
	// 同一个应用上的两件事，同时做会互相踩。
	return app.ID, raw, nil
}

// ---------------------------------------------------------------------------
// 执行
// ---------------------------------------------------------------------------

// ExecuteDeploy 执行一次部署（规格 D6 的 6 步）。
func (s *DeployService) ExecuteDeploy(ctx context.Context, releaseID string, logf DeployLogf) error {
	return s.apply(ctx, releaseID, false, logf)
}

// ExecuteRollback 执行一次回滚。它与部署走**同一条**应用路径：回滚就是「把某个既有版本
// 再应用一次」，差别只在目标的选择与它落在哪个状态上。
func (s *DeployService) ExecuteRollback(ctx context.Context, releaseID string, logf DeployLogf) error {
	return s.apply(ctx, releaseID, true, logf)
}

// apply 是部署与回滚的共同实现。
//
// 顺序（规格 D6）：物化 → 准备（目录、用户、unit、凭据）→ 切换 → 启动与就绪 → 记状态。
// 任何一步失败都走同一条收尾：能切回原处就切回，删掉本次新建的目录，把 release 记成 failed。
func (s *DeployService) apply(ctx context.Context, releaseID string, rollback bool, logf DeployLogf) error {
	release, err := s.repo.GetRelease(ctx, releaseID)
	if err != nil {
		return err
	}
	// 执行时读**这一版自己的**规格（而不是"当前规格"）：部署与回滚用的必须是这次要上的
	// 那个版本当时写下的配置，否则回滚只回了一半。
	spec, err := s.repo.GetApplicationSpecForRelease(ctx, release.ApplicationID, releaseID)
	if err != nil {
		return err
	}
	if err := spec.Validate(); err != nil {
		return err
	}

	previous, err := s.repo.ActiveRelease(ctx, release.ApplicationID)
	if err != nil {
		return err
	}
	// 上一个版本的规格要提前读出来：换版本时必须**先把它停掉**（见下面的注释），
	// 失败回滚时还要用它把配置一起放回去。
	var previousSpec *domain.ApplicationSpec
	if previous != nil {
		if previousSpec, err = s.repo.GetApplicationSpecForRelease(ctx, previous.ApplicationID, previous.ID); err != nil {
			return err
		}
	}
	directory := domain.ReleaseDir(spec.Application, releaseID)
	if err := s.repo.MarkReleaseDeploying(ctx, releaseID, directory, s.now()); err != nil {
		return err
	}

	action := "部署"
	if rollback {
		action = "回滚"
	}
	logf("info", domain.PhaseValidate, action+"开始", map[string]string{
		"releaseId": releaseID, "version": release.Version, "directory": directory,
	})

	// 失败收尾：把状态收回去，并且**说清楚现在跑的是哪个版本**。
	//
	// 两个标记，而不是一个：`switched` 说「current 已经切过去了」，`stoppedPrevious`
	// 说「上一个版本已经被我们停掉了」。**两者之间那段窗口是真实存在的**——停掉旧版本之后、
	// 切成新版本之前，物化或切换自己就可能失败。只看 `switched` 会把那段窗口里的失败当成
	// 「什么都没发生」，于是旧版本停在那里没人管，而 Operation 却写着「已回到上一个稳定版本」。
	var (
		switched        bool
		stoppedPrevious bool
	)
	fail := func(cause error) error {
		var rollbackErr error
		undone := false
		switch {
		case (switched || stoppedPrevious) && previous != nil:
			undone = true
			rollbackErr = s.restorePrevious(ctx, spec, previous, previousSpec, logf)
		case switched:
			// 这是这台应用上的第一次部署：没有可回退的版本，因此只能把它停下来——
			// 让一个起不来的 unit 留在那里反复重启，比停下来更难看，也更难排查。
			undone = true
			rollbackErr = s.teardownFirstDeploy(ctx, spec, releaseID, logf)
		}
		if spec.Materializes() {
			// 本次新建的目录：回滚之后它没有任何用处（内容可以从制品重新解出来）。
			if err := s.releases.Remove(ctx, spec, releaseID); err != nil {
				s.logger.Warn("清理失败版本的目录失败", "releaseId", releaseID, "error", err)
			}
		}
		if markErr := s.repo.FailRelease(ctx, releaseID,
			string(domain.CodeOf(cause)), domain.MessageOf(cause), s.now()); markErr != nil {
			s.logger.Error("记录失败状态失败", "releaseId", releaseID, "error", markErr)
		}

		if rollbackErr != nil {
			// **回滚失败比原故障更严重**：现在没有任何版本在跑。因此刻意**不**报
			// DEPLOY_ROLLED_BACK——那个码说的是「已经回到上一个稳定版本」，而这里没有。
			logf("error", domain.PhaseFinalize, "回滚失败，当前没有正在运行的版本",
				map[string]string{"error": domain.MessageOf(rollbackErr)})
			return domain.NewError(domain.CodeOf(cause),
				"%s；**回滚也失败了**（%s），当前没有正在运行的版本，需要人工介入",
				domain.MessageOf(cause), domain.MessageOf(rollbackErr))
		}
		if !undone {
			// **什么都没被动过**：失败发生在改动线上之前（典型是这个新版本的 manifest
			// 或主机事实不成立，Prepare 阶段就拒了）。
			//
			// 这时报 DEPLOY_ROLLED_BACK 是错的：那个码说的是「已经回到上一个稳定版本」，
			// 会让运维以为发生过一次回滚、进而去查「为什么回滚了」；而真相是线上从头到尾
			// 没有被碰过，该改的是那份 manifest。原因码（MANIFEST_INVALID 之类）才是他要的。
			logf("error", domain.PhaseFinalize, action+"失败，线上没有任何变化",
				map[string]string{"error": domain.MessageOf(cause)})
			return domain.NewError(domain.CodeOf(cause),
				"%s；线上没有任何变化（%s），失败发生在本版本被应用之前",
				domain.MessageOf(cause), runningDescription(previous))
		}
		logf("warn", domain.PhaseFinalize, action+"失败，已回到上一个稳定版本", nil)
		return domain.NewError(v1.CodeDeployRolledBack, "%s", domain.MessageOf(cause))
	}

	if err := s.runtime.Prepare(ctx, spec, ""); err != nil {
		return fail(err)
	}
	// **先停掉上一个版本**：`Start` 的语义是「把应用跑起来」，对已经 active 的 unit 它是
	// 幂等的 no-op——那是 runtime.start 要的性质（重复提交无害），但部署要的是**换版本**，
	// 不重启根本换不过去：旧进程会继续占着它自己的端口，而新版本的就绪检查永远等不到。
	if previousSpec != nil {
		if err := s.runtime.Stop(ctx, previousSpec, ""); err != nil {
			return fail(err)
		}
		// 从这一刻起线上是被动过的：旧版本已经停了。后面任何一步失败都必须把它放回去
		// ——包括「还没切到新版本就失败」这种（见 fail 里的注释）。
		stoppedPrevious = true
	}
	if spec.Materializes() {
		// 目录已存在就跳过物化：那是「把某个既有版本再应用一次」（回滚，或者部署一个
		// 早先的版本号）。制品不可变，重新解一遍只会撞上「目录已存在」。
		if err := s.materializeIfMissing(ctx, spec, releaseID, release.ArtifactID, logf); err != nil {
			return fail(err)
		}
		if err := s.releases.Activate(ctx, spec, releaseID); err != nil {
			return fail(err)
		}
		switched = true
	}
	if err := s.runtime.Start(ctx, spec, ""); err != nil {
		return fail(err)
	}
	// **健康通过才算发布成功**：1c 的 runtime.start 只负责把进程起来（「启动 ≠ 就绪」是它
	// 刻意的区分，就绪要单独问 health），因此这一步由部署流程自己做。
	if err := s.waitForReady(ctx, spec, logf); err != nil {
		return fail(err)
	}

	superseded, err := s.repo.ActivateRelease(ctx, releaseID, s.now())
	if err != nil {
		return fail(err)
	}
	logf("info", domain.PhaseFinalize, action+"完成", map[string]string{
		"releaseId": releaseID, "version": release.Version,
		"superseded": itoa(superseded),
	})

	s.pruneReleases(ctx, spec, releaseID, logf)
	return nil
}

// runningDescription 用一句话说清「这次失败有没有动到线上」，供失败信息使用。
//
// 措辞刻意只说**我们确实知道的事**：没有上一个版本，就是「本来就没有」；有，那就是
// 「没有被停过」——而不是断言「它此刻一定在健康服务」（那可能因为别的原因为假，
// 而失败信息里的一句想当然，比不说更糟）。
func runningDescription(previous *domain.Release) string {
	if previous == nil {
		return "这是该应用的第一次部署，本来就没有正在运行的版本"
	}
	return "上一个版本（" + previous.Version + "）没有被停过"
}

// materializeIfMissing 在 release 目录不存在时把制品解出来。
func (s *DeployService) materializeIfMissing(
	ctx context.Context, spec *domain.ApplicationSpec, releaseID, artifactID string, logf DeployLogf,
) error {
	existing, err := s.releases.List(ctx, spec)
	if err != nil {
		return err
	}
	for _, id := range existing {
		if id == releaseID {
			logf("info", domain.PhaseExecute, "release 目录已存在，跳过解包（把既有版本再应用一次）",
				map[string]string{"releaseId": releaseID})
			return nil
		}
	}

	artifact, reader, err := s.artifacts.Download(ctx, artifactID)
	if err != nil {
		return err
	}
	defer reader.Close()

	logf("info", domain.PhaseExecute, "解包制品", map[string]string{
		"releaseId": releaseID, "artifact": artifact.ID, "digest": artifact.Digest.String(),
	})
	return s.releases.Materialize(ctx, spec, releaseID, reader)
}

// restorePrevious 把上一个稳定版本放回去：先切回它的目录，再用**它自己的规格**准备与启动。
func (s *DeployService) restorePrevious(
	ctx context.Context,
	current *domain.ApplicationSpec,
	previous *domain.Release,
	previousSpec *domain.ApplicationSpec,
	logf DeployLogf,
) error {
	logf("warn", domain.PhaseRollback, "切回上一个稳定版本",
		map[string]string{"releaseId": previous.ID, "version": previous.Version})

	// 与正向部署同一条道理，方向相反：先把失败的这一版停掉，再把旧版本起来。
	// 少了这一步，`Start` 会因为 unit 已经 active 而变成 no-op——于是库里写着"已回滚"，
	// 而进程仍然是那个坏版本在跑。这是最难查的一类不一致。
	if err := s.runtime.Stop(ctx, current, ""); err != nil {
		return err
	}
	if previousSpec.Materializes() {
		if err := s.releases.Activate(ctx, previousSpec, previous.ID); err != nil {
			return err
		}
	}
	if err := s.runtime.Prepare(ctx, previousSpec, ""); err != nil {
		return err
	}
	return s.runtime.Start(ctx, previousSpec, "")
}

// teardownFirstDeploy 处理「第一次部署就失败」：停下 unit、撤掉 current 指针。
//
// 撤掉指针是必须的：不撤的话 current 会指着马上要被删掉的目录，于是「现在跑的是哪个版本」
// 这句话指向一个不存在的目录，下一次部署前的健康检查也会以一种难懂的方式失败。
func (s *DeployService) teardownFirstDeploy(
	ctx context.Context, spec *domain.ApplicationSpec, releaseID string, logf DeployLogf,
) error {
	logf("warn", domain.PhaseRollback, "这是本应用的第一次部署，没有可回退的版本，停止它", nil)
	stopErr := s.runtime.Stop(ctx, spec, "")
	deactivateErr := s.releases.Deactivate(ctx, spec)
	if stopErr != nil {
		return stopErr
	}
	return deactivateErr
}

// pruneReleases 按 release.keepLast 清理不再需要的版本目录（规格 D7）。
//
// 它的失败**不影响**部署的结果：版本已经上去了，清理失败只是少释放一点磁盘。
func (s *DeployService) pruneReleases(ctx context.Context, spec *domain.ApplicationSpec, keepLastReleaseID string, logf DeployLogf) {
	app, err := s.repo.GetApplication(ctx, spec.Application)
	if err != nil {
		s.logger.Warn("清理旧版本时找不到应用", "application", spec.Application, "error", err)
		return
	}
	releases, err := s.repo.ListReleases(ctx, app.ID, maxReleaseScan)
	if err != nil {
		s.logger.Warn("清理旧版本时读取版本列表失败", "application", app.Name, "error", err)
		return
	}

	removal := domain.PlanReleaseRemoval(releases, spec.Release.KeepLast)
	for _, id := range removal {
		// 先删目录、再标记记录（规格 D7）：库里有记录、盘上没目录，比反过来更容易让人
		// 误判——后者会让人以为某个版本还能回滚。
		if err := s.releases.Remove(ctx, spec, id); err != nil {
			s.logger.Warn("删除旧版本目录失败", "releaseId", id, "error", err)
			continue
		}
		if err := s.repo.MarkReleaseRemoved(ctx, id, s.now()); err != nil {
			s.logger.Warn("标记旧版本为已清理失败", "releaseId", id, "error", err)
		}
	}
	if len(removal) > 0 {
		logf("info", domain.PhaseFinalize, "清理了旧版本", map[string]string{
			"removed": itoa(len(removal)), "keepLast": itoa(spec.Release.KeepLast),
		})
	}
}

// artifactRef 取 manifest 里用来说明「要跑哪个制品」的那个引用（id 与 digest 二选一）。
func artifactRef(artifact domain.SpecArtifact) string {
	if artifact.ID != "" {
		return artifact.ID
	}
	return artifact.Digest
}

// maxReleaseScan 是列出版本时的上限。它不是容量设计，而是防呆：真撞到它说明该分页了。
const maxReleaseScan = 1000

// readyPollInterval 是轮询就绪的间隔。200ms 是在「快了会白烧 CPU」与「慢了会让部署的
// 反馈迟到」之间的取值；真正的预算由 manifest 的 health.startTimeoutSeconds 决定。
const readyPollInterval = 200 * time.Millisecond

// waitForReady 轮询直到应用就绪，或超过 spec 的启动超时。
//
// 「连续成功次数」不在这里数：那是适配器的状态（systemd 的实例态计数），`Health` 只在
// 达成之后才报 Ready。这里只负责「等」与「超时」。
func (s *DeployService) waitForReady(ctx context.Context, spec *domain.ApplicationSpec, logf DeployLogf) error {
	timeout := spec.Health.StartTimeout
	if timeout <= 0 {
		timeout = domain.DefaultStartTimeout
	}
	deadline := time.Now().Add(timeout)
	logf("info", domain.PhaseExecute, "等待就绪", map[string]string{"target": spec.Health.Readiness.Target, "timeout": timeout.String()})

	for {
		health, err := s.runtime.Health(ctx, spec, "")
		if err != nil {
			// 适配器自己也可能报「超时未就绪」（systemd 那边在超过 startTimeout 之后会
			// 直接给出 RUNTIME_NOT_READY），那种错误原样上抛。
			return err
		}
		if health.Ready {
			logf("info", domain.PhaseExecute, "已就绪", map[string]string{"detail": health.Detail})
			return nil
		}
		if time.Now().After(deadline) {
			return domain.NewError(v1.CodeRuntimeNotReady,
				"应用在 %s 内未就绪（目标 %s，最近一次检查：%s）", timeout, spec.Health.Readiness.Target, health.Detail)
		}
		select {
		case <-ctx.Done():
			return domain.NewError(v1.CodeExecCancelled, "等待就绪时被取消")
		case <-time.After(readyPollInterval):
		}
	}
}

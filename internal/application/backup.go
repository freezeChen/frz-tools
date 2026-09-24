package application

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/backupcodec"
	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/idgen"
)

// BackupLogf 把执行过程中的进展写进 Operation 的日志。
//
// 由 worker 提供，因为「写进哪条 Operation」是任务引擎的事，不是备份逻辑的事；
// 备份服务只负责在正确的时机说正确的话。
type BackupLogf func(level, phase, message string, fields map[string]string)

// BackupService 编排一次备份 / 校验 / 恢复。
//
// 它负责把「适配器产出的逻辑流」与「要落盘的字节」接起来：传输编码（压缩、加密）、
// 摘要与原子提交都在这里，而适配器只看到明文的逻辑流（迭代 2 规格 D1）。
type BackupService struct {
	repo     Repository
	store    StorageBackend
	secrets  SecretResolver
	adapters map[domain.BackupResourceKind]BackupAdapter

	newID  func(prefix string) string
	now    func() time.Time
	logger *slog.Logger
}

func newBackupService(
	repo Repository,
	store StorageBackend,
	secrets SecretResolver,
	adapters []BackupAdapter,
	newID func(prefix string) string,
	now func() time.Time,
	logger *slog.Logger,
) *BackupService {
	if newID == nil {
		newID = idgen.New
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	registry := make(map[domain.BackupResourceKind]BackupAdapter, len(adapters))
	for _, adapter := range adapters {
		registry[adapter.Kind()] = adapter
	}
	return &BackupService{
		repo: repo, store: store, secrets: secrets, adapters: registry,
		newID: newID, now: now, logger: logger,
	}
}

// Configured 报告备份能力是否可用。没配置存储根时备份相关端点应当明确拒绝，
// 而不是让每一次备份都跑到写盘时才失败（与制品根未配置时的做法一致）。
func (s *BackupService) Configured() bool { return s != nil && s.store != nil }

// SavePolicy 落库一份策略。它只做校验与持久化，不触发任何备份动作。
func (s *BackupService) SavePolicy(ctx context.Context, policy *domain.BackupPolicy, updatedBy string) (*domain.BackupPolicy, error) {
	if policy == nil {
		return nil, domain.NewError(v1.CodeManifestInvalid, "备份策略不能为空")
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	// 提交期就走一遍**适配器**的校验，而不只是领域模型的校验：
	// 「这个 kind 本版本有没有适配器」「这份声明与工具的实际行为一致吗」
	// （例如 postgres 的归档自带压缩、必须声明 compression: none）都是适配器才知道的事，
	// 让它们在第一次备份跑到一半时才失败，等于把一份必然失败的策略存进库里。
	adapter, err := s.adapterFor(policy.Resource.Kind)
	if err != nil {
		return nil, err
	}
	if err := adapter.Validate(ctx, policy); err != nil {
		return nil, err
	}
	saved, err := s.repo.SaveBackupPolicy(ctx, policy, s.newID("bpl"), s.now(), updatedBy)
	if err != nil {
		return nil, err
	}
	s.logger.Info("backup policy saved", "policy", saved.Name, "kind", string(saved.Resource.Kind))
	return saved, nil
}

func (s *BackupService) GetPolicy(ctx context.Context, ref string) (*domain.BackupPolicy, error) {
	return s.repo.GetBackupPolicy(ctx, ref)
}

func (s *BackupService) ListPolicies(ctx context.Context) ([]domain.BackupPolicy, error) {
	return s.repo.ListBackupPolicies(ctx)
}

func (s *BackupService) List(ctx context.Context, policyRef string, limit int) ([]domain.Backup, error) {
	return s.repo.ListBackups(ctx, policyRef, limit)
}

func (s *BackupService) Get(ctx context.Context, id string) (*domain.Backup, error) {
	return s.repo.GetBackup(ctx, id)
}

// ResolveBackupOperation 是 Service 与备份之间的接缝：把 kind 与引用解析成规范化的
// Operation 资源，并在建 Operation **之前**完成能提前做的校验。
//
// 提前校验的意义在于反馈速度：让 `backup run` 对一个不存在的策略立刻报错，
// 而不是先建一条注定失败的 Operation 再让它失败。
func (s *BackupService) ResolveBackupOperation(ctx context.Context, kind, ref string) (string, error) {
	if !s.Configured() {
		return "", domain.NewError(v1.CodeConfigInvalid,
			"本部署未配置备份存储根（backupStore.root），备份相关操作不可用")
	}

	switch kind {
	case v1.KindBackupRun, v1.KindBackupVerify, v1.KindBackupRestore:
	default:
		return "", domain.NewError(v1.CodeInvalidRequest, "不是备份类操作: %q", kind)
	}

	switch kind {
	case v1.KindBackupRun:
		policy, err := s.repo.GetBackupPolicy(ctx, ref)
		if err != nil {
			return "", err
		}
		if _, err := s.adapterFor(policy.Resource.Kind); err != nil {
			return "", err
		}
		// 资源用策略名：同一份策略的两次备份天然互斥，不需要额外机制。
		return policy.Name, nil
	default:
		// 校验与恢复的资源是**具体那份备份**，不是策略——同一个策略下可以校验
		// 或恢复任意一份历史备份，它们之间不该互相阻塞。
		backup, err := s.repo.GetBackup(ctx, ref)
		if err != nil {
			return "", err
		}
		return backup.ID, nil
	}
}

// RestoreOptions 是 backup.restore **创建时**决定的参数。
//
// 模式必须随操作一起存下来：它在提交时被确认过，执行时不该再去问一次「用哪种模式」——
// 那条路只会在执行期失败，而运维以为提交成功了。
type RestoreOptions struct {
	Mode      domain.RestoreMode `json:"mode"`
	Confirmed bool               `json:"confirmed"`
}

// RestoreOptionsJSON 把恢复参数编成要随 Operation 存下的形态。
func RestoreOptionsJSON(mode domain.RestoreMode, confirmed bool) ([]byte, error) {
	return json.Marshal(RestoreOptions{Mode: mode, Confirmed: confirmed})
}

// DecodeRestoreOptions 从 Operation 的 spec 里读回恢复参数。
func DecodeRestoreOptions(raw []byte) (RestoreOptions, error) {
	var options RestoreOptions
	if len(raw) == 0 {
		return options, domain.NewError(v1.CodeInvalidRequest, "backup.restore 缺少恢复参数")
	}
	if err := json.Unmarshal(raw, &options); err != nil {
		return options, domain.NewError(v1.CodeInvalidRequest, "恢复参数无法解析: %v", err)
	}
	if !options.Mode.Valid() {
		return options, domain.NewError(v1.CodeInvalidRequest, "恢复模式取值非法: %q", options.Mode)
	}
	return options, nil
}

// RestoreTarget 是恢复请求的解析结果，供 Service.Create 拿到「用哪种模式恢复」。
type RestoreTarget struct {
	Backup *domain.Backup
	Policy *domain.BackupPolicy
	Mode   domain.RestoreMode
}

// PrepareRestore 校验一次恢复请求：备份存在、策略还在、模式合法，且原地恢复
// 必须**显式确认**。
//
// 确认在**创建期**就检查，而不是等执行时再失败：原地恢复会覆盖真实数据，
// 让它在排队之后才报错，等于让运维以为提交成功了。
func (s *BackupService) PrepareRestore(ctx context.Context, backupID string, mode domain.RestoreMode, confirmed bool) (*RestoreTarget, error) {
	if !mode.Valid() {
		return nil, domain.NewError(v1.CodeInvalidRequest, "恢复模式取值非法: %q", mode)
	}
	backup, err := s.repo.GetBackup(ctx, backupID)
	if err != nil {
		return nil, err
	}
	policy, err := s.repo.GetBackupPolicy(ctx, backup.PolicyID)
	if err != nil {
		return nil, err
	}
	if mode == domain.RestoreInPlace && !confirmed {
		return nil, domain.NewError(v1.CodeBackupRestoreUnconfirmed,
			"原地恢复会覆盖 %s 上的真实数据，必须显式确认（--confirm）", policy.Resource.Paths)
	}
	if backup.Status != domain.BackupSucceeded {
		return nil, domain.NewError(v1.CodeBackupNotFound,
			"备份 %s 的状态是 %s，只有 succeeded 的备份才能恢复", backup.ID, backup.Status)
	}
	return &RestoreTarget{Backup: backup, Policy: policy, Mode: mode}, nil
}

func (s *BackupService) adapterFor(kind domain.BackupResourceKind) (BackupAdapter, error) {
	if adapter, ok := s.adapters[kind]; ok {
		return adapter, nil
	}
	// 用 MANIFEST_INVALID 而不是新造一个码：这是「这份 manifest 声明的资源种类在本
	// 版本没有对应适配器」，属于对提交内容的判断。
	return nil, domain.NewError(v1.CodeManifestInvalid,
		"resource.kind=%s 在本版本没有对应的备份适配器", kind)
}

// cleanupAdapter 释放适配器在本次操作里留下的临时资源（临时目录、隔离恢复的临时库）。
//
// 它**不是**保留策略：「哪些备份该删」由 GFS 决定，是通用逻辑，属于应用层（规格 D2）。
//
// 两处细节是有意的：
//   - 无论成败都调用。中断路径留下的东西没有别的出口——在 2b 之前，端口的
//     Cleanup 其实只有合约测试在调，产品路径上从来没人调过。
//   - 用**不带取消**的上下文加一个固定超时。操作被取消时 ctx 已经死了，
//     而收尾恰恰是取消之后最该做完的事；反过来，收尾也不该无限期挂着。
func (s *BackupService) cleanupAdapter(ctx context.Context, adapter BackupAdapter, policy *domain.BackupPolicy, operationID string) {
	if adapter == nil || policy == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	if err := adapter.Cleanup(cleanupCtx, policy, operationID); err != nil {
		// 清理失败**不改**操作的结果：恢复成功这件事不会因为临时库没删掉而变成失败。
		// 但必须留下痕迹——这些残留会一直占着对端的磁盘。
		s.logger.Warn("backup adapter cleanup failed",
			"policy", policy.Name, "operationId", operationID, "error", err)
	}
}

// cleanupTimeout 是收尾动作的固定预算：它发生在操作结束之后，不在任何 Operation
// 的超时预算之内。
const cleanupTimeout = 60 * time.Second

// ---------------------------------------------------------------------------
// 执行入口。worker 调用这三个方法，它们负责把结果落进 backups 表。
// ---------------------------------------------------------------------------

// ExecuteRun 执行一次备份。
func (s *BackupService) ExecuteRun(ctx context.Context, policy *domain.BackupPolicy, operationID string, logf BackupLogf) error {
	if !s.Configured() {
		return domain.NewError(v1.CodeConfigInvalid, "本部署未配置备份存储根，无法备份")
	}
	adapter, err := s.adapterFor(policy.Resource.Kind)
	if err != nil {
		return err
	}
	defer s.cleanupAdapter(ctx, adapter, policy, operationID)

	// 预检必须在产生任何备份产物之前完成——它的全部意义就在这里。
	logf("info", domain.PhaseValidate, "开始预检", map[string]string{"policy": policy.Name})
	report, err := adapter.Preflight(ctx, policy)
	if err != nil {
		return err
	}
	logf("info", domain.PhaseValidate, "预检通过", map[string]string{
		"checks":        itoa(len(report.Checks)),
		"toolVersion":   report.ToolVersion,
		"serverVersion": report.ServerVersion,
	})

	key, keyID, err := s.resolveKey(ctx, policy)
	if err != nil {
		return err
	}

	// 先落一条 running 的记录。它**只有**在真正成功之后才会变成 succeeded，
	// 因此进程被杀、上传中断都不会留下「成功」的记录。
	backupID := s.newID("bkp")
	if err := s.repo.CreateBackup(ctx, &domain.Backup{
		ID:           backupID,
		PolicyID:     policy.ID,
		OperationID:  operationID,
		Status:       domain.BackupRunning,
		ResourceKind: policy.Resource.Kind,
		StartedAt:    s.now(),
	}); err != nil {
		return err
	}

	metadata, logicalBytes, stored, err := s.streamBackup(ctx, adapter, policy, key)
	if err != nil {
		// 失败也要落库：不写的话这条记录永远停在 running，运维分不清
		// 「在跑」还是「早就死了」。
		s.finishBackup(ctx, domain.FinishBackupInput{
			BackupID:     backupID,
			Status:       domain.BackupFailed,
			ErrorCode:    string(domain.CodeOf(err)),
			ErrorMessage: domain.MessageOf(err),
		})
		return err
	}

	logf("info", domain.PhaseExecute, "备份已落盘", map[string]string{
		"digest":       stored.Digest.String(),
		"logicalBytes": itoa64(logicalBytes),
		"storedBytes":  itoa64(stored.Size),
	})

	if _, err := s.finishBackup(ctx, domain.FinishBackupInput{
		BackupID:        backupID,
		Status:          domain.BackupSucceeded,
		StorageDigest:   stored.Digest,
		LogicalBytes:    logicalBytes,
		StoredBytes:     stored.Size,
		Compression:     policy.Encoding.Compression,
		EncryptionKeyID: keyID,
		ServerVersion:   metadata.ServerVersion,
		ClientVersion:   metadata.ClientVersion,
		Tool:            metadata.Tool,
		Labels:          metadata.Labels,
	}); err != nil {
		return err
	}

	s.logger.Info("backup finished", "backupId", backupID, "policy", policy.Name, "digest", stored.Digest.String())
	return nil
}

// streamBackup 把「适配器产出逻辑流 → 传输编码 → 存储」串成一条流水线。
//
// 用 io.Pipe 而不是先攒到内存：备份可能比内存大得多，而整份读进内存正是备份工具
// 在真实数据上会破的地方。
func (s *BackupService) streamBackup(
	ctx context.Context,
	adapter BackupAdapter,
	policy *domain.BackupPolicy,
	key []byte,
) (domain.BackupMetadata, int64, Stored, error) {
	pr, pw := io.Pipe()

	type produced struct {
		metadata     domain.BackupMetadata
		logicalBytes int64
		err          error
	}
	producedCh := make(chan produced, 1)

	go func() {
		encoder, err := backupcodec.NewWriter(pw, policy.Encoding.Compression, key)
		if err != nil {
			_ = pw.CloseWithError(err)
			producedCh <- produced{err: err}
			return
		}
		// 逻辑字节数在**明文一侧**统计：压缩比要靠它和落盘字节数一起才算得出来。
		counter := &countingWriter{dst: encoder}

		metadata, backupErr := adapter.Backup(ctx, policy, counter)
		if backupErr != nil {
			_ = pw.CloseWithError(backupErr)
			producedCh <- produced{err: backupErr}
			return
		}
		// 收尾顺序不能反：编码层要先把最后一段数据刷进管道，管道才能正常关闭。
		if closeErr := encoder.Close(); closeErr != nil {
			_ = pw.CloseWithError(closeErr)
			producedCh <- produced{err: closeErr}
			return
		}
		_ = pw.Close()
		producedCh <- produced{metadata: metadata, logicalBytes: counter.n}
	}()

	// expected 传空：内容是我们自己产出的，没有「事先声明」的摘要可比对；
	// StorageBackend 会边写边算，并把结果还给我们。
	stored, putErr := s.store.Put(ctx, pr, "")
	if putErr != nil {
		// 让写侧尽早停下，别继续往一个已经失败的管道里灌数据。
		_ = pr.CloseWithError(putErr)
	}
	result := <-producedCh

	if putErr != nil {
		return domain.BackupMetadata{}, 0, Stored{}, putErr
	}
	if result.err != nil {
		return domain.BackupMetadata{}, 0, Stored{}, result.err
	}
	return result.metadata, result.logicalBytes, stored, nil
}

// resolveKey 解析加密密钥。返回的 keyID 只在启用了加密时非空。
func (s *BackupService) resolveKey(ctx context.Context, policy *domain.BackupPolicy) ([]byte, string, error) {
	if !policy.Encoding.Encryption.Enabled {
		return nil, "", nil
	}
	if s.secrets == nil {
		return nil, "", domain.NewError(v1.CodeBackupKeyUnresolved,
			"本部署没有配置凭据解析器，无法启用加密")
	}
	value, err := s.secrets.Resolve(ctx, policy.Encoding.Encryption.KeySecret)
	if err != nil {
		// **绝不**降级为不加密：静默产出明文备份是这里最坏的结果，
		// 比「备份失败」坏得多。
		return nil, "", domain.NewError(v1.CodeBackupKeyUnresolved,
			"加密密钥 %s 无法解析: %v", policy.Encoding.Encryption.KeySecret, err)
	}
	key := []byte(value)
	return key, backupcodec.KeyID(key), nil
}

// ExecuteVerify 校验一份已存在的备份：把流读一遍，交给适配器做自洽性检查。
//
// 它**不接触目标资源**，因此比隔离恢复便宜得多；也正因如此，它证明不了
// 「内容能放回去」——那要 restore。
//
// 两件事都在这条路径上发生，它们回答的是**不同**的问题：
//
//  1. 适配器的自洽性检查：「这个工具读不读得动这份备份」；
//  2. 存储摘要核对：「存储里的字节与当初记录的 digest 一致吗」。
//
// 第二件是 2b 补上的（规格 D14）。在它之前，存储层按 digest **定位**文件却从不
// **重算**摘要：`compression: none` 且不加密的备份被改坏或换掉，校验会一路通过。
// 路线图的验收标准写的是「备份文件可通过 checksum 验证」，缺的就是这一环。
func (s *BackupService) ExecuteVerify(ctx context.Context, backupID string, logf BackupLogf) error {
	opened, err := s.openBackup(ctx, backupID)
	if err != nil {
		return err
	}
	defer opened.Reader.Close()

	adapter, err := s.adapterFor(opened.Policy.Resource.Kind)
	if err != nil {
		return err
	}
	defer s.cleanupAdapter(ctx, adapter, opened.Policy, backupID)

	logf("info", domain.PhaseExecute, "开始校验备份流", map[string]string{"digest": opened.Backup.StorageDigest.String()})
	verifyErr := s.verifyStream(ctx, adapter, opened)
	// 校验**不通过**也要落库：「校验过但没通过」与「从没校验过」是两件事。
	if markErr := s.repo.MarkBackupVerified(ctx, backupID, verifyErr == nil, s.now()); markErr != nil {
		s.logger.Error("failed to record verify result", "backupId", backupID, "error", markErr)
	}
	if verifyErr != nil {
		return verifyErr
	}
	logf("info", domain.PhaseFinalize, "校验通过", map[string]string{
		"storedBytes": itoa64(opened.Digest.Size()),
	})
	return nil
}

// verifyStream 依次做适配器自检与存储摘要核对。
func (s *BackupService) verifyStream(ctx context.Context, adapter BackupAdapter, opened openedBackup) error {
	if err := adapter.Verify(ctx, opened.Policy, opened.Reader); err != nil {
		return err
	}
	// 适配器可能没有把流读到底（例如只读目录不读数据），摘要因此需要补读剩余的字节
	// 才算得全。读干净之后的摘要才代表**存储里的全部内容**。
	if _, err := io.Copy(io.Discard, opened.Reader); err != nil {
		return domain.NewError(v1.CodeBackupVerifyFailed, "读取备份流失败: %v", err)
	}
	if opened.Backup.StorageDigest == "" {
		return nil
	}
	if got := opened.Digest.Digest(); got != opened.Backup.StorageDigest {
		return domain.NewError(v1.CodeBackupVerifyFailed,
			"存储内容与记录不一致：记录里的摘要是 %s，实际读到的是 %s——"+
				"这份备份的内容已经变了，不能当作可恢复的备份",
			opened.Backup.StorageDigest, got)
	}
	return nil
}

// ExecuteRestore 恢复一份备份。
func (s *BackupService) ExecuteRestore(ctx context.Context, backupID string, mode domain.RestoreMode, logf BackupLogf) error {
	opened, err := s.openBackup(ctx, backupID)
	if err != nil {
		return err
	}
	defer opened.Reader.Close()

	adapter, err := s.adapterFor(opened.Policy.Resource.Kind)
	if err != nil {
		return err
	}
	defer s.cleanupAdapter(ctx, adapter, opened.Policy, backupID)

	logf("info", domain.PhaseExecute, "开始恢复", map[string]string{
		"backupId": opened.Backup.ID,
		"mode":     string(mode),
	})
	// 恢复**不做**摘要核对（与 verify 不同）：流是边读边往目标写的，等读完才知道摘要
	// 对不对时，数据已经写进去了。要提前知道内容对不对，先跑一次 `backup verify`。
	if err := adapter.Restore(ctx, opened.Policy, opened.Reader, mode); err != nil {
		return err
	}
	logf("info", domain.PhaseFinalize, "恢复完成", map[string]string{"mode": string(mode)})
	return nil
}

// openedBackup 是一份**解码之后**的逻辑流，外加核对存储摘要所需的东西。
type openedBackup struct {
	Backup *domain.Backup
	Policy *domain.BackupPolicy
	// Reader 是解码之后的明文逻辑流：适配器永远只看到明文，不需要知道备份压过还是加过密。
	Reader io.ReadCloser
	// Digest 边读边算**存储字节**（压缩加密之后的那些字节）的摘要，
	// 用来与 backups.storage_digest 核对。
	Digest *domain.Hasher
}

// openBackup 取出备份、它的策略，以及**解码之后**的逻辑流。
//
// 解码（去压缩、解密）在这里完成，因此适配器永远只看到明文逻辑流，
// 不需要知道备份是压过的还是加过密的。
func (s *BackupService) openBackup(ctx context.Context, backupID string) (openedBackup, error) {
	if !s.Configured() {
		return openedBackup{}, domain.NewError(v1.CodeConfigInvalid, "本部署未配置备份存储根")
	}
	backup, err := s.repo.GetBackup(ctx, backupID)
	if err != nil {
		return openedBackup{}, err
	}
	if backup.StorageDigest == "" {
		return openedBackup{}, domain.NewError(v1.CodeBackupNotFound,
			"备份 %s 没有对应的存储内容（状态 %s）", backup.ID, backup.Status)
	}
	policy, err := s.repo.GetBackupPolicy(ctx, backup.PolicyID)
	if err != nil {
		return openedBackup{}, err
	}

	raw, err := s.store.Open(ctx, backup.StorageDigest)
	if err != nil {
		return openedBackup{}, err
	}

	key, _, err := s.resolveKey(ctx, policy)
	if err != nil {
		_ = raw.Close()
		return openedBackup{}, err
	}

	// 摘要算在**存储字节**上（解码之前），与 Put 时算的是同一串字节。
	hasher := domain.NewHasher()
	decoded, err := backupcodec.Decode(io.TeeReader(raw, hasher), backup.Compression, key)
	if err != nil {
		_ = raw.Close()
		return openedBackup{}, err
	}
	return openedBackup{
		Backup: backup,
		Policy: policy,
		Reader: &decodeCloser{Reader: decoded, raw: raw},
		Digest: hasher,
	}, nil
}

func (s *BackupService) finishBackup(ctx context.Context, in domain.FinishBackupInput) (*domain.Backup, error) {
	backup, err := s.repo.FinishBackup(ctx, in, s.now())
	if err != nil {
		s.logger.Error("failed to persist backup result", "backupId", in.BackupID, "error", err)
		return nil, err
	}
	return backup, nil
}

// decodeCloser 把「解码后的读端」与「底层的存储读端」一起关掉：
// 只关解码层会让文件描述符留在那里。
type decodeCloser struct {
	io.Reader
	raw io.Closer
}

func (d *decodeCloser) Close() error { return d.raw.Close() }

// countingWriter 统计写过的**明文**字节数。
type countingWriter struct {
	dst io.Writer
	n   int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.dst.Write(p)
	c.n += int64(n)
	return n, err
}

func itoa(value int) string { return itoa64(int64(value)) }

func itoa64(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

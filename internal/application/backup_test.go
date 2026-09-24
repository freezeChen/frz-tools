package application_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/backup/files"
	"github.com/freezeChen/frz-tools/internal/adapters/blob"
	"github.com/freezeChen/frz-tools/internal/adapters/secret"
	"github.com/freezeChen/frz-tools/internal/adapters/sqlite"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// backupFixture 是一套跑得起来的备份环境。
//
// sourceRoot 是「被备份的目录」在测试机上的真实落点：策略里写的是绝对路径，
// 适配器用 root 前缀把它重定向到这里——因此测试断言的是真实的文件内容与模式。
type backupFixture struct {
	rt          *application.Runtime
	sqlStore    *sqlite.Store
	backupStore *blob.Local
	sourceRoot  string
	declared    string
}

func newBackupFixture(t *testing.T, opts ...func(*application.Options)) *backupFixture {
	t.Helper()

	sourceRoot := t.TempDir()
	backupStore, err := blob.NewLocal(filepath.Join(t.TempDir(), "backups"), 0o640, 0o750)
	if err != nil {
		t.Fatalf("backup store: %v", err)
	}

	sqlStore, err := sqlite.Open(filepath.Join(t.TempDir(), "opsd.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { sqlStore.Close() })
	if err := sqlStore.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	adapter := files.New(files.WithRoot(sourceRoot), files.WithTempRoot(t.TempDir()))
	options := application.Options{
		Repo:           sqlStore,
		BackupStore:    backupStore,
		BackupAdapters: []application.BackupAdapter{adapter},
		Workers:        1,
		Logger:         discardLogger(),
	}
	for _, opt := range opts {
		opt(&options)
	}

	return &backupFixture{
		rt:          application.NewRuntime(options),
		sqlStore:    sqlStore,
		backupStore: backupStore,
		sourceRoot:  sourceRoot,
		declared:    "/srv/data",
	}
}

// savePolicy 提交一份策略。默认不加密——加密的用例单独开，因为它需要真的配一把密钥。
func (f *backupFixture) savePolicy(t *testing.T, name string, mutate ...func(*domain.BackupPolicy)) *domain.BackupPolicy {
	t.Helper()

	policy := &domain.BackupPolicy{
		APIVersion: domain.BackupPolicyAPIVersion,
		Kind:       domain.BackupPolicyKind,
		Name:       name,
		Resource: domain.BackupResource{
			Kind:  domain.BackupResourceFiles,
			Paths: []string{f.declared},
		},
		Encoding: domain.BackupEncoding{
			Compression: domain.CompressionGzip,
			Encryption:  domain.BackupEncryption{Enabled: false},
		},
		Retention: domain.BackupRetention{KeepLast: 3},
	}
	for _, m := range mutate {
		m(policy)
	}

	saved, err := f.rt.Backups.SavePolicy(context.Background(), policy, "tester")
	if err != nil {
		t.Fatalf("save policy: %v", err)
	}
	return saved
}

// populate 在声明路径下写入一份可辨认的内容，并返回它的指纹。
func (f *backupFixture) populate(t *testing.T) string {
	t.Helper()

	base := filepath.Join(f.sourceRoot, strings.TrimPrefix(f.declared, "/"))
	if err := os.MkdirAll(filepath.Join(base, "nested"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "alpha.txt"), []byte("alpha-content\n"), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "nested", "beta.bin"), []byte{0, 1, 2, 255}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return f.fingerprint(t)
}

func (f *backupFixture) fingerprint(t *testing.T) string {
	t.Helper()

	base := filepath.Join(f.sourceRoot, strings.TrimPrefix(f.declared, "/"))
	var entries []string
	err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			entries = append(entries, "dir:"+rel+":"+info.Mode().Perm().String())
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, "file:"+rel+":"+string(content)+":"+info.Mode().Perm().String())
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sortStrings(entries)
	return strings.Join(entries, "|")
}

// runOperation 走完整的创建 → 领取 → 执行路径，用真实的 worker 代码而不是直接调服务。
func (f *backupFixture) runOperation(t *testing.T, req v1.CreateOperationRequest) *domain.Operation {
	t.Helper()

	op, _, err := f.rt.Service.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("create %s: %v", req.Kind, err)
	}
	if _, err := f.rt.Pool.ProcessNext(context.Background()); err != nil {
		t.Fatalf("process: %v", err)
	}
	stored, err := f.rt.Service.Get(context.Background(), op.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return stored
}

func backupRunRequest(policy string) v1.CreateOperationRequest {
	return v1.CreateOperationRequest{
		Kind:     v1.KindBackupRun,
		Resource: policy,
	}
}

// 闭环：备份 → 校验 → 隔离恢复 → 原地恢复 → 内容一致。
func TestBackupRunVerifyRestoreRoundTrip(t *testing.T) {
	f := newBackupFixture(t)
	ctx := context.Background()

	f.savePolicy(t, "orders")
	before := f.populate(t)

	op := f.runOperation(t, backupRunRequest("orders"))
	if op.Status != domain.StatusSucceeded {
		t.Fatalf("备份操作应当成功，got %s (%s: %s)", op.Status, op.ErrorCode, op.ErrorMessage)
	}

	backups, err := f.rt.Backups.List(ctx, "orders", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("应当恰好一条备份记录，got %d", len(backups))
	}
	record := backups[0]
	if record.Status != domain.BackupSucceeded {
		t.Fatalf("备份记录应当 succeeded，got %s（%s）", record.Status, record.ErrorMessage)
	}
	// 内容寻址：digest 必须真的落在备份自己的根里，而且能被打开。
	if record.StorageDigest == "" {
		t.Fatal("成功的备份必须记录 digest")
	}
	if record.LogicalBytes == 0 || record.StoredBytes == 0 {
		t.Fatalf("字节数应当被记录：logical=%d stored=%d", record.LogicalBytes, record.StoredBytes)
	}
	if !record.Usable() {
		t.Fatal("已校验通过之前的备份仍应可参与保留计算（VerifiedOK 为 nil 表示尚未校验）")
	}

	// 校验：走 Operation，且结果落库。
	verifyOp := f.runOperation(t, v1.CreateOperationRequest{
		Kind:     v1.KindBackupVerify,
		Resource: record.ID,
	})
	if verifyOp.Status != domain.StatusSucceeded {
		t.Fatalf("校验应当成功，got %s (%s)", verifyOp.Status, verifyOp.ErrorMessage)
	}
	verified, err := f.rt.Backups.Get(ctx, record.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if verified.VerifiedOK == nil || !*verified.VerifiedOK {
		t.Fatalf("校验结果应当落库为通过，got %v", verified.VerifiedOK)
	}

	// 隔离恢复：不许碰真实资源。
	isolatedOp := f.runOperation(t, v1.CreateOperationRequest{
		Kind:     v1.KindBackupRestore,
		Resource: record.ID,
		Spec:     mustRestoreOptions(t, domain.RestoreIsolated, false),
	})
	if isolatedOp.Status != domain.StatusSucceeded {
		t.Fatalf("隔离恢复应当成功，got %s (%s)", isolatedOp.Status, isolatedOp.ErrorMessage)
	}
	if got := f.fingerprint(t); got != before {
		t.Fatal("隔离恢复不得改动真实资源")
	}

	// 原地恢复 ≠ 从空到有：先破坏再恢复，才能证明「真的把内容写回来了」。
	if err := os.RemoveAll(filepath.Join(f.sourceRoot, strings.TrimPrefix(f.declared, "/"))); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	inPlaceOp := f.runOperation(t, v1.CreateOperationRequest{
		Kind:     v1.KindBackupRestore,
		Resource: record.ID,
		Spec:     mustRestoreOptions(t, domain.RestoreInPlace, true),
	})
	if inPlaceOp.Status != domain.StatusSucceeded {
		t.Fatalf("原地恢复应当成功，got %s (%s)", inPlaceOp.Status, inPlaceOp.ErrorMessage)
	}
	if got := f.fingerprint(t); got != before {
		t.Fatalf("恢复后内容不一致：\nwant %s\ngot  %s", before, got)
	}
}

// D7：原地恢复会覆盖真实数据，必须显式确认——而且在**创建期**就拒绝，
// 不留下一条排队之后才失败的 Operation。
func TestRestoreInPlaceRequiresConfirmation(t *testing.T) {
	f := newBackupFixture(t)
	ctx := context.Background()

	f.savePolicy(t, "orders")
	f.populate(t)
	f.runOperation(t, backupRunRequest("orders"))

	backups, _ := f.rt.Backups.List(ctx, "orders", 0)
	record := backups[0]

	_, _, err := f.rt.Service.Create(ctx, v1.CreateOperationRequest{
		Kind:     v1.KindBackupRestore,
		Resource: record.ID,
		Spec:     mustRestoreOptions(t, domain.RestoreInPlace, false),
	})
	if domain.CodeOf(err) != v1.CodeBackupRestoreUnconfirmed {
		t.Fatalf("want BACKUP_RESTORE_UNCONFIRMED, got %v", err)
	}

	// 隔离恢复不需要确认。
	if _, _, err := f.rt.Service.Create(ctx, v1.CreateOperationRequest{
		Kind:     v1.KindBackupRestore,
		Resource: record.ID,
		Spec:     mustRestoreOptions(t, domain.RestoreIsolated, false),
	}); err != nil {
		t.Fatalf("隔离恢复不该要求确认: %v", err)
	}
}

// 加密开启但密钥解析不了时，备份必须**失败**，而不是静默产出明文备份。
func TestEncryptionWithoutResolvableKeyFails(t *testing.T) {
	f := newBackupFixture(t) // 刻意不注入 SecretResolver
	ctx := context.Background()

	f.savePolicy(t, "encrypted", func(p *domain.BackupPolicy) {
		p.Encoding.Encryption = domain.BackupEncryption{
			Enabled:   true,
			KeySecret: domain.SecretRef{Kind: domain.SecretKindFile, Name: "/etc/opsd/secrets/nope"},
		}
	})
	f.populate(t)

	op := f.runOperation(t, backupRunRequest("encrypted"))
	if op.Status != domain.StatusFailed {
		t.Fatalf("密钥解析不了时备份必须失败，got %s", op.Status)
	}
	if op.ErrorCode != string(v1.CodeBackupKeyUnresolved) {
		t.Fatalf("want BACKUP_KEY_UNRESOLVED, got %q", op.ErrorCode)
	}

	// 关键的一条：绝不能留下一条「成功」的备份记录——那意味着有些字节落盘了，
	// 而它是明文。
	backups, _ := f.rt.Backups.List(ctx, "encrypted", 0)
	for _, record := range backups {
		if record.Status == domain.BackupSucceeded {
			t.Fatal("密钥解析失败时不得产生 succeeded 的备份")
		}
	}
}

// 加密开起来之后：内容真的被加密了，而且能原样恢复。
func TestEncryptionRoundTrip(t *testing.T) {
	keyDir := t.TempDir()
	keyPath := filepath.Join(keyDir, "backup-key")
	if err := os.WriteFile(keyPath, []byte("a-real-key-material"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	resolver := secret.NewResolver([]string{keyDir})

	f := newBackupFixture(t, func(options *application.Options) {
		options.Secrets = resolver
	})

	f.savePolicy(t, "encrypted", func(p *domain.BackupPolicy) {
		p.Encoding.Encryption = domain.BackupEncryption{
			Enabled:   true,
			KeySecret: domain.SecretRef{Kind: domain.SecretKindFile, Name: keyPath},
		}
	})
	before := f.populate(t)

	op := f.runOperation(t, backupRunRequest("encrypted"))
	if op.Status != domain.StatusSucceeded {
		t.Fatalf("加密备份应当成功，got %s (%s)", op.Status, op.ErrorMessage)
	}

	backups, _ := f.rt.Backups.List(context.Background(), "encrypted", 0)
	record := backups[0]
	if record.EncryptionKeyID == "" {
		t.Fatal("加密过的备份必须记录密钥标识——轮换时它是唯一能说明用的是哪把钥匙的依据")
	}

	// 落盘的字节里不得出现明文。通过 StorageBackend 读原始字节，而不是拼路径——
	// 拼路径会把测试绑死在 blob 的目录布局上。
	stored, err := f.backupStore.Open(context.Background(), record.StorageDigest)
	if err != nil {
		t.Fatalf("open stored blob: %v", err)
	}
	raw, err := io.ReadAll(stored)
	stored.Close()
	if err != nil {
		t.Fatalf("read stored blob: %v", err)
	}
	if strings.Contains(string(raw), "alpha-content") {
		t.Fatal("落盘的备份里出现了明文")
	}

	// 恢复回内容。
	if err := os.RemoveAll(filepath.Join(f.sourceRoot, strings.TrimPrefix(f.declared, "/"))); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	restoreOp := f.runOperation(t, v1.CreateOperationRequest{
		Kind:     v1.KindBackupRestore,
		Resource: record.ID,
		Spec:     mustRestoreOptions(t, domain.RestoreInPlace, true),
	})
	if restoreOp.Status != domain.StatusSucceeded {
		t.Fatalf("加密备份的恢复应当成功，got %s (%s)", restoreOp.Status, restoreOp.ErrorMessage)
	}
	if got := f.fingerprint(t); got != before {
		t.Fatalf("加密往返后内容不一致：\nwant %s\ngot  %s", before, got)
	}
}

// D5 的直接回归：制品 GC **不得**碰到备份根。
//
// 1a 的制品 GC 把「digest 不在 artifacts 表里」一律当作孤儿删除，因此两者共用
// 存储根会让 `artifact gc` 删掉全部备份——静默的数据丢失，只有真要恢复时才发现。
func TestArtifactGCDoesNotTouchBackupStore(t *testing.T) {
	artifactRoot := filepath.Join(t.TempDir(), "artifacts")
	artifactStore, err := blob.NewLocal(artifactRoot, 0o640, 0o750)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}

	f := newBackupFixture(t, func(options *application.Options) {
		options.Store = artifactStore
		options.ArtifactPolicy = application.ArtifactPolicy{MaxUploadBytes: 1 << 20, QuotaBytes: 1 << 22}
	})
	ctx := context.Background()

	f.savePolicy(t, "orders")
	f.populate(t)
	if op := f.runOperation(t, backupRunRequest("orders")); op.Status != domain.StatusSucceeded {
		t.Fatalf("备份应当成功: %s %s", op.Status, op.ErrorMessage)
	}

	backups, _ := f.rt.Backups.List(ctx, "orders", 0)
	record := backups[0]

	// 跑一次制品 GC（会回收它自己根下的所有「孤儿」），然后备份内容必须完好。
	if _, err := f.rt.Artifacts.Collect(ctx, 0, 0, false); err != nil {
		t.Fatalf("artifact gc: %v", err)
	}

	// 备份的字节还在，而且真的能读回来。
	opened, err := f.rt.Backups.Get(ctx, record.ID)
	if err != nil {
		t.Fatalf("get backup: %v", err)
	}
	if opened.StorageDigest != record.StorageDigest {
		t.Fatalf("digest 变了：%s → %s", record.StorageDigest, opened.StorageDigest)
	}

	verifyOp := f.runOperation(t, v1.CreateOperationRequest{
		Kind:     v1.KindBackupVerify,
		Resource: record.ID,
	})
	if verifyOp.Status != domain.StatusSucceeded {
		t.Fatalf("制品 GC 之后备份内容应当完好，校验却失败：%s %s", verifyOp.Status, verifyOp.ErrorMessage)
	}
}

// 中断安全：备份记录**先**以 running 落库，只有真正全部写完才变成 succeeded。
// 因此进程被杀、上传中断都不会留下「成功」的记录。
func TestInterruptedBackupLeavesNoSuccessRecord(t *testing.T) {
	f := newBackupFixture(t)
	ctx := context.Background()

	f.savePolicy(t, "orders")
	f.populate(t)
	f.runOperation(t, backupRunRequest("orders"))

	backups, _ := f.rt.Backups.List(ctx, "orders", 0)
	if len(backups) != 1 || backups[0].Status != domain.BackupSucceeded {
		t.Fatalf("正常路径应当留下一条 succeeded 记录，got %+v", backups)
	}

	// 模拟「备份跑到一半进程没了」：手工插一条 running 的记录，然后走恢复流程。
	policy, err := f.rt.Backups.GetPolicy(ctx, "orders")
	if err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if err := f.sqlStore.CreateBackup(ctx, &domain.Backup{
		ID:           "bkp_interrupted",
		PolicyID:     policy.ID,
		Status:       domain.BackupRunning,
		ResourceKind: domain.BackupResourceFiles,
		StartedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := application.Recover(ctx, f.sqlStore, discardLogger(), time.Now().UTC(), application.RecoveryOptions{}); err != nil {
		t.Fatalf("recover: %v", err)
	}

	interrupted, err := f.rt.Backups.Get(ctx, "bkp_interrupted")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if interrupted.Status != domain.BackupFailed {
		t.Fatalf("被中断的备份应当被标记为失败，got %s", interrupted.Status)
	}
	if interrupted.Usable() {
		t.Fatal("失败的备份绝不能被当成可用备份参与保留计算")
	}
}

func TestBackupRejectsDryRun(t *testing.T) {
	f := newBackupFixture(t)
	f.savePolicy(t, "orders")

	_, _, err := f.rt.Service.Create(context.Background(), v1.CreateOperationRequest{
		Kind:     v1.KindBackupRun,
		Resource: "orders",
		DryRun:   true,
	})
	if domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("want INVALID_REQUEST, got %v", err)
	}
}

// 2a 只实现 files；声明 postgres 的策略在**创建期**就该被挡住，
// 而不是先排一条注定失败的 Operation。
// 本版本没有适配器的资源种类，必须在**提交期**就被拒。
//
// 2a 时这个用例断言的是「能存下、但跑不了」——那时 postgres/mysql 确实还没实现。
// 2b 起两者都有了适配器，规则收紧成「没有适配器就不许存」：存一份必然跑不起来的
// 策略进库，只会让人在第一次备份失败时才发现。
func TestBackupRejectsUnregisteredResourceKindAtSubmit(t *testing.T) {
	f := newBackupFixture(t)
	ctx := context.Background()

	_, err := f.rt.Backups.SavePolicy(ctx, postgresPolicy("orders-db"), "tester")
	if domain.CodeOf(err) != v1.CodeManifestInvalid {
		t.Fatalf("want MANIFEST_INVALID（本部署没有 postgres 适配器）, got %v", err)
	}
}

// 但「存进去了」这条路径仍然要挡：库里可能留着一份**旧版本**写下的策略，
// 或者有人绕过了服务直接写库。执行期再判一次，代价只有一次 map 查找。
func TestBackupRejectsUnregisteredResourceKindAtRun(t *testing.T) {
	f := newBackupFixture(t)
	ctx := context.Background()

	// 直接落库，绕过 SavePolicy。
	if _, err := f.sqlStore.SaveBackupPolicy(ctx, postgresPolicy("orders-db"), "bpl_legacy", time.Now().UTC(), "tester"); err != nil {
		t.Fatalf("写库失败: %v", err)
	}

	_, _, err := f.rt.Service.Create(ctx, backupRunRequest("orders-db"))
	if domain.CodeOf(err) != v1.CodeManifestInvalid {
		t.Fatalf("want MANIFEST_INVALID（本版本没有 postgres 适配器）, got %v", err)
	}
}

func postgresPolicy(name string) *domain.BackupPolicy {
	return &domain.BackupPolicy{
		APIVersion: domain.BackupPolicyAPIVersion,
		Kind:       domain.BackupPolicyKind,
		Name:       name,
		Resource: domain.BackupResource{
			Kind:      domain.BackupResourcePostgres,
			DSNSecret: domain.SecretRef{Kind: domain.SecretKindFile, Name: "orders-dsn"},
			Database:  "orders",
		},
		Encoding:  domain.BackupEncoding{Compression: domain.CompressionNone},
		Retention: domain.BackupRetention{KeepLast: 1},
	}
}

// 未配置备份根时，备份类操作必须明确拒绝，而不是跑到写盘时才失败。
func TestBackupUnavailableWithoutStore(t *testing.T) {
	f := newBackupFixture(t, func(options *application.Options) {
		options.BackupStore = nil
	})

	_, _, err := f.rt.Service.Create(context.Background(), backupRunRequest("whatever"))
	if domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("want CONFIG_INVALID, got %v", err)
	}
}

func mustRestoreOptions(t *testing.T, mode domain.RestoreMode, confirmed bool) []byte {
	t.Helper()
	raw, err := application.RestoreOptionsJSON(mode, confirmed)
	if err != nil {
		t.Fatalf("restore options: %v", err)
	}
	return raw
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// D14：校验必须核对**存储摘要**，而不只是问适配器「你读得动吗」。
//
// 存储层按 digest 定位文件却从不重算摘要，所以在这之前，一份 compression: none
// 且不加密的备份被改坏之后，校验会一路通过——路线图的验收标准写的是
// 「备份文件可通过 checksum 验证」，缺的正是这一环。
func TestVerifyDetectsTamperedBlob(t *testing.T) {
	f := newBackupFixture(t)
	ctx := context.Background()

	// compression=none 且不加密：存储里的字节就是 tar 本体，改一个内容字节之后
	// tar 结构仍然完整、仍读得动。于是能发现它的只可能是摘要核对本身。
	f.savePolicy(t, "orders", func(policy *domain.BackupPolicy) {
		policy.Encoding.Compression = domain.CompressionNone
	})
	f.populate(t)

	op := f.runOperation(t, backupRunRequest("orders"))
	if op.Status != domain.StatusSucceeded {
		t.Fatalf("备份应当成功，got %s (%s)", op.Status, op.ErrorMessage)
	}
	record := f.singleBackup(t)

	// 先证明「没被动过时校验是通过的」——否则下面的失败说明不了任何事。
	if clean := f.runOperation(t, v1.CreateOperationRequest{
		Kind: v1.KindBackupVerify, Resource: record.ID,
	}); clean.Status != domain.StatusSucceeded {
		t.Fatalf("未改动时校验应当通过，got %s (%s)", clean.Status, clean.ErrorMessage)
	}

	path := f.storedBlobPath(t)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读存储内容: %v", err)
	}
	const original = "alpha-content"
	if !bytes.Contains(content, []byte(original)) {
		t.Fatalf("存储里应当能直接看到原始内容（compression=none），实际前 200 字节: %q", content[:min(200, len(content))])
	}
	// 等长替换：tar 的头部长度字段仍然自洽，因此适配器的结构校验会放行。
	tampered := bytes.Replace(content, []byte(original), []byte("alpha-CONTENT"), 1)
	if err := os.WriteFile(path, tampered, 0o640); err != nil {
		t.Fatalf("篡改存储内容: %v", err)
	}

	verifyOp := f.runOperation(t, v1.CreateOperationRequest{
		Kind: v1.KindBackupVerify, Resource: record.ID,
	})
	if verifyOp.Status != domain.StatusFailed {
		t.Fatalf("被改过的备份必须校验失败，got %s", verifyOp.Status)
	}
	if verifyOp.ErrorCode != string(v1.CodeBackupVerifyFailed) {
		t.Fatalf("want BACKUP_VERIFY_FAILED, got %s: %s", verifyOp.ErrorCode, verifyOp.ErrorMessage)
	}
	if !strings.Contains(verifyOp.ErrorMessage, "摘要") {
		t.Fatalf("错误信息要指明是摘要不符，got %s", verifyOp.ErrorMessage)
	}

	// 「校验过但没通过」与「从没校验过」是两件事。
	after, err := f.rt.Backups.Get(ctx, record.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.VerifiedOK == nil || *after.VerifiedOK {
		t.Fatalf("校验不通过的记录不得被标成通过，got %v", after.VerifiedOK)
	}
	// 而这份备份也因此不该参与保留策略的计算（D9）。
	if after.Usable() {
		t.Fatal("校验失败的备份不得算作可用")
	}
}

// 端口的 Cleanup 有实现 ≠ 产品路径上有人调它。2b 之前它只有合约测试在调，
// 于是「被中断的恢复留下的临时资源」在生产上没有任何出口。
func TestAdapterCleanupRunsOnProductPath(t *testing.T) {
	var recorder *recordingAdapter
	f := newBackupFixture(t, func(options *application.Options) {
		recorder = &recordingAdapter{BackupAdapter: options.BackupAdapters[0]}
		options.BackupAdapters = []application.BackupAdapter{recorder}
	})

	f.savePolicy(t, "orders")
	f.populate(t)
	op := f.runOperation(t, backupRunRequest("orders"))
	if op.Status != domain.StatusSucceeded {
		t.Fatalf("备份应当成功，got %s (%s)", op.Status, op.ErrorMessage)
	}
	record := f.singleBackup(t)

	f.runOperation(t, v1.CreateOperationRequest{Kind: v1.KindBackupVerify, Resource: record.ID})
	f.runOperation(t, v1.CreateOperationRequest{
		Kind: v1.KindBackupRestore, Resource: record.ID,
		Spec: mustRestoreOptions(t, domain.RestoreIsolated, false),
	})

	// 三次操作各收尾一次。断言「至少 3 次」而不是精确值：将来多一次收尾
	// 不该让这条用例失败，而**一次都没有**才是要抓的。
	if got := recorder.cleanupCount(); got < 3 {
		t.Fatalf("每次操作结束后都应当收尾一次，实际只调用了 %d 次", got)
	}
}

// recordingAdapter 包一层适配器，用来观察产品路径到底调了端口的哪些方法。
type recordingAdapter struct {
	application.BackupAdapter

	mu        sync.Mutex
	cleanups  int
	lastOpsID []string
}

func (r *recordingAdapter) Cleanup(ctx context.Context, policy *domain.BackupPolicy, operationID string) error {
	r.mu.Lock()
	r.cleanups++
	r.lastOpsID = append(r.lastOpsID, operationID)
	r.mu.Unlock()
	return r.BackupAdapter.Cleanup(ctx, policy, operationID)
}

func (r *recordingAdapter) cleanupCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cleanups
}

// singleBackup 取出唯一一条备份记录。
func (f *backupFixture) singleBackup(t *testing.T) *domain.Backup {
	t.Helper()
	backups, err := f.rt.Backups.List(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("应当恰好一条备份记录，got %d", len(backups))
	}
	return &backups[0]
}

// storedBlobPath 找出存储根里唯一的那个 blob 文件。
//
// 刻意**不**复刻一遍 blob 的分片路径规则：那是第二份会与实现漂移的真相。
// 测试要的是「存储里那几个字节」，走一遍目录比复刻规则结实。
func (f *backupFixture) storedBlobPath(t *testing.T) string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(f.backupStore.Root(), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		found = append(found, path)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历存储根: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("存储根里应当恰好一个 blob，got %v", found)
	}
	return found[0]
}

package application

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

const (
	defaultArtifactLimit = 100
	maxArtifactLimit     = 1000
)

type PutArtifactInput struct {
	Name      string
	MediaType string
	Digest    domain.Digest
	Body      io.Reader
	CreatedBy string
}

type ArtifactService struct {
	repo   Repository
	store  StorageBackend
	policy ArtifactPolicy
	newID  func() string
	now    func() time.Time
	logger *slog.Logger
}

func newArtifactService(repo Repository, store StorageBackend, policy ArtifactPolicy, newID func() string, logger *slog.Logger) *ArtifactService {
	return &ArtifactService{
		repo:   repo,
		store:  store,
		policy: policy,
		newID:  newID,
		now:    func() time.Time { return time.Now().UTC() },
		logger: logger,
	}
}

// Put 把上传流写入内容存储，并以 digest 为幂等键登记制品。相同内容重复上传
// 复用同一条记录，不产生新版本。
func (s *ArtifactService) Put(ctx context.Context, in PutArtifactInput) (*domain.Artifact, bool, error) {
	if s.store == nil {
		return nil, false, errArtifactStoreDisabled()
	}
	if in.Digest != "" {
		if err := in.Digest.Validate(); err != nil {
			return nil, false, err
		}
	}

	limited, err := s.limitStream(ctx, in.Body)
	if err != nil {
		return nil, false, err
	}

	stored, err := s.store.Put(ctx, limited, in.Digest)
	if err != nil {
		return nil, false, err
	}

	artifact := &domain.Artifact{
		ID:        s.newID(),
		Digest:    stored.Digest,
		Size:      stored.Size,
		MediaType: in.MediaType,
		Name:      in.Name,
		CreatedAt: s.now(),
		CreatedBy: in.CreatedBy,
	}
	result, created, err := s.repo.CreateArtifact(ctx, artifact)
	if err != nil {
		// 记录失败时同步回收内容，避免留下没有元数据的孤儿 blob。
		if deleteErr := s.store.Delete(ctx, stored.Digest); deleteErr != nil {
			s.logger.Error("failed to roll back artifact content", "digest", stored.Digest, "error", deleteErr)
		}
		return nil, false, err
	}

	if created {
		if err := s.repo.AppendAudit(ctx, domain.AuditEvent{
			EventType: domain.EventArtifactCreated,
			Actor:     in.CreatedBy,
			Resource:  result.Digest.String(),
			Result:    "created",
			Time:      s.now(),
			Details:   map[string]string{"artifactId": result.ID, "size": strconv.FormatInt(result.Size, 10)},
		}); err != nil {
			return nil, false, err
		}
	}
	return result, created, nil
}

func (s *ArtifactService) Get(ctx context.Context, ref string) (*domain.Artifact, error) {
	if err := s.requireStore(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(ref) == "" {
		return nil, domain.NewError(v1.CodeInvalidRequest, "artifact reference is required")
	}
	return s.repo.GetArtifact(ctx, ref)
}

func (s *ArtifactService) List(ctx context.Context, cursor string, limit int) ([]domain.Artifact, error) {
	if err := s.requireStore(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxArtifactLimit {
		limit = defaultArtifactLimit
	}
	return s.repo.ListArtifacts(ctx, cursor, limit)
}

// Verify 从存储重新读取内容并重算摘要，确认落盘内容与记录一致。
func (s *ArtifactService) Verify(ctx context.Context, ref string) (*domain.Artifact, error) {
	artifact, err := s.repo.GetArtifact(ctx, ref)
	if err != nil {
		return nil, err
	}
	if s.store == nil {
		return nil, errArtifactStoreDisabled()
	}

	reader, err := s.store.Open(ctx, artifact.Digest)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	digest, size, err := domain.DigestOf(reader)
	if err != nil {
		return nil, domain.NewError(v1.CodeInternal, "cannot read stored artifact: %v", err)
	}
	if digest != artifact.Digest || size != artifact.Size {
		return nil, domain.NewError(v1.CodeArtifactChecksum,
			"stored content of artifact %s does not match its record", artifact.ID)
	}
	return artifact, nil
}

// Download 打开制品内容供下载。它先按 Verify 的规则把存储内容完整读一遍并重算摘要，
// 确认与记录一致，再重新打开一次交给调用方流式读取。
//
// 「先校验、后输出」是唯一能保证不把与记录不一致的内容当成功响应发出去的顺序：
// 若边发边算，损坏的字节早已出网，而 HTTP 状态码无法追回。代价是下载多读一遍文件，
// 换来的是不需要把整个制品读进内存（制品可能是几百 MB 的归档）。
func (s *ArtifactService) Download(ctx context.Context, ref string) (*domain.Artifact, io.ReadCloser, error) {
	if err := s.requireStore(); err != nil {
		return nil, nil, err
	}
	artifact, err := s.Verify(ctx, ref)
	if err != nil {
		return nil, nil, err
	}

	reader, err := s.store.Open(ctx, artifact.Digest)
	if err != nil {
		return nil, nil, err
	}
	return artifact, reader, nil
}

// Delete 软删除制品并移除内容。被 Release 引用时拒绝，不做级联删除。
func (s *ArtifactService) Delete(ctx context.Context, ref string) error {
	if err := s.requireStore(); err != nil {
		return err
	}

	artifact, err := s.repo.GetArtifact(ctx, ref)
	if err != nil {
		return err
	}

	references, err := s.repo.CountArtifactReferences(ctx, artifact.ID)
	if err != nil {
		return err
	}
	if references > 0 {
		return domain.NewError(v1.CodeArtifactInUse,
			"artifact %s is referenced by %d release(s)", artifact.ID, references).
			WithDetail("references", references)
	}

	if err := s.repo.SoftDeleteArtifact(ctx, artifact.ID, s.now()); err != nil {
		return err
	}
	if s.store != nil {
		if err := s.store.Delete(ctx, artifact.Digest); err != nil {
			s.logger.Error("failed to remove artifact content", "digest", artifact.Digest, "error", err)
		}
	}

	return s.repo.AppendAudit(ctx, domain.AuditEvent{
		EventType: domain.EventArtifactDeleted,
		Resource:  artifact.Digest.String(),
		Result:    "deleted",
		Time:      s.now(),
		Details:   map[string]string{"artifactId": artifact.ID},
	})
}

type GCResult struct {
	DryRun     bool
	Removed    []string
	Orphans    []string
	Kept       int
	FreedBytes int64
}

// Collect 清理超过保留策略且没有被任何 Release 引用的制品，并顺手回收没有元数据的
// 孤儿内容——崩溃在 rename 与写库之间时会留下这种 blob。
func (s *ArtifactService) Collect(ctx context.Context, keepLast int, olderThan time.Duration, dryRun bool) (GCResult, error) {
	result := GCResult{DryRun: dryRun}
	if keepLast < 0 {
		keepLast = 0
	}

	artifacts, err := s.repo.ListArtifacts(ctx, "", maxArtifactLimit)
	if err != nil {
		return result, err
	}

	cutoff := s.now().Add(-olderThan)
	live := map[domain.Digest]bool{}
	for i, artifact := range artifacts {
		live[artifact.Digest] = true

		references, err := s.repo.CountArtifactReferences(ctx, artifact.ID)
		if err != nil {
			return result, err
		}
		if i < keepLast || references > 0 {
			result.Kept++
			continue
		}
		if olderThan > 0 && artifact.CreatedAt.After(cutoff) {
			result.Kept++
			continue
		}
		if !dryRun {
			if err := s.repo.SoftDeleteArtifact(ctx, artifact.ID, s.now()); err != nil {
				return result, err
			}
			if err := s.store.Delete(ctx, artifact.Digest); err != nil {
				return result, err
			}
		}
		result.Removed = append(result.Removed, artifact.ID)
		result.FreedBytes += artifact.Size
	}

	if s.store != nil {
		entries, err := s.store.List(ctx)
		if err != nil {
			return result, err
		}
		for _, entry := range entries {
			if live[entry.Digest] {
				continue
			}
			if !dryRun {
				if err := s.store.Delete(ctx, entry.Digest); err != nil {
					return result, err
				}
			}
			result.Orphans = append(result.Orphans, entry.Digest.String())
			result.FreedBytes += entry.Size
		}
	}

	if !dryRun && (len(result.Removed) > 0 || len(result.Orphans) > 0) {
		if err := s.repo.AppendAudit(ctx, domain.AuditEvent{
			EventType: domain.EventArtifactCollected,
			Result:    "collected",
			Time:      s.now(),
			Details: map[string]string{
				"removed": strconv.Itoa(len(result.Removed)),
				"orphans": strconv.Itoa(len(result.Orphans)),
			},
		}); err != nil {
			return result, err
		}
	}
	return result, nil
}

// limitStream 用「单次上传上限」和「剩余配额」中较小的一个限制上传流，使超限在
// 上传过程中就被截断，而不是写满磁盘之后才发现。
func (s *ArtifactService) limitStream(ctx context.Context, body io.Reader) (io.Reader, error) {
	limit := s.policy.MaxUploadBytes
	code := v1.CodeUploadTooLarge

	if s.policy.QuotaBytes > 0 {
		used, err := s.repo.SumArtifactSizes(ctx)
		if err != nil {
			return nil, err
		}
		remaining := s.policy.QuotaBytes - used
		if remaining <= 0 {
			return nil, domain.NewError(v1.CodeStorageQuotaExceeded,
				"artifact store quota of %d bytes is exhausted", s.policy.QuotaBytes)
		}
		if limit <= 0 || remaining < limit {
			limit = remaining
			code = v1.CodeStorageQuotaExceeded
		}
	}
	if limit <= 0 {
		return body, nil
	}
	return &limitingReader{ctx: ctx, reader: body, remaining: limit, code: code}, nil
}

type limitingReader struct {
	ctx       context.Context
	reader    io.Reader
	remaining int64
	code      v1.ErrorCode
}

func (l *limitingReader) Read(p []byte) (int, error) {
	if err := l.ctx.Err(); err != nil {
		return 0, err
	}
	if l.remaining <= 0 {
		// 已经读满限制：多读一个字节就能区分「刚好达到上限」和「超限」，
		// 否则恰好等于上限的合法上传会被误判。
		var probe [1]byte
		n, err := l.reader.Read(probe[:])
		if n > 0 {
			return 0, domain.NewError(l.code, "upload exceeds the configured limit")
		}
		return 0, err
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.reader.Read(p)
	l.remaining -= int64(n)
	return n, err
}

// requireStore 让所有制品端点在未配置存储时给出同一种、可诊断的错误，
// 而不是各自走到一半才失败。
func (s *ArtifactService) requireStore() error {
	if s.store == nil {
		return errArtifactStoreDisabled()
	}
	return nil
}

func errArtifactStoreDisabled() error {
	return domain.NewError(v1.CodeConfigInvalid,
		"artifactStore.root is not configured, so artifact operations are disabled")
}

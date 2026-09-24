package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

const artifactColumns = `id, digest, size, media_type, name, created_at, created_by, deleted_at`

// CreateArtifact 以 digest 为幂等键写入制品。digest 相同即视为同一制品；
// 若原制品已被软删除，则直接复活——内容完全相同，且删除时已校验它没有被任何
// Release 引用。
func (s *Store) CreateArtifact(ctx context.Context, artifact *domain.Artifact) (*domain.Artifact, bool, error) {
	if err := artifact.Validate(); err != nil {
		return nil, false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	existing, err := scanArtifact(tx.QueryRowContext(ctx,
		`SELECT `+artifactColumns+` FROM artifacts WHERE digest = ?`, artifact.Digest.String()))
	switch {
	case err == nil:
		if existing.DeletedAt != nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE artifacts SET deleted_at = NULL WHERE id = ?`, existing.ID); err != nil {
				return nil, false, err
			}
			existing.DeletedAt = nil
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifacts (id, digest, size, media_type, name, created_at, created_by, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL)`,
		artifact.ID, artifact.Digest.String(), artifact.Size, artifact.MediaType, artifact.Name,
		formatTime(artifact.CreatedAt), nullString(artifact.CreatedBy)); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return artifact, true, nil
}

// GetArtifact 同时接受不透明 ID 和 digest，便于 CLI 直接用摘要引用制品。
func (s *Store) GetArtifact(ctx context.Context, ref string) (*domain.Artifact, error) {
	artifact, err := scanArtifact(s.db.QueryRowContext(ctx,
		`SELECT `+artifactColumns+` FROM artifacts
		 WHERE (id = ? OR digest = ?) AND deleted_at IS NULL`, ref, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NewError(v1.CodeArtifactNotFound, "artifact %q not found", ref)
	}
	if err != nil {
		return nil, err
	}
	return artifact, nil
}

func (s *Store) ListArtifacts(ctx context.Context, cursor string, limit int) ([]domain.Artifact, error) {
	const base = `SELECT ` + artifactColumns + ` FROM artifacts WHERE deleted_at IS NULL`
	query := base + ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args := []any{limit}
	if cursor != "" {
		query = `SELECT ` + artifactColumns + ` FROM artifacts
			WHERE deleted_at IS NULL
			  AND (created_at, id) < (SELECT created_at, id FROM artifacts WHERE id = ?)
			ORDER BY created_at DESC, id DESC LIMIT ?`
		args = []any{cursor, limit}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var artifacts []domain.Artifact
	for rows.Next() {
		artifact, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, *artifact)
	}
	return artifacts, rows.Err()
}

func (s *Store) SoftDeleteArtifact(ctx context.Context, id string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE artifacts SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, formatTime(now), id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return domain.NewError(v1.CodeArtifactNotFound, "artifact %q not found", id)
	}
	return nil
}

func (s *Store) CountArtifactReferences(ctx context.Context, artifactID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM releases WHERE artifact_id = ?`, artifactID).Scan(&count)
	return count, err
}

func (s *Store) SumArtifactSizes(ctx context.Context) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT SUM(size) FROM artifacts WHERE deleted_at IS NULL`).Scan(&total)
	return total.Int64, err
}

func (s *Store) CreateApplication(ctx context.Context, app *domain.Application) error {
	if err := app.Validate(); err != nil {
		return err
	}
	labels, err := encodeLabels(app.Labels)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO applications (id, name, labels_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		app.ID, app.Name, labels, formatTime(app.CreatedAt), formatTime(app.UpdatedAt))
	if isUniqueViolation(err) {
		return domain.NewError(v1.CodeInvalidRequest, "application %q already exists", app.Name)
	}
	return err
}

func (s *Store) GetApplication(ctx context.Context, ref string) (*domain.Application, error) {
	app, err := scanApplication(s.db.QueryRowContext(ctx, `
		SELECT id, name, labels_json, created_at, updated_at
		FROM applications WHERE id = ? OR name = ?`, ref, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NewError(v1.CodeApplicationNotFound, "application %q not found", ref)
	}
	if err != nil {
		return nil, err
	}
	return app, nil
}

func (s *Store) ListApplications(ctx context.Context) ([]domain.Application, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, labels_json, created_at, updated_at
		FROM applications ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []domain.Application
	for rows.Next() {
		app, err := scanApplication(rows)
		if err != nil {
			return nil, err
		}
		apps = append(apps, *app)
	}
	return apps, rows.Err()
}

func (s *Store) CreateRelease(ctx context.Context, release *domain.Release) (*domain.Release, error) {
	if err := release.Validate(); err != nil {
		return nil, err
	}

	// 外键错误信息不具可读性，先显式确认被引用的对象存在。
	if _, err := s.GetApplication(ctx, release.ApplicationID); err != nil {
		return nil, err
	}
	if _, err := s.GetArtifact(ctx, release.ArtifactID); err != nil {
		return nil, err
	}

	labels, err := encodeLabels(release.Labels)
	if err != nil {
		return nil, err
	}
	status := release.Status
	if status == "" {
		// 回填给调用方：返回的对象应当反映**库里的事实**，而不是调用方传进来时的零值
		// （否则调用方要自己知道默认值是 created 才对得上）。
		status = domain.ReleaseCreated
		release.Status = status
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO releases (id, application_id, artifact_id, version, labels_json, created_at, created_by, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		release.ID, release.ApplicationID, release.ArtifactID, release.Version,
		labels, formatTime(release.CreatedAt), nullString(release.CreatedBy), string(status))
	if isUniqueViolation(err) {
		return nil, domain.NewError(v1.CodeReleaseConflict,
			"application %s already has a release with version %q", release.ApplicationID, release.Version)
	}
	if err != nil {
		return nil, err
	}
	return release, nil
}

func (s *Store) GetRelease(ctx context.Context, id string) (*domain.Release, error) {
	release, err := scanRelease(s.db.QueryRowContext(ctx, `
		SELECT `+releaseColumns+` FROM releases WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NewError(v1.CodeReleaseNotFound, "release %q not found", id)
	}
	if err != nil {
		return nil, err
	}
	return release, nil
}

func (s *Store) ListReleases(ctx context.Context, applicationID string, limit int) ([]domain.Release, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+releaseColumns+` FROM releases WHERE application_id = ?
		ORDER BY created_at DESC, id DESC LIMIT ?`, applicationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var releases []domain.Release
	for rows.Next() {
		release, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		releases = append(releases, *release)
	}
	return releases, rows.Err()
}

func scanArtifact(sc scanner) (*domain.Artifact, error) {
	var (
		artifact  domain.Artifact
		digest    string
		createdAt string
		createdBy sql.NullString
		deletedAt sql.NullString
	)
	if err := sc.Scan(&artifact.ID, &digest, &artifact.Size, &artifact.MediaType, &artifact.Name,
		&createdAt, &createdBy, &deletedAt); err != nil {
		return nil, err
	}
	artifact.Digest = domain.Digest(digest)
	artifact.CreatedBy = createdBy.String

	parsed, err := parseTime(createdAt)
	if err != nil {
		return nil, err
	}
	artifact.CreatedAt = parsed

	if deletedAt.Valid {
		deleted, err := parseTime(deletedAt.String)
		if err != nil {
			return nil, err
		}
		artifact.DeletedAt = &deleted
	}
	return &artifact, nil
}

func scanApplication(sc scanner) (*domain.Application, error) {
	var (
		app       domain.Application
		labels    sql.NullString
		createdAt string
		updatedAt string
	)
	if err := sc.Scan(&app.ID, &app.Name, &labels, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	decoded, err := decodeLabels(labels)
	if err != nil {
		return nil, err
	}
	app.Labels = decoded

	if app.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if app.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &app, nil
}

// releaseColumns 是 reads 用的列清单。集中一处，免得三处 SELECT 各自漂移出不同的字段集。
const releaseColumns = `id, application_id, artifact_id, version, labels_json, created_at, created_by,
	status, COALESCE(directory, ''), activated_at, finished_at,
	COALESCE(error_code, ''), COALESCE(error_message, '')`

func scanRelease(sc scanner) (*domain.Release, error) {
	var (
		release     domain.Release
		labels      sql.NullString
		createdAt   string
		createdBy   sql.NullString
		activated   sql.NullString
		finished    sql.NullString
		statusValue string
	)
	if err := sc.Scan(&release.ID, &release.ApplicationID, &release.ArtifactID, &release.Version,
		&labels, &createdAt, &createdBy, &statusValue, &release.Directory,
		&activated, &finished, &release.ErrorCode, &release.ErrorMessage); err != nil {
		return nil, err
	}
	release.CreatedBy = createdBy.String
	release.Status = domain.ReleaseStatus(statusValue)

	decoded, err := decodeLabels(labels)
	if err != nil {
		return nil, err
	}
	release.Labels = decoded

	if release.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	// 可空时间统一用「先看 Valid 再 parse」的写法（与 scanBackup 同一套）。
	if activated.Valid {
		parsed, err := parseTime(activated.String)
		if err != nil {
			return nil, err
		}
		release.ActivatedAt = &parsed
	}
	if finished.Valid {
		parsed, err := parseTime(finished.String)
		if err != nil {
			return nil, err
		}
		release.FinishedAt = &parsed
	}
	return &release, nil
}

func encodeLabels(labels map[string]string) (any, error) {
	if len(labels) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(labels)
	if err != nil {
		return nil, domain.NewError(v1.CodeInvalidRequest, "labels must be valid key/value pairs: %v", err)
	}
	return string(encoded), nil
}

func decodeLabels(raw sql.NullString) (map[string]string, error) {
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(raw.String), &labels); err != nil {
		return nil, err
	}
	return labels, nil
}

// 用字符串匹配判断约束冲突，是为了不依赖驱动内部包（modernc.org/sqlite/lib）的常量；
// SQLite 的约束错误信息是稳定的。
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// 下面四个方法实现 release 的状态机。每一次转移都带 status 条件，因此并发下重复调用是
// **幂等**的：第二次 UPDATE 影响 0 行，而不会把一个已经 active 的版本倒退回去。

// MarkReleaseDeploying 把一次部署推进到 deploying，并记下它的目录。
//
// 允许的来源状态是 created / failed / removed：前两个是「这一次部署要开始了」（重试同一个
// 版本走的就是失败那一行），最后一个是「这个版本的目录被保留策略清掉了，现在重新部署它」。
// **不允许**从 active / superseded 进来——那意味着「已经在跑或跑过的版本又要部署一次」，
// 而调用方应当先把它识别成幂等或冲突（见 application 层的判断）。
func (s *Store) MarkReleaseDeploying(ctx context.Context, id, directory string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE releases SET status = ?, directory = ?, activated_at = NULL, finished_at = NULL,
			error_code = NULL, error_message = NULL
		WHERE id = ? AND status IN (?, ?, ?)`,
		string(domain.ReleaseDeploying), directory, id,
		string(domain.ReleaseCreated), string(domain.ReleaseFailed), string(domain.ReleaseRemoved))
	return err
}

// ActivateRelease 把该 release 置为 active，并把同应用里**原本 active 的那个**置为 superseded。
//
// 两件事必须在**同一个事务**里：中间崩一下会出现「两个 active」或「一个都没有」的状态，
// 而 current 指针只有一个——那正是「现在跑的是哪个版本」这个问题的第二个答案。
func (s *Store) ActivateRelease(ctx context.Context, id string, now time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE releases SET status = ?
		WHERE status = ? AND application_id = (SELECT application_id FROM releases WHERE id = ?)`,
		string(domain.ReleaseSuperseded), string(domain.ReleaseActive), id)
	if err != nil {
		return 0, err
	}
	superseded, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE releases SET status = ?, activated_at = ?, finished_at = ? WHERE id = ?`,
		string(domain.ReleaseActive), formatTime(now), formatTime(now), id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(superseded), nil
}

// FailRelease 记下一次失败的部署。
//
// directory 被**清空**：失败的 release 目录会被删掉，于是「failed 的行没有目录」是自洽的，
// 而不是让 list 时看起来像「库里有记录、盘上没目录」。目录路径本身进审计与 Operation 日志，
// 排查时从那里查。
func (s *Store) FailRelease(ctx context.Context, id, code, message string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE releases SET status = ?, finished_at = ?, error_code = ?, error_message = ?,
			directory = NULL
		WHERE id = ? AND status IN (?, ?)`,
		string(domain.ReleaseFailed), formatTime(now), nullString(code), nullString(message), id,
		string(domain.ReleaseCreated), string(domain.ReleaseDeploying))
	return err
}

// MarkReleaseRemoved 标记一个 release 的目录已被保留策略清理。
//
// 只在**非 active** 的行上生效：current 指向的那个永远不能被标记为 removed（ReleaseAdapter
// 也会拒绝真的删它，这是第二道保险）。
func (s *Store) MarkReleaseRemoved(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE releases SET status = ?, directory = NULL, finished_at = ?
		WHERE id = ? AND status IN (?, ?)`,
		string(domain.ReleaseRemoved), formatTime(now), id,
		string(domain.ReleaseSuperseded), string(domain.ReleaseFailed))
	return err
}

// ActiveRelease 返回某个应用当前激活的 release（没有时返回 nil, nil）。
//
// 「当前激活」的唯一权威是数据库里的这一行，而 current 符号链接是它的镜像；两者不一致时
// 以数据库为准（部署时是先切链接再写库，因此中途崩溃会留下「链接指向新的、库里还是旧的」
// ——那种情况下回滚的依据应当是库里那个，见部署流程的失败处理）。
func (s *Store) ActiveRelease(ctx context.Context, applicationID string) (*domain.Release, error) {
	release, err := scanRelease(s.db.QueryRowContext(ctx, `
		SELECT `+releaseColumns+` FROM releases
		WHERE application_id = ? AND status = ?
		ORDER BY activated_at DESC, id DESC LIMIT 1`,
		applicationID, string(domain.ReleaseActive)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return release, nil
}

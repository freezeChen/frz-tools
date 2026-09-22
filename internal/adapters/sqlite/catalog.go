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
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO releases (id, application_id, artifact_id, version, labels_json, created_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		release.ID, release.ApplicationID, release.ArtifactID, release.Version,
		labels, formatTime(release.CreatedAt), nullString(release.CreatedBy))
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
		SELECT id, application_id, artifact_id, version, labels_json, created_at, created_by
		FROM releases WHERE id = ?`, id))
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
		SELECT id, application_id, artifact_id, version, labels_json, created_at, created_by
		FROM releases WHERE application_id = ?
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

func scanRelease(sc scanner) (*domain.Release, error) {
	var (
		release   domain.Release
		labels    sql.NullString
		createdAt string
		createdBy sql.NullString
	)
	if err := sc.Scan(&release.ID, &release.ApplicationID, &release.ArtifactID, &release.Version,
		&labels, &createdAt, &createdBy); err != nil {
		return nil, err
	}
	release.CreatedBy = createdBy.String

	decoded, err := decodeLabels(labels)
	if err != nil {
		return nil, err
	}
	release.Labels = decoded

	if release.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
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

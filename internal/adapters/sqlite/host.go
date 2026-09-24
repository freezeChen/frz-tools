package sqlite

import (
	"context"
	"database/sql"
	"errors"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

const hostColumns = `id, name, address, labels_json, created_at, updated_at`

const environmentColumns = `id, name, labels_json, created_at, updated_at`

func (s *Store) CreateHost(ctx context.Context, host *domain.Host) error {
	if err := host.Validate(); err != nil {
		return err
	}
	labels, err := encodeLabels(host.Labels)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO hosts (id, name, address, labels_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		host.ID, host.Name, host.Address, labels, formatTime(host.CreatedAt), formatTime(host.UpdatedAt))
	if isUniqueViolation(err) {
		return domain.NewError(v1.CodeInvalidRequest, "host %q already exists", host.Name)
	}
	return err
}

// GetHost 同时接受不透明 ID 与主机名称，便于 CLI 直接用名称引用。
func (s *Store) GetHost(ctx context.Context, ref string) (*domain.Host, error) {
	host, err := scanHost(s.db.QueryRowContext(ctx,
		`SELECT `+hostColumns+` FROM hosts WHERE id = ? OR name = ?`, ref, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NewError(v1.CodeHostNotFound, "host %q not found", ref)
	}
	if err != nil {
		return nil, err
	}
	return host, nil
}

func (s *Store) ListHosts(ctx context.Context) ([]domain.Host, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+hostColumns+` FROM hosts ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []domain.Host
	for rows.Next() {
		host, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, *host)
	}
	return hosts, rows.Err()
}

// FindLocalHost 按「本机」的语义查找：address 为空。数据库层面不禁止出现第二条
// 这样的记录，因此这里取最早创建的一条，让自举行为稳定且可重复。
func (s *Store) FindLocalHost(ctx context.Context) (*domain.Host, error) {
	host, err := scanHost(s.db.QueryRowContext(ctx,
		`SELECT `+hostColumns+` FROM hosts
		 WHERE address = '' ORDER BY created_at, id LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return host, nil
}

func (s *Store) CreateEnvironment(ctx context.Context, environment *domain.Environment) error {
	if err := environment.Validate(); err != nil {
		return err
	}
	labels, err := encodeLabels(environment.Labels)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO environments (id, name, labels_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		environment.ID, environment.Name, labels,
		formatTime(environment.CreatedAt), formatTime(environment.UpdatedAt))
	if isUniqueViolation(err) {
		return domain.NewError(v1.CodeInvalidRequest, "environment %q already exists", environment.Name)
	}
	return err
}

// GetEnvironment 同时接受不透明 ID 与环境名称。
func (s *Store) GetEnvironment(ctx context.Context, ref string) (*domain.Environment, error) {
	environment, err := scanEnvironment(s.db.QueryRowContext(ctx,
		`SELECT `+environmentColumns+` FROM environments WHERE id = ? OR name = ?`, ref, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NewError(v1.CodeEnvNotFound, "environment %q not found", ref)
	}
	if err != nil {
		return nil, err
	}
	return environment, nil
}

func (s *Store) ListEnvironments(ctx context.Context) ([]domain.Environment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+environmentColumns+` FROM environments ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var environments []domain.Environment
	for rows.Next() {
		environment, err := scanEnvironment(rows)
		if err != nil {
			return nil, err
		}
		environments = append(environments, *environment)
	}
	return environments, rows.Err()
}

func scanHost(sc scanner) (*domain.Host, error) {
	var (
		host      domain.Host
		labels    sql.NullString
		createdAt string
		updatedAt string
	)
	if err := sc.Scan(&host.ID, &host.Name, &host.Address, &labels, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	decoded, err := decodeLabels(labels)
	if err != nil {
		return nil, err
	}
	host.Labels = decoded

	if host.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if host.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &host, nil
}

func scanEnvironment(sc scanner) (*domain.Environment, error) {
	var (
		environment domain.Environment
		labels      sql.NullString
		createdAt   string
		updatedAt   string
	)
	if err := sc.Scan(&environment.ID, &environment.Name, &labels, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	decoded, err := decodeLabels(labels)
	if err != nil {
		return nil, err
	}
	environment.Labels = decoded

	if environment.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if environment.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &environment, nil
}

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// PutApplicationSpec 写入应用的当前规格，已存在则整份覆盖。仓储不解释 spec_json：
// 严格解码与字段校验属于领域层，在这里再实现一套只会让两处规则各自漂移。
func (s *Store) PutApplicationSpec(ctx context.Context, applicationID string, specJSON []byte, now time.Time, updatedBy string) error {
	// 外键报错的信息不具可读性，先显式确认应用存在。
	if _, err := s.GetApplication(ctx, applicationID); err != nil {
		return err
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO application_specs (application_id, spec_json, updated_at, updated_by)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (application_id) DO UPDATE SET
			spec_json  = excluded.spec_json,
			updated_at = excluded.updated_at,
			updated_by = excluded.updated_by`,
		applicationID, string(specJSON), formatTime(now), nullString(updatedBy))
	return err
}

// GetApplicationSpec 取回应用的当前规格。解码用领域类型，因此读出来的规格与
// 领域层的字段、默认值语义天然一致。
func (s *Store) GetApplicationSpec(ctx context.Context, applicationID string) (*domain.ApplicationSpec, error) {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT spec_json FROM application_specs WHERE application_id = ?`, applicationID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NewError(v1.CodeSpecNotFound, "application %q has no spec", applicationID)
	}
	if err != nil {
		return nil, err
	}

	spec := &domain.ApplicationSpec{}
	if err := json.Unmarshal([]byte(raw), spec); err != nil {
		// 落库的 JSON 由本进程写出，解不开说明数据库被外部改过或被写坏，
		// 属于内部不一致而不是调用方的输入问题。
		return nil, domain.NewError(v1.CodeInternal,
			"application %q 的 spec_json 无法解码: %v", applicationID, err)
	}
	return spec, nil
}

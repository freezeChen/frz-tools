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

// 应用规格有两层（迭代 3 规格 D8）：
//
//   - **应用级**（release_id IS NULL）：今天的语义，`spec put` 写的就是它，也是
//     `runtime.*` 读的那一份；迁移 `0009` 之前的行都在这一层。
//   - **每个 release 一份**：部署时写下，回滚时读回——**回滚连配置一起回滚**，
//     否则「回到上一个版本」只回了一半（1c 第 15 节决定 3 的原话）。
//
// 仓储不解释 spec_json：严格解码与字段校验属于领域层，在这里再实现一套只会让两处规则各自漂移。

// PutApplicationSpec 写入应用的**应用级**当前规格，已存在则整份覆盖。
func (s *Store) PutApplicationSpec(ctx context.Context, applicationID string, specJSON []byte, now time.Time, updatedBy string) error {
	if _, err := s.GetApplication(ctx, applicationID); err != nil {
		return err
	}
	return putSpec(ctx, s.db, applicationID, nil, specJSON, now, updatedBy)
}

// GetApplicationSpec 取回应用的**应用级**当前规格。
//
// `release_id IS NULL` 这个条件不能省：迁移之后同一个应用会有多行（一行应用级 + 每个 release
// 一行），只按 application_id 查会取到任意一行——而「任意一行」在测试里往往恰好是对的，
// 到真机上才变成「改了这个版本的配置，另一个版本的行为也变了」。
func (s *Store) GetApplicationSpec(ctx context.Context, applicationID string) (*domain.ApplicationSpec, error) {
	return getSpec(ctx, s.db, applicationID, nil)
}

// PutApplicationSpecForRelease 把该 release 采用的规格写下来。部署时调用。
func (s *Store) PutApplicationSpecForRelease(
	ctx context.Context, applicationID, releaseID string, specJSON []byte, now time.Time, updatedBy string,
) error {
	if _, err := s.GetApplication(ctx, applicationID); err != nil {
		return err
	}
	if releaseID == "" {
		return domain.NewError(v1.CodeInvalidRequest, "release_id 不能为空（应用级规格请用 PutApplicationSpec）")
	}
	return putSpec(ctx, s.db, applicationID, &releaseID, specJSON, now, updatedBy)
}

// GetApplicationSpecForRelease 取回该 release 采用的规格。回滚时调用。
func (s *Store) GetApplicationSpecForRelease(ctx context.Context, applicationID, releaseID string) (*domain.ApplicationSpec, error) {
	return getSpec(ctx, s.db, applicationID, &releaseID)
}

// execer 是 *sql.DB 与 *sql.Tx 的公共部分——与 store.go 里那个同名接口同形，
// 这里的两个函数只需要 Exec/QueryRow。
type specExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func putSpec(ctx context.Context, ex specExecer, applicationID string, releaseID *string, specJSON []byte, now time.Time, updatedBy string) error {
	var release any
	if releaseID != nil {
		release = *releaseID
	}

	// 冲突目标随层级不同：
	//   - 应用级那一行用**偏索引**做目标（`WHERE release_id IS NULL`）。不能写成
	//     `ON CONFLICT (application_id, release_id)`：SQLite 认为 NULL 互不相等，
	//     那个目标对 release_id 为 NULL 的行永远不触发，于是每写一次就多一行。
	//   - release 级那一行是普通的复合主键，直接用它。
	if releaseID == nil {
		_, err := ex.ExecContext(ctx, `
			INSERT INTO application_specs (application_id, release_id, spec_json, updated_at, updated_by)
			VALUES (?, NULL, ?, ?, ?)
			ON CONFLICT (application_id) WHERE release_id IS NULL DO UPDATE SET
				spec_json  = excluded.spec_json,
				updated_at = excluded.updated_at,
				updated_by = excluded.updated_by`,
			applicationID, string(specJSON), formatTime(now), nullString(updatedBy))
		return err
	}

	_, err := ex.ExecContext(ctx, `
		INSERT INTO application_specs (application_id, release_id, spec_json, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (application_id, release_id) DO UPDATE SET
			spec_json  = excluded.spec_json,
			updated_at = excluded.updated_at,
			updated_by = excluded.updated_by`,
		applicationID, release, string(specJSON), formatTime(now), nullString(updatedBy))
	return err
}

func getSpec(ctx context.Context, ex specExecer, applicationID string, releaseID *string) (*domain.ApplicationSpec, error) {
	var (
		raw string
		err error
	)
	if releaseID == nil {
		err = ex.QueryRowContext(ctx,
			`SELECT spec_json FROM application_specs WHERE application_id = ? AND release_id IS NULL`,
			applicationID).Scan(&raw)
	} else {
		err = ex.QueryRowContext(ctx,
			`SELECT spec_json FROM application_specs WHERE application_id = ? AND release_id = ?`,
			applicationID, *releaseID).Scan(&raw)
	}
	if errors.Is(err, sql.ErrNoRows) {
		if releaseID == nil {
			return nil, domain.NewError(v1.CodeSpecNotFound, "application %q has no spec", applicationID)
		}
		return nil, domain.NewError(v1.CodeSpecNotFound,
			"release %q 没有记录规格（它可能是在迭代 3 之前部署的）", *releaseID)
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

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// ReplaceApplicationTargets 用一个新列表**替换**某个应用的部署目标。
//
// 替换而不是增量：这张表表达的是「现在应该在哪几台」。增量语义会让「撤掉一台」变成
// 一个必须显式表达的动作（否则它永远删不掉），而运维真正想说的就是「现在是这样」。
//
// 整个动作在一个事务里：任一主机名不存在就回滚，**一台都不改**。写进去一半比不写更糟
// ——它会让下一次批量发布少发一台，而少发的那台在汇总里看起来是「本来就不该发」。
func (s *Store) ReplaceApplicationTargets(ctx context.Context, application string, hostNames []string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 先把全部主机名解析成 hosts 表里的行。缺一个就整体拒绝：打错一个字母的后果
	// 不该等到部署那天才显形。
	for _, hostName := range hostNames {
		var exists string
		err := tx.QueryRowContext(ctx, `SELECT name FROM hosts WHERE name = ?`, hostName).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NewError(v1.CodeHostNotFound,
				"主机 %q 不在本机的主机表里：先创建它（opsctl host create），再登记部署目标", hostName)
		}
		if err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM application_targets WHERE application_name = ?`, application); err != nil {
		return err
	}
	for _, hostName := range hostNames {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO application_targets (application_name, host_name, created_at) VALUES (?, ?, ?)`,
			application, hostName, formatTime(now)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListApplicationTargets(ctx context.Context, application string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT host_name FROM application_targets WHERE application_name = ? ORDER BY created_at, host_name`,
		application)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hosts := make([]string, 0, 4)
	for rows.Next() {
		var hostName string
		if err := rows.Scan(&hostName); err != nil {
			return nil, err
		}
		hosts = append(hosts, hostName)
	}
	return hosts, rows.Err()
}

// ListAllApplicationTargets 一次给出所有应用的部署目标——5a 欠下的「跨主机应用清单
// 汇总」就是它。
//
// 它读的是**声明的意图**，不是「到每台机上问一圈」。两者会不一致，而不一致时这份
// 声明才是权威：一台机没登记这个应用，不代表它不该跑。
func (s *Store) ListAllApplicationTargets(ctx context.Context) ([]domain.ApplicationTargets, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT application_name, host_name FROM application_targets
		 ORDER BY application_name, created_at, host_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []domain.ApplicationTargets
	for rows.Next() {
		var application, hostName string
		if err := rows.Scan(&application, &hostName); err != nil {
			return nil, err
		}
		if len(result) == 0 || result[len(result)-1].Application != application {
			result = append(result, domain.ApplicationTargets{Application: application})
		}
		last := &result[len(result)-1]
		last.Hosts = append(last.Hosts, hostName)
	}
	return result, rows.Err()
}

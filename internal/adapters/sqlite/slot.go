package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// 迭代 4a：槽位的数据层。
//
// 三件事分开存，是因为它们的**生命力**不同：
//   · `releases.slot` 属于「这一次部署」——它是历史，部署完就不再变；
//   · `application_slots` 是「这一侧现在什么状态」——每次动作都会被覆盖；
//   · `applications.serving_slot` 是「现在哪一侧接流量」——它只有一个值，
//     而且是**Nginx 配置的镜像**（迭代 4 规格 D2：对账以 Nginx 为准）。
//
// 把它们合成一处会有个具体后果：热路径上每次读「谁在接流量」都要去翻历史表。

// SetReleaseSlot 记下这次部署落在哪个槽位。
//
// 空槽位会把列写回 NULL（单槽形态）——「把一次部署从蓝绿改回单槽」不该靠一条 UPDATE
// 之外的任何魔法。
func (s *Store) SetReleaseSlot(ctx context.Context, releaseID string, slot domain.Slot) error {
	if slot != "" && !slot.Valid() {
		return domain.NewError(v1.CodeInvalidRequest, "slot 取值非法: %q", slot)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE releases SET slot = ? WHERE id = ?`,
		nullString(string(slot)), releaseID)
	return err
}

// PutApplicationSlot 写入或更新一个槽位的运营视图。
//
// `switchedAt` 为 nil 表示**这一次动作没有切流量**，此时保留原来那个时间：
// 把「刚做完一次部署但没切流」写成 switched_at = NULL，会让排查时最想看的那个时间
// （「流量是什么时候切到这一侧的」）凭空消失。
//
// 冲突目标直接写复合主键：这张表的两个主键列都是 NOT NULL，因此 `ON CONFLICT` 对它
// 一定触发——与 `application_specs` 那次不同（那边 release_id 可为 NULL，而 SQLite 认为
// NULL 互不相等，主键对它形同虚设，只能用偏索引；见 0009 的注释）。
func (s *Store) PutApplicationSlot(
	ctx context.Context, applicationID string, slot domain.Slot, releaseID string,
	state domain.SlotState, switchedAt *time.Time, now time.Time,
) error {
	if !slot.Valid() {
		return domain.NewError(v1.CodeInvalidRequest, "slot 取值非法: %q", slot)
	}
	if !state.Valid() {
		return domain.NewError(v1.CodeInvalidRequest, "槽位状态取值非法: %q", state)
	}
	if _, err := s.GetApplication(ctx, applicationID); err != nil {
		return err
	}

	var switched any
	if switchedAt != nil {
		switched = formatTime(*switchedAt)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO application_slots (application_id, slot, release_id, state, switched_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (application_id, slot) DO UPDATE SET
			release_id  = excluded.release_id,
			state       = excluded.state,
			switched_at = COALESCE(excluded.switched_at, application_slots.switched_at),
			updated_at  = excluded.updated_at`,
		applicationID, string(slot), nullString(releaseID), string(state), switched, formatTime(now))
	return err
}

// ApplicationSlots 返回某个应用已有的槽位行（按槽位名排序：blue 在 green 前）。
//
// 它**不补齐**「没有行的那一侧」：调用方要区分「这一侧没部署过」与「这一侧状态是空」，
// 补一行零值会让这两种情况长得一样。`domain.Slots` 的固定顺序给出了补齐的依据。
func (s *Store) ApplicationSlots(ctx context.Context, applicationID string) ([]domain.ApplicationSlot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT application_id, slot, release_id, state, switched_at, updated_at
		FROM application_slots WHERE application_id = ? ORDER BY slot`, applicationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var slots []domain.ApplicationSlot
	for rows.Next() {
		var (
			slot        domain.ApplicationSlot
			releaseID   sql.NullString
			state       string
			switchedAt  sql.NullString
			updatedAt   string
			slotNameRaw string
		)
		if err := rows.Scan(&slot.ApplicationID, &slotNameRaw, &releaseID, &state, &switchedAt, &updatedAt); err != nil {
			return nil, err
		}
		slot.Slot = domain.Slot(slotNameRaw)
		slot.ReleaseID = releaseID.String
		slot.State = domain.SlotState(state)
		if switchedAt.Valid {
			parsed, err := parseTime(switchedAt.String)
			if err != nil {
				return nil, err
			}
			slot.SwitchedAt = &parsed
		}
		if slot.UpdatedAt, err = parseTime(updatedAt); err != nil {
			return nil, err
		}
		slots = append(slots, slot)
	}
	return slots, rows.Err()
}

// SetServingSlot 记下「现在哪一侧在接流量」。空槽位表示回到单槽形态（写 NULL）。
func (s *Store) SetServingSlot(ctx context.Context, applicationID string, slot domain.Slot, now time.Time) error {
	if slot != "" && !slot.Valid() {
		return domain.NewError(v1.CodeInvalidRequest, "slot 取值非法: %q", slot)
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE applications SET serving_slot = ?, updated_at = ? WHERE id = ?`,
		nullString(string(slot)), formatTime(now), applicationID)
	if err != nil {
		return err
	}
	// 影响 0 行说明这个应用不存在——静默成功会让「切流记到了一个不存在的应用上」变成
	// 一件查不出来的事。
	if affected, err := result.RowsAffected(); err == nil && affected == 0 {
		return domain.NewError(v1.CodeApplicationNotFound, "应用 %q 不存在", applicationID)
	}
	return nil
}

// ServingSlot 读回「现在哪一侧在接流量」。没记录过（单槽形态）时返回空串。
func (s *Store) ServingSlot(ctx context.Context, applicationID string) (domain.Slot, error) {
	var slot sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT serving_slot FROM applications WHERE id = ?`, applicationID).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return "", domain.NewError(v1.CodeApplicationNotFound, "应用 %q 不存在", applicationID)
	}
	if err != nil {
		return "", err
	}
	return domain.Slot(slot.String), nil
}

// ==== 时间线（迭代 4c）====

// AppendSlotEvent 追加一条槽位时间线事件。
//
// 只追加、不更新：时间线的价值恰恰在于它**不会被后来的动作改写**。
func (s *Store) AppendSlotEvent(ctx context.Context, event *domain.SlotEvent) error {
	if !event.Slot.Valid() {
		return domain.NewError(v1.CodeInvalidRequest, "slot 取值非法: %q", event.Slot)
	}
	if !event.Kind.Valid() {
		return domain.NewError(v1.CodeInvalidRequest, "槽位事件种类取值非法: %q", event.Kind)
	}
	if _, err := s.GetApplication(ctx, event.ApplicationID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO slot_events
			(application_id, slot, release_id, version, kind, detail, operation_id, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ApplicationID, string(event.Slot), nullString(event.ReleaseID), nullString(event.Version),
		string(event.Kind), nullString(event.Detail), nullString(event.OperationID), formatTime(event.At))
	return err
}

// SlotEvents 按**时间倒序**读某个应用的时间线（limit ≤ 0 时取默认条数）。
//
// 倒序是刻意的：问「最近这次是什么时候切的」远比问「第一次是什么时候」常见，
// 而调用方要正序自己反转一下就是了，反过来（默认正序、想倒序要读全表）做不到。
func (s *Store) SlotEvents(ctx context.Context, applicationID string, limit int) ([]domain.SlotEvent, error) {
	if limit <= 0 {
		limit = defaultSlotEventLimit
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, application_id, slot, release_id, version, kind, detail, operation_id, at
		FROM slot_events WHERE application_id = ?
		ORDER BY at DESC, id DESC LIMIT ?`, applicationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []domain.SlotEvent
	for rows.Next() {
		var (
			event       domain.SlotEvent
			slotName    string
			releaseID   sql.NullString
			version     sql.NullString
			kind        string
			detail      sql.NullString
			operationID sql.NullString
			at          string
		)
		if err := rows.Scan(&event.ID, &event.ApplicationID, &slotName, &releaseID, &version,
			&kind, &detail, &operationID, &at); err != nil {
			return nil, err
		}
		event.Slot = domain.Slot(slotName)
		event.ReleaseID = releaseID.String
		event.Version = version.String
		event.Kind = domain.SlotEventKind(kind)
		event.Detail = detail.String
		event.OperationID = operationID.String
		if event.At, err = parseTime(at); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// defaultSlotEventLimit 是 `app slot history` 不带 --limit 时的条数。
// 时间线是给人看的，几十行足够回答「最近发生过什么」。
const defaultSlotEventLimit = 50

// ApplicationsWithSlots 返回**带蓝绿痕迹**的应用 ID：有 serving_slot、有槽位行，
// 或某个 release 记着槽位。
//
// 对账只扫这一批。判据是「我们写过槽位相关的任何一列」——受管 Nginx 配置是**我们自己**
// 写下去的，因此线上有它的应用必然在库里留下过痕迹；没有痕迹的应用去问 Nginx 只是
// 白起一个进程（而且会把「这个应用不是蓝绿」和「这台主机没有 nginx」两件事混在一起）。
func (s *Store) ApplicationsWithSlots(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM applications WHERE serving_slot IS NOT NULL
		UNION
		SELECT application_id FROM application_slots
		UNION
		SELECT application_id FROM releases WHERE slot IS NOT NULL
		ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

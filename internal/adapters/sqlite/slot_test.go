package sqlite

import (
	"testing"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// 迭代 4a：槽位在数据层的行为。
//
// 这一组用例护的是「三件事分开存」之后的读写正确性，尤其是两个容易写错的地方：
// ① NULL 与空串在读写两侧的含义必须一致（单槽形态 = NULL，不是 ''）；
// ② 幂等的 upsert 不能把「上一次切流的时间」抹掉。

// TestReleaseSlotRoundTrip：`releases.slot` 写进去能原样读回来，且**空槽位 = NULL**。
func TestReleaseSlotRoundTrip(t *testing.T) {
	store, ctx := newSpecStore(t)
	appID := seedApplication(t, store, ctx, "orders-api")
	artifactID := seedArtifact(t, store, ctx)
	now := time.Now().UTC()

	if _, err := store.CreateRelease(ctx, &domain.Release{
		ID: "rel_1", ApplicationID: appID, ArtifactID: artifactID,
		Version: "1.0.0", CreatedAt: now, CreatedBy: "tester",
	}); err != nil {
		t.Fatalf("CreateRelease: %v", err)
	}

	// 刚建出来的行是**单槽形态**：slot 为 NULL，读回来是空串。
	fresh, err := store.GetRelease(ctx, "rel_1")
	if err != nil {
		t.Fatalf("GetRelease: %v", err)
	}
	if fresh.Slot != "" {
		t.Fatalf("新建的 release 应当是单槽形态（空 slot），got %q", fresh.Slot)
	}

	if err := store.SetReleaseSlot(ctx, "rel_1", domain.SlotGreen); err != nil {
		t.Fatalf("SetReleaseSlot: %v", err)
	}
	stored, err := store.GetRelease(ctx, "rel_1")
	if err != nil {
		t.Fatalf("GetRelease: %v", err)
	}
	if stored.Slot != domain.SlotGreen {
		t.Fatalf("want green, got %q", stored.Slot)
	}

	// 列清单是共享的（releaseColumns），因此列表路径也必须带回 slot：
	// 「一处加了列、另一处忘了」正是这种共享清单要防的事。
	releases, err := store.ListReleases(ctx, appID, 10)
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	if len(releases) != 1 || releases[0].Slot != domain.SlotGreen {
		t.Fatalf("列表路径也要带回 slot，got %+v", releases)
	}

	// 写回空串 = 回到单槽形态（NULL），不是空字符串。
	if err := store.SetReleaseSlot(ctx, "rel_1", ""); err != nil {
		t.Fatalf("SetReleaseSlot(空): %v", err)
	}
	var raw *string
	if err := store.db.QueryRowContext(ctx, `SELECT slot FROM releases WHERE id = 'rel_1'`).Scan(&raw); err != nil {
		t.Fatalf("直读 slot 列: %v", err)
	}
	if raw != nil {
		t.Fatalf("空槽位必须写成 NULL，got %q", *raw)
	}

	if err := store.SetReleaseSlot(ctx, "rel_1", domain.Slot("purple")); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("非法槽位应当被拒，got %v", err)
	}
}

// TestApplicationSlotUpsertKeepsSwitchedAt：同一个槽位反复写入只保留一行，
// 而且「没有切流的那几次写入」不该抹掉上一次切流的时间。
func TestApplicationSlotUpsertKeepsSwitchedAt(t *testing.T) {
	store, ctx := newSpecStore(t)
	appID := seedApplication(t, store, ctx, "orders-api")
	now := time.Now().UTC()
	switched := now.Add(-10 * time.Minute)

	// 第一次：流量切到 blue。
	if err := store.PutApplicationSlot(ctx, appID, domain.SlotBlue, "rel_1",
		domain.SlotServing, &switched, now); err != nil {
		t.Fatalf("PutApplicationSlot: %v", err)
	}
	// 第二次：同一个槽位又动了一次，但**没有切流**（switchedAt = nil）。
	if err := store.PutApplicationSlot(ctx, appID, domain.SlotBlue, "rel_2",
		domain.SlotServing, nil, now.Add(time.Minute)); err != nil {
		t.Fatalf("PutApplicationSlot: %v", err)
	}

	slots, err := store.ApplicationSlots(ctx, appID)
	if err != nil {
		t.Fatalf("ApplicationSlots: %v", err)
	}
	if len(slots) != 1 {
		t.Fatalf("同一个槽位只该有一行，got %d 行: %+v", len(slots), slots)
	}
	if slots[0].ReleaseID != "rel_2" || slots[0].State != domain.SlotServing {
		t.Fatalf("第二次写入应当覆盖前一次，got %+v", slots[0])
	}
	if slots[0].SwitchedAt == nil || !slots[0].SwitchedAt.Equal(switched) {
		t.Fatalf("switchedAt 为 nil 时应当保留上一次切流的时间，got %v", slots[0].SwitchedAt)
	}

	// 两个槽位各一行，blue 在 green 前（domain.Slots 的固定顺序）。
	if err := store.PutApplicationSlot(ctx, appID, domain.SlotGreen, "rel_3",
		domain.SlotStandby, nil, now); err != nil {
		t.Fatalf("PutApplicationSlot(green): %v", err)
	}
	slots, err = store.ApplicationSlots(ctx, appID)
	if err != nil {
		t.Fatalf("ApplicationSlots: %v", err)
	}
	if len(slots) != 2 || slots[0].Slot != domain.SlotBlue || slots[1].Slot != domain.SlotGreen {
		t.Fatalf("want [blue green]，got %+v", slots)
	}
	// 没写过 switched_at 的那一侧就是 nil——「没切过流」与「切流时间是零值」是两件事。
	if slots[1].SwitchedAt != nil {
		t.Fatalf("green 从没切过流量，SwitchedAt 应当为 nil，got %v", slots[1].SwitchedAt)
	}

	// 非法输入一律拒绝：写进去之后读不出来的状态，比报错更糟。
	if err := store.PutApplicationSlot(ctx, appID, domain.Slot("purple"), "rel_1",
		domain.SlotServing, nil, now); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("非法槽位应当被拒，got %v", err)
	}
	if err := store.PutApplicationSlot(ctx, appID, domain.SlotBlue, "rel_1",
		domain.SlotState("flying"), nil, now); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("非法状态应当被拒，got %v", err)
	}
	// 不存在的应用：外键会拒，但我们要的是**明确的** APPLICATION_NOT_FOUND。
	if err := store.PutApplicationSlot(ctx, "app_nope", domain.SlotBlue, "rel_1",
		domain.SlotServing, nil, now); domain.CodeOf(err) != v1.CodeApplicationNotFound {
		t.Fatalf("不存在的应用应当报 APPLICATION_NOT_FOUND，got %v", err)
	}
}

// TestServingSlotRoundTrip：`applications.serving_slot` 的读写，含「没记录过」与「写回单槽」。
func TestServingSlotRoundTrip(t *testing.T) {
	store, ctx := newSpecStore(t)
	appID := seedApplication(t, store, ctx, "orders-api")
	now := time.Now().UTC()

	// 没记录过 = 空串（单槽形态）。
	slot, err := store.ServingSlot(ctx, appID)
	if err != nil {
		t.Fatalf("ServingSlot: %v", err)
	}
	if slot != "" {
		t.Fatalf("没切过流时应当是空串，got %q", slot)
	}

	if err := store.SetServingSlot(ctx, appID, domain.SlotBlue, now); err != nil {
		t.Fatalf("SetServingSlot: %v", err)
	}
	if slot, err = store.ServingSlot(ctx, appID); err != nil || slot != domain.SlotBlue {
		t.Fatalf("want blue, got %q err=%v", slot, err)
	}

	// 写回空串 = 回到单槽形态：列是 NULL，而不是空字符串。
	if err := store.SetServingSlot(ctx, appID, "", now); err != nil {
		t.Fatalf("SetServingSlot(空): %v", err)
	}
	var raw *string
	if err := store.db.QueryRowContext(ctx,
		`SELECT serving_slot FROM applications WHERE id = ?`, appID).Scan(&raw); err != nil {
		t.Fatalf("直读 serving_slot: %v", err)
	}
	if raw != nil {
		t.Fatalf("回到单槽形态必须写成 NULL，got %q", *raw)
	}

	// 不存在的应用要报错，而不是静默成功。
	if err := store.SetServingSlot(ctx, "app_nope", domain.SlotBlue, now); domain.CodeOf(err) != v1.CodeApplicationNotFound {
		t.Fatalf("不存在的应用应当报 APPLICATION_NOT_FOUND，got %v", err)
	}
}

// ==== 迭代 4c：槽位时间线 ====

// 时间线按「新的在前」读，且每个字段都原样往返（尤其 version 与 operation_id：
// 它们是「这条事件是谁、什么时候、把哪一版切到哪一侧」的全部依据）。
func TestSlotEventsRoundTripNewestFirst(t *testing.T) {
	store, ctx := newSpecStore(t)
	appID := seedApplication(t, store, ctx, "orders-api")
	artifactID := seedArtifact(t, store, ctx)
	base := time.Now().UTC()

	if _, err := store.CreateRelease(ctx, &domain.Release{
		ID: "rel_1", ApplicationID: appID, ArtifactID: artifactID,
		Version: "1.0.0", CreatedAt: base, CreatedBy: "tester",
	}); err != nil {
		t.Fatalf("CreateRelease: %v", err)
	}

	events := []*domain.SlotEvent{
		{ApplicationID: appID, Slot: domain.SlotBlue, ReleaseID: "rel_1", Version: "1.0.0",
			Kind: domain.SlotEventSwitched, Detail: "流量切到这一侧（部署）",
			OperationID: "op_1", At: base.Add(time.Second)},
		{ApplicationID: appID, Slot: domain.SlotBlue, ReleaseID: "rel_1", Version: "1.0.0",
			Kind: domain.SlotEventObserved, Detail: "观察窗口通过：150 次采样，窗口 30 秒",
			OperationID: "op_1", At: base.Add(2 * time.Second)},
		{ApplicationID: appID, Slot: domain.SlotGreen, Kind: domain.SlotEventReconciled,
			Detail: "对账：按线上事实把状态从 serving 改成 standby", At: base.Add(3 * time.Second)},
	}
	for _, event := range events {
		if err := store.AppendSlotEvent(ctx, event); err != nil {
			t.Fatalf("AppendSlotEvent: %v", err)
		}
	}

	got, err := store.SlotEvents(ctx, appID, 0)
	if err != nil {
		t.Fatalf("SlotEvents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d", len(got))
	}
	if got[0].Kind != domain.SlotEventReconciled || got[2].Kind != domain.SlotEventSwitched {
		t.Fatalf("应当按时间倒序（新的在前），got %+v", got)
	}
	if got[2].Version != "1.0.0" || got[2].OperationID != "op_1" || got[2].Slot != domain.SlotBlue {
		t.Fatalf("字段没有原样往返：%+v", got[2])
	}
	// 没有 release 的事件（对账产生的）读回来仍是空串，而不是被写成别的什么。
	if got[0].ReleaseID != "" || got[0].Version != "" || got[0].OperationID != "" {
		t.Fatalf("可空字段应当原样是空，got %+v", got[0])
	}
	if !got[0].At.Equal(events[2].At) {
		t.Fatalf("时刻往返不一致：want %s got %s", events[2].At, got[0].At)
	}
	// 只读某个应用的事件：别的应用不该混进来。
	other := seedApplication(t, store, ctx, "billing-api")
	if events, err := store.SlotEvents(ctx, other, 0); err != nil || len(events) != 0 {
		t.Fatalf("别的应用应当没有事件，got %d err=%v", len(events), err)
	}
}

// limit 生效，且 limit ≤ 0 取默认条数（而不是「一条都不返回」）。
func TestSlotEventsLimit(t *testing.T) {
	store, ctx := newSpecStore(t)
	appID := seedApplication(t, store, ctx, "orders-api")
	base := time.Now().UTC()
	for i := 0; i < 5; i++ {
		if err := store.AppendSlotEvent(ctx, &domain.SlotEvent{
			ApplicationID: appID, Slot: domain.SlotBlue, Kind: domain.SlotEventReconciled,
			Detail: "第 " + string(rune('A'+i)) + " 条", At: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("AppendSlotEvent: %v", err)
		}
	}

	got, err := store.SlotEvents(ctx, appID, 2)
	if err != nil {
		t.Fatalf("SlotEvents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit 应当生效，got %d", len(got))
	}
	if got[0].Detail != "第 E 条" {
		t.Fatalf("取的应当是最新的两条，got %+v", got)
	}
	if _, err := store.SlotEvents(ctx, appID, -1); err != nil {
		t.Fatalf("limit ≤ 0 应当取默认条数而不是报错：%v", err)
	}
}

// 非法输入要被拒绝，而不是写进去一行读不出来的东西。
func TestSlotEventRejectsBadInput(t *testing.T) {
	store, ctx := newSpecStore(t)
	appID := seedApplication(t, store, ctx, "orders-api")
	now := time.Now().UTC()

	if err := store.AppendSlotEvent(ctx, &domain.SlotEvent{
		ApplicationID: appID, Slot: "purple", Kind: domain.SlotEventSwitched, At: now,
	}); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("非法槽位应当报 INVALID_REQUEST，got %v", err)
	}
	if err := store.AppendSlotEvent(ctx, &domain.SlotEvent{
		ApplicationID: appID, Slot: domain.SlotBlue, Kind: "unknown-kind", At: now,
	}); domain.CodeOf(err) != v1.CodeInvalidRequest {
		t.Fatalf("非法事件种类应当报 INVALID_REQUEST，got %v", err)
	}
	if err := store.AppendSlotEvent(ctx, &domain.SlotEvent{
		ApplicationID: "app_nope", Slot: domain.SlotBlue, Kind: domain.SlotEventSwitched, At: now,
	}); domain.CodeOf(err) != v1.CodeApplicationNotFound {
		t.Fatalf("不存在的应用应当报 APPLICATION_NOT_FOUND，got %v", err)
	}
}

// 对账要扫的那一批：**带蓝绿痕迹**的应用。三条痕迹各构造一个，另加一个干净的。
func TestApplicationsWithSlots(t *testing.T) {
	store, ctx := newSpecStore(t)
	artifactID := seedArtifact(t, store, ctx)
	now := time.Now().UTC()

	serving := seedApplication(t, store, ctx, "serving-app")
	rowed := seedApplication(t, store, ctx, "rowed-app")
	released := seedApplication(t, store, ctx, "released-app")
	clean := seedApplication(t, store, ctx, "clean-app")

	if err := store.SetServingSlot(ctx, serving, domain.SlotBlue, now); err != nil {
		t.Fatalf("SetServingSlot: %v", err)
	}
	if err := store.PutApplicationSlot(ctx, rowed, domain.SlotGreen, "", domain.SlotStopped, nil, now); err != nil {
		t.Fatalf("PutApplicationSlot: %v", err)
	}
	if _, err := store.CreateRelease(ctx, &domain.Release{
		ID: "rel_1", ApplicationID: released, ArtifactID: artifactID,
		Version: "1.0.0", CreatedAt: now, CreatedBy: "tester",
	}); err != nil {
		t.Fatalf("CreateRelease: %v", err)
	}
	if err := store.SetReleaseSlot(ctx, "rel_1", domain.SlotGreen); err != nil {
		t.Fatalf("SetReleaseSlot: %v", err)
	}

	ids, err := store.ApplicationsWithSlots(ctx)
	if err != nil {
		t.Fatalf("ApplicationsWithSlots: %v", err)
	}
	want := map[string]bool{serving: true, rowed: true, released: true}
	if len(ids) != len(want) {
		t.Fatalf("want %d 个应用，got %d：%v", len(want), len(ids), ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("多出了不该被扫的应用 %s（干净的应用不该参与对账）", id)
		}
	}
	if want[clean] {
		t.Fatal("干净的应用不该参与对账")
	}
}

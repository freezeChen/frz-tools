-- 迭代 4c：槽位的时间线。
--
-- `application_slots` 只存「这一侧**现在**什么状态」，每次动作都被覆盖写。而
-- 「什么时候切到过哪一版」是**历史**，覆盖写天然回答不了——`app slot history` 要的
-- 正是这个。两张表的生命力不同，因此分开：一张是运营视图，一张是事件流。
--
-- 兼容性：
--   · 新表，没有回填也没有改动既有列；
--   · 存量应用没有事件行——它们本来就没有槽位可言，`slot history` 返回空列表而不是错误；
--   · 与 0010 同一条理由，刻意**不加** kind 的 CHECK 约束：SQLite 改不了约束，
--     种类由领域层枚举与 `domain.SlotEventKind.Valid()` 守着。

CREATE TABLE slot_events (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    application_id TEXT NOT NULL REFERENCES applications (id),
    -- 事件发生在哪一侧（blue / green）。
    slot           TEXT NOT NULL,
    -- 这一侧当时跑的是哪个 release（可为空：对账时可能只认得槽位、不认得版本）。
    release_id     TEXT,
    version        TEXT,
    -- switched | observed | failed | stopped | reconciled（见 domain.SlotEventKind）。
    kind           TEXT NOT NULL,
    -- 人类可读的补充说明（错误码与原因、采样次数……）。
    detail         TEXT,
    -- 触发这次事件的 Operation（对账产生的事件为空）。
    operation_id   TEXT,
    at             TEXT NOT NULL
);

-- 时间线永远是「按应用、按时间倒序」读的，这条索引正好覆盖它。
CREATE INDEX ix_slot_events_app ON slot_events (application_id, at);

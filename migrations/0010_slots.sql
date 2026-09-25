-- 迭代 4a：槽位（slot）进入数据层。
--
-- 迭代 3 的部署是**单槽**的：一个应用一个 unit，`releases/current` 指向当前版本。
-- 迭代 4 的蓝绿要的是两个槽位各自运行、由 Nginx 决定哪一侧接流量，因此需要三件事：
-- ① 每次部署落在哪个槽位；② 现在哪一侧在接流量；③ 每个槽位自己的运营状态。
--
-- 兼容性：
--   · 两处 ADD COLUMN 都允许 NULL，NULL 就是**单槽形态**的既有语义——迭代 3 的流程
--     一行都不写这两列，因此「同一个库上同时跑单槽与蓝绿两种形态的应用」是允许的；
--   · application_slots 是空表起步，没有回填：存量应用本来就没有槽位可言；
--   · 刻意**不加** status/slot 的 CHECK 约束：SQLite 改不了约束，这张表从 0002 起就没有
--     约束，状态机由领域层的枚举与仓储里带条件的 UPDATE 守着（与 0009 同一条理由）。

-- 这次部署落在哪个槽位（blue/green）。NULL = 单槽形态。
ALTER TABLE releases ADD COLUMN slot TEXT;

-- 现在哪一侧在接流量。NULL = 单槽形态。
--
-- **它是运营视图，不是线上事实**：真正决定请求去哪边的是 Nginx 的 upstream。
-- 对账时以 Nginx 的配置为准，这一列是它的镜像（迭代 4 规格 D2）。
ALTER TABLE applications ADD COLUMN serving_slot TEXT;

-- 每个槽位的运营状态。一个应用最多两行（blue 与 green）。
CREATE TABLE application_slots (
    application_id TEXT NOT NULL REFERENCES applications (id),
    slot           TEXT NOT NULL,
    -- 这个槽位当前跑着的 release。空串表示还没部署过。
    release_id     TEXT,
    -- serving | standby | draining | stopped | failed（见 domain.SlotState）。
    state          TEXT NOT NULL,
    -- **流量**最近一次切到这一侧的时刻（不是部署时刻）。
    switched_at    TEXT,
    updated_at     TEXT NOT NULL,
    PRIMARY KEY (application_id, slot)
);

-- 按应用查「另一侧是谁」是切流的必经查询，索引覆盖它。
CREATE INDEX ix_application_slots_slot ON application_slots (slot);

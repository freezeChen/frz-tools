-- 迭代 3b：release 从「一条记录」变成「一次部署」，并把应用规格按 release 版本化。
--
-- 1a 建 releases 时只把它当「某个应用采用了某个制品」的记录，注释里写着「slot、active 标记、
-- 部署状态要等迭代 3/4 有真实语义时再加」——现在就是那个时候。
--
-- 兼容性：
--   · ADD COLUMN 带默认值，既有行不受影响（status 默认 created = 「记录已建、部署未尝试」，
--     对 1a 时期写下的那些行来说是准确的说法）。
--   · 刻意**不加** status 的 CHECK 约束：那需要重建整张表（SQLite 改不了约束），而这张表在
--     1a 就没有约束。状态机由领域层的枚举与仓储里带 status 条件的 UPDATE 守着，见
--     internal/domain/release.go 与 sqlite/catalog.go。
--
--   · application_specs 从「应用一行」改成「应用 + release 一行」（1c 第 15 节决定 3：
--     否则回滚只回二进制不回配置，语义不完整）。SQLite 改不了主键，因此走标准的重建流程；
--     既有行迁移为 release_id = NULL，表示「应用级当前规格」，语义与迁移前一致。

ALTER TABLE releases ADD COLUMN status TEXT NOT NULL DEFAULT 'created';
ALTER TABLE releases ADD COLUMN directory TEXT;
ALTER TABLE releases ADD COLUMN activated_at TEXT;
ALTER TABLE releases ADD COLUMN finished_at TEXT;
ALTER TABLE releases ADD COLUMN error_code TEXT;
ALTER TABLE releases ADD COLUMN error_message TEXT;

-- 按应用查「当前激活的是哪个」是部署与回滚的必经查询，索引覆盖它。
CREATE INDEX ix_releases_application_status ON releases (application_id, status);

CREATE TABLE application_specs_new (
    application_id TEXT NOT NULL REFERENCES applications (id),
    -- release_id 为 NULL 表示「应用级当前规格」：迁移前的老行、以及还没有过 release 的应用。
    release_id     TEXT,
    spec_json      TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    updated_by     TEXT,
    PRIMARY KEY (application_id, release_id)
);

-- 关键的一条：SQLite 认为 NULL **互不相等**，因此「应用级」那一行（release_id IS NULL）
-- 不受主键约束，`ON CONFLICT (application_id, release_id)` 也就永远不触发——结果是同一个
-- 应用每提交一次规格就多一行，而读的时候只取第一行，看起来一切正常。偏索引把「每个应用的
-- 应用级规格只有一行」钉死，upsert 也就能以它为冲突目标（见 sqlite/spec.go）。
CREATE UNIQUE INDEX ux_application_specs_app_level
    ON application_specs_new (application_id) WHERE release_id IS NULL;

INSERT INTO application_specs_new (application_id, release_id, spec_json, updated_at, updated_by)
    SELECT application_id, NULL, spec_json, updated_at, updated_by FROM application_specs;

DROP TABLE application_specs;

ALTER TABLE application_specs_new RENAME TO application_specs;

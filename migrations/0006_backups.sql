-- 迭代 2a：备份策略与备份记录。
--
-- 全是新建表，不动任何既有表：升级与回滚都不影响迭代 0/1 的数据。
--
-- 备份的内容本身存在**独立的存储根**（配置项 `backupStore.root`），不在这两张表里，
-- 也不与制品共用 blob 目录——1a 的制品 GC 把「digest 不在 artifacts 表里」一律当作
-- 孤儿删除（internal/application/artifact.go 的 Collect），共用根会让 artifact gc
-- 删掉全部备份。这条是结构性要求，见 2a 规格的 D5。

CREATE TABLE backup_policies (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    policy_json TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    updated_by  TEXT
);

CREATE TABLE backups (
    id                TEXT PRIMARY KEY,
    policy_id         TEXT NOT NULL REFERENCES backup_policies (id),
    -- 发起这次备份的 Operation。备份走任务引擎，因此它的进度、日志、取消、重试
    -- 全部由既有机制负责，这里只留一条指针。
    operation_id      TEXT REFERENCES operations (id),
    status            TEXT NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'pruned')),
    -- 内容寻址的 digest。只有 succeeded 的行才有值。
    storage_digest    TEXT,
    -- logical_bytes 是压缩前的逻辑流字节数，stored_bytes 是落盘字节数；
    -- 两个都留，才能回答「压缩比是多少」与「占了多少盘」这两个不同的问题。
    logical_bytes     INTEGER,
    stored_bytes      INTEGER,
    compression       TEXT,
    -- 加密密钥的标识。不存密钥本身，只存「用的是哪把」——
    -- 将来轮换密钥时，这个字段是唯一能说明「这个备份还能不能用新钥匙打开」的依据。
    encryption_key_id TEXT,
    resource_kind     TEXT NOT NULL,
    server_version    TEXT,
    client_version    TEXT,
    tool              TEXT,
    -- GFS 标签等保留策略需要的标记。
    labels_json       TEXT,
    started_at        TEXT NOT NULL,
    finished_at       TEXT,
    verified_at       TEXT,
    verified_ok       INTEGER,
    error_code        TEXT,
    error_message     TEXT
);

-- 保留策略按「某个策略 + 完成时间倒序」取，索引覆盖这一段。
CREATE INDEX ix_backups_policy_finished ON backups (policy_id, finished_at DESC);

-- 「这个 digest 有没有备份元数据」是删除前的检查，也是备份侧孤儿回收的依据。
CREATE INDEX ix_backups_digest ON backups (storage_digest) WHERE storage_digest IS NOT NULL;

-- 按 Operation 反查备份：排查「这个操作到底备了什么」时走这条。
CREATE INDEX ix_backups_operation ON backups (operation_id) WHERE operation_id IS NOT NULL;

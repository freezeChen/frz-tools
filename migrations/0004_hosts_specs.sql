-- 每个应用一份「当前」规格：提交 manifest 是覆盖，不是追加。迭代 3 若要让规格随
-- release 版本化，需要改成 (release_id, spec_json) 并提升 apiVersion——现在刻意不加
-- 一个永远为 NULL 的 release_id，那种列会让人误以为已经支持版本化。
CREATE TABLE application_specs (
    application_id TEXT PRIMARY KEY REFERENCES applications (id),
    spec_json      TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    updated_by     TEXT
);

-- address 为空表示本机，非空留给迭代 5 的远程主机；1c 只把这两个表当身份与标签用，
-- 不承载连接语义，因此也不加任何引用它们的外键。
CREATE TABLE hosts (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    address     TEXT NOT NULL DEFAULT '',
    labels_json TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE environments (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    labels_json TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

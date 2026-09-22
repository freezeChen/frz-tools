CREATE TABLE artifacts (
    id         TEXT PRIMARY KEY,
    digest     TEXT NOT NULL UNIQUE,
    size       INTEGER NOT NULL,
    media_type TEXT NOT NULL DEFAULT '',
    name       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    created_by TEXT,
    deleted_at TEXT
);

CREATE INDEX ix_artifacts_created ON artifacts (created_at, id);

CREATE TABLE applications (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    labels_json TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE releases (
    id             TEXT PRIMARY KEY,
    application_id TEXT NOT NULL REFERENCES applications (id),
    artifact_id    TEXT NOT NULL REFERENCES artifacts (id),
    version        TEXT NOT NULL,
    labels_json    TEXT,
    created_at     TEXT NOT NULL,
    created_by     TEXT
);

CREATE UNIQUE INDEX ux_releases_application_version
    ON releases (application_id, version);

CREATE INDEX ix_releases_application ON releases (application_id, created_at);
CREATE INDEX ix_releases_artifact ON releases (artifact_id);

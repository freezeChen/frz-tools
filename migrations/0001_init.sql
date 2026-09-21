CREATE TABLE operations (
    id              TEXT PRIMARY KEY,
    kind            TEXT NOT NULL,
    resource        TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'cancelled', 'rolled_back')),
    phase           TEXT NOT NULL DEFAULT '',
    dry_run         INTEGER NOT NULL DEFAULT 0 CHECK (dry_run IN (0, 1)),
    idempotency_key TEXT,
    request_hash    TEXT NOT NULL,
    retry_of        TEXT REFERENCES operations (id),
    exit_code       INTEGER,
    error_code      TEXT,
    error_message   TEXT,
    spec_json       TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    started_at      TEXT,
    finished_at     TEXT,
    created_by      TEXT
);

CREATE UNIQUE INDEX ux_operations_idempotency_key
    ON operations (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE INDEX ix_operations_status_created
    ON operations (status, created_at);

CREATE INDEX ix_operations_resource
    ON operations (resource, created_at);

CREATE TABLE operation_logs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    operation_id TEXT NOT NULL REFERENCES operations (id) ON DELETE CASCADE,
    level        TEXT NOT NULL,
    ts           TEXT NOT NULL,
    phase        TEXT,
    message      TEXT NOT NULL,
    fields_json  TEXT
);

CREATE INDEX ix_operation_logs_operation
    ON operation_logs (operation_id, id);

CREATE TABLE resource_locks (
    resource           TEXT PRIMARY KEY,
    owner_operation_id TEXT NOT NULL REFERENCES operations (id),
    acquired_at        TEXT NOT NULL,
    released_at        TEXT
);

CREATE INDEX ix_resource_locks_owner
    ON resource_locks (owner_operation_id);

CREATE TABLE audit_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type   TEXT NOT NULL,
    actor        TEXT,
    operation_id TEXT REFERENCES operations (id),
    resource     TEXT,
    result       TEXT,
    ts           TEXT NOT NULL,
    details_json TEXT
);

CREATE INDEX ix_audit_events_operation
    ON audit_events (operation_id, id);

CREATE INDEX ix_audit_events_ts
    ON audit_events (ts);

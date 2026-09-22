CREATE TABLE schedules (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL UNIQUE,
    enabled           INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    kind              TEXT NOT NULL CHECK (kind IN ('cron', 'interval')),
    cron              TEXT,
    interval_seconds  INTEGER,
    timezone          TEXT NOT NULL DEFAULT 'UTC',
    resource          TEXT NOT NULL,
    spec_json         TEXT NOT NULL,
    missed_run_policy TEXT NOT NULL DEFAULT 'skip'
        CHECK (missed_run_policy IN ('skip', 'runOnce')),
    next_run_at       TEXT,
    last_run_at       TEXT,
    last_result       TEXT,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    created_by        TEXT
);

CREATE INDEX ix_schedules_enabled ON schedules (enabled, next_run_at);

CREATE TABLE schedule_runs (
    id            TEXT PRIMARY KEY,
    schedule_id   TEXT NOT NULL REFERENCES schedules (id) ON DELETE CASCADE,
    scheduled_for TEXT NOT NULL,
    started_at    TEXT NOT NULL,
    finished_at   TEXT,
    result        TEXT NOT NULL
        CHECK (result IN ('dispatched', 'missed', 'failed')),
    operation_id  TEXT REFERENCES operations (id),
    error_code    TEXT,
    error_message TEXT
);

-- 这条唯一约束是「同一时刻不重复触发」的最终保证：opsd 重启后重算触发时间、
-- 或多个 worker 并发处理同一条计划，都会在这里被拒绝。
CREATE UNIQUE INDEX ux_schedule_runs_schedule_moment
    ON schedule_runs (schedule_id, scheduled_for);

CREATE INDEX ix_schedule_runs_schedule
    ON schedule_runs (schedule_id, scheduled_for DESC);

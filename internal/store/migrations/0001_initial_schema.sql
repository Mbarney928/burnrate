CREATE TABLE services (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE,
    team        TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);

CREATE TABLE slos (
    id          INTEGER PRIMARY KEY,
    service_id  INTEGER NOT NULL REFERENCES services (id) ON DELETE CASCADE,
    name        TEXT    NOT NULL,
    target      REAL    NOT NULL CHECK (target > 0 AND target < 1),
    window_days INTEGER NOT NULL CHECK (window_days BETWEEN 1 AND 90),
    created_at  INTEGER NOT NULL,
    UNIQUE (service_id, name)
);

-- One row per SLO per minute. Ingestion adds to an existing row, so clients can
-- report the same minute more than once (e.g. from several replicas).
CREATE TABLE event_buckets (
    slo_id INTEGER NOT NULL REFERENCES slos (id) ON DELETE CASCADE,
    minute INTEGER NOT NULL, -- unix seconds, truncated to the minute
    good   INTEGER NOT NULL CHECK (good >= 0),
    total  INTEGER NOT NULL CHECK (total >= good),
    PRIMARY KEY (slo_id, minute)
) WITHOUT ROWID;

CREATE TABLE api_tokens (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    role       TEXT    NOT NULL CHECK (role IN ('viewer', 'editor')),
    token_hash TEXT    NOT NULL UNIQUE, -- SHA-256 of the secret; the secret itself is never stored
    created_at INTEGER NOT NULL
);

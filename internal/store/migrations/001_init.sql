-- 001_init.sql — initial schema for Bugbot's embedded state store.
--
-- All timestamps are stored as RFC3339 strings in UTC (TEXT) so they sort
-- lexicographically and round-trip losslessly through Go's time.Time.

-- findings holds every candidate bug the pipeline has surfaced. Each finding is
-- anchored to a specific code version via commit_sha + file_hash so the daemon
-- can detect when the underlying code has changed and re-verify.
CREATE TABLE findings (
    id          TEXT PRIMARY KEY,
    fingerprint TEXT NOT NULL,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    reasoning   TEXT NOT NULL DEFAULT '',
    severity    TEXT NOT NULL DEFAULT '',
    tier        INTEGER NOT NULL,
    status      TEXT NOT NULL,
    lens        TEXT NOT NULL DEFAULT '',
    file        TEXT NOT NULL DEFAULT '',
    line        INTEGER NOT NULL DEFAULT 0,
    commit_sha  TEXT NOT NULL DEFAULT '',
    file_hash   TEXT NOT NULL DEFAULT '',
    repro_path  TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

-- A finding is uniquely identified by its fingerprint (lens+file+location+title
-- hash). Re-finding the same issue must update the existing row, not insert a
-- duplicate.
CREATE UNIQUE INDEX idx_findings_fingerprint ON findings(fingerprint);
CREATE INDEX idx_findings_status ON findings(status);
CREATE INDEX idx_findings_tier ON findings(tier);
CREATE INDEX idx_findings_commit ON findings(commit_sha);

-- suppressions records fingerprints the maintainers have dismissed. A suppressed
-- fingerprint must never resurface as an open finding. Rows are written when a
-- finding is dismissed and consulted during triage.
CREATE TABLE suppressions (
    fingerprint TEXT PRIMARY KEY,
    reason      TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
);

-- file_state records the scan watermark for each file: the content hash and the
-- commit at which it was last scanned. Used to drive incremental scanning.
CREATE TABLE file_state (
    path                 TEXT PRIMARY KEY,
    content_hash         TEXT NOT NULL,
    last_scanned_commit  TEXT NOT NULL DEFAULT '',
    last_scanned_at      TEXT NOT NULL
);

-- scan_runs records each scan invocation for auditing and to scope spend.
CREATE TABLE scan_runs (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    commit_sha  TEXT NOT NULL DEFAULT '',
    started_at  TEXT NOT NULL,
    finished_at TEXT,
    stats_json  TEXT NOT NULL DEFAULT ''
);

-- spend is the token-spend ledger. Each row records the tokens consumed by a
-- single LLM call, attributed to a scan run, role, provider, and model. Rolled
-- up for per-cycle and per-day budget checks.
CREATE TABLE spend (
    id            TEXT PRIMARY KEY,
    ts            TEXT NOT NULL,
    scan_run_id   TEXT NOT NULL DEFAULT '',
    role          TEXT NOT NULL DEFAULT '',
    provider      TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_spend_ts ON spend(ts);
CREATE INDEX idx_spend_scan_run ON spend(scan_run_id);

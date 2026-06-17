CREATE TABLE package_summaries (
    pkg         TEXT PRIMARY KEY,
    fingerprint TEXT NOT NULL,
    summary     TEXT NOT NULL,
    model       TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL
);

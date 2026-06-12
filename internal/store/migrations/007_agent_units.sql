-- 007_agent_units.sql — per-unit agent observability table.
--
-- Each row represents one finder, verifier, or reproducer agent execution (or
-- a skipped unit that never launched). This is the empirical feed for learned
-- (lens × strategy × language) yield tables and the "what did the budget never
-- reach" query surface. Skipped units get rows too: zero tokens, empty
-- started_at, and a skipped_* status.
--
-- Status vocabulary (shared across roles):
--   finder:    ok, parse_failed, budget_stopped, skipped_hard_budget, skipped_degraded
--   verifier:  survived, killed, orphaned_budget
--   reproducer: reproduced, exhausted, invalid_plan, infra_error
--
-- CAUTION: Do NOT add ORDER BY on any timestamp column. RFC3339Nano values do
-- not sort consistently as strings across the second boundary (see the warning
-- in filestate.go). Use launch_order or rowid for ordering.

CREATE TABLE agent_units (
    id                TEXT PRIMARY KEY,
    scan_run_id       TEXT NOT NULL,
    role              TEXT NOT NULL,              -- 'finder' | 'verifier' | 'reproducer'
    lens              TEXT NOT NULL DEFAULT '',   -- bare lens name (finder) or candidate's lens (verifier)
    strategy          TEXT NOT NULL DEFAULT '',   -- finder strategy name; '' for other roles
    launch_order      INTEGER NOT NULL DEFAULT 0,
    files_json        TEXT NOT NULL DEFAULT '[]', -- JSON array of the unit's target files
    started_at        TEXT NOT NULL DEFAULT '',   -- '' for units skipped before launch
    finished_at       TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL,              -- see status vocabulary above
    input_tokens      INTEGER NOT NULL DEFAULT 0,
    output_tokens     INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    candidates        INTEGER NOT NULL DEFAULT 0, -- finder: candidates emitted; verifier: 1 if survived else 0
    leads_posted      INTEGER NOT NULL DEFAULT 0,
    detail            TEXT NOT NULL DEFAULT '',   -- role-specific detail (verifier seat/arbiter summary, repro outcome)
    created_at        TEXT NOT NULL
);

CREATE INDEX idx_agent_units_scan_run ON agent_units(scan_run_id);
CREATE INDEX idx_agent_units_lens ON agent_units(lens, strategy);

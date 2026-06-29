-- 016_dead_hypotheses.sql — durable audit trail for verifier-killed candidates.
--
-- A verifier kill is the precision lever's terminal output: the panel (or arbiter
-- on a split) decided the candidate was refuted and dropped it. The dedup table
-- (findings) would poison suppression memory for a kill (every kill is not a
-- real bug), so kills were historically only a Stats counter. Operators had no
-- way to distinguish a good kill (false positive removed) from a bad kill
-- (real bug suppressed) on a production scan.
--
-- This table persists every killed candidate WITH its kill-reasoning trace so
-- the audit feed can be queried post-hoc. Unlike agent_units.Detail (deliberately
-- free-text-free, counts and seat names only), this table IS allowed to hold
-- model-authored prose in reasoning_trace — that is its purpose. The
-- verdict_breakdown columns are structured and queryable; only the trace is free
-- text.
--
-- The verdict_breakdown columns mirror the compact shape agent_units.Detail uses
-- (seats + refuted count + arbiter verdict) so the two tables are consistent.
--
-- Triage drops (counters only, no surviving artifact) and budget-orphaned
-- candidates (already durable as T3 findings) are NOT written here.
--
-- CAUTION: prune by recency of scan_run_id, not by created_at — RFC3339Nano TEXT
-- does not sort lexicographically across the second boundary. See agentunits.go
-- PruneAgentUnits for the same rule.
--
-- CAUTION: the structured verdict breakdown columns (seat_names, refuted_count,
-- total_seats, arbiter_ran, arbiter_refuted, arbiter_verdict) are the QUERYABLE
-- surface; reasoning_trace is the FREE-TEXT audit and is NOT for aggregation.

CREATE TABLE dead_hypotheses (
    id              TEXT PRIMARY KEY,
    scan_run_id     TEXT NOT NULL,
    fingerprint     TEXT NOT NULL,
    lens            TEXT NOT NULL DEFAULT '',
    file            TEXT NOT NULL DEFAULT '',
    line            INTEGER NOT NULL DEFAULT 0,
    title           TEXT NOT NULL DEFAULT '',
    severity        TEXT NOT NULL DEFAULT '',

    -- Structured kill-verdict breakdown. seat_names is a comma-separated list
    -- of refuter seat names (JSON-array not used: seat names are short,
    -- well-controlled, and joining is irrelevant at query time). The
    -- arbiter_* columns are populated only when an arbiter ran.
    seat_names      TEXT NOT NULL DEFAULT '',
    refuted_count   INTEGER NOT NULL DEFAULT 0,
    total_seats     INTEGER NOT NULL DEFAULT 0,
    arbiter_ran     INTEGER NOT NULL DEFAULT 0,
    arbiter_refuted INTEGER NOT NULL DEFAULT 0,
    arbiter_verdict TEXT NOT NULL DEFAULT '',

    -- The buildReasoning trace for this kill. Free text is acceptable here:
    -- this is a deliberately-scoped audit store, not the free-text-free
    -- agent_units.Detail.
    reasoning_trace TEXT NOT NULL DEFAULT '',

    created_at      TEXT NOT NULL
);

CREATE INDEX idx_dead_hypotheses_scan_run ON dead_hypotheses(scan_run_id);
CREATE INDEX idx_dead_hypotheses_fingerprint ON dead_hypotheses(fingerprint);

-- 023_finder_traversals.sql — durable audit trail for finder traversal coverage.
--
-- A finder unit (e.g. contract-trace-deep) can run successfully over the exact
-- chunk holding a bug and emit ZERO candidates. Without a traversal audit trail,
-- there is no record of what the finder enumerated or which sites it visited,
-- making it impossible to distinguish:
--   (a) never-enumerated — the site was not reached,
--   (b) enumerated-not-traced — the site appeared in the scope but was not
--       followed up, and
--   (c) traced-and-dismissed — the finder inspected the site and decided there
--       was no defect.
--
-- This table persists one row per successful (finderOK) finder unit that reports
-- a traversal summary in its output JSON. Finders that omit the traversal field
-- contribute zero rows — no synthetic rows are created. The enumerated_json and
-- visited_json columns are JSON arrays of string site identifiers authored by
-- the model; they are the queryable traversal surface.
--
-- candidate_count records how many candidates the unit reported (possibly zero),
-- which is the primary observability gap: a row with candidate_count=0 but
-- non-empty enumerated_json means the finder visited sites and still found
-- nothing.
--
-- CAUTION: prune by recency of scan_run_id, not by created_at — RFC3339Nano TEXT
-- does not sort lexicographically across the second boundary. See agentunits.go
-- PruneAgentUnits for the same rule.

CREATE TABLE finder_traversals (
    id              TEXT PRIMARY KEY,
    scan_run_id     TEXT NOT NULL,
    lens            TEXT NOT NULL DEFAULT '',
    strategy        TEXT NOT NULL DEFAULT '',

    -- JSON arrays of repo-relative file paths the unit was assigned.
    files_json      TEXT NOT NULL DEFAULT '[]',

    -- JSON arrays of string site identifiers reported by the finder model.
    -- enumerated: sites the finder considered as candidates for inspection.
    -- visited: sites the finder actually traced in detail.
    enumerated_json TEXT NOT NULL DEFAULT '[]',
    visited_json    TEXT NOT NULL DEFAULT '[]',

    -- How many candidates this unit emitted (possibly zero). A row with
    -- candidate_count=0 and non-empty enumerated_json is the primary signal
    -- that the finder reached sites but dismissed them all.
    candidate_count INTEGER NOT NULL DEFAULT 0,

    created_at      TEXT NOT NULL
);

CREATE INDEX idx_finder_traversals_scan_run ON finder_traversals(scan_run_id);

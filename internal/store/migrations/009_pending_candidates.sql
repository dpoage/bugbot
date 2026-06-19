-- 009_pending_candidates.sql — write-ahead log of in-flight finder hypotheses.
--
-- A "candidate" (funnel.Candidate) is a finder-proposed bug that lives between
-- the hypothesize and verify stages. The streaming funnel (bugbot-280) persists
-- findings, per-unit coverage, and the scan_runs row durably, but candidates
-- that have not yet reached a terminal fate (verified / killed / budget-orphaned
-- / triage-dropped) live ONLY in in-memory channels. An interrupt (SIGINT, OOM,
-- crash) discarded them AND their expensive finder work — and because per-unit
-- coverage stamping had already marked their files scanned, the next sweep
-- deprioritized those files, so the hypotheses were lost for good.
--
-- This table is that missing durability point. Each finder-emitted candidate is
-- written here (batched per finder unit) BEFORE entering the channel pipeline,
-- and deleted when it reaches a terminal fate. Whatever survives an interrupt is
-- replayed straight into triage/verify on the next run (daemon or oneshot),
-- skipping re-hypothesize.
--
-- Lifecycle: a clean run empties this table (every row deleted at its terminal
-- fate); only an interrupted run leaves rows, which the next run drains. The
-- replay self-cleans stale rows (a candidate whose file is now out of scope is
-- triage-dropped, which deletes its row), so no recency-based retention prune is
-- needed — unlike agent_units.
--
-- No UNIQUE constraint on the natural key: triage already dedups by fingerprint,
-- duplicate candidates are harmless in the WAL (both replay, triage drops one),
-- and a UNIQUE collision on a same-unit duplicate would break the batch insert.
--
-- CAUTION: Do NOT add ORDER BY on created_at. RFC3339Nano values do not sort
-- consistently as strings across the second boundary (see filestate.go). Order
-- by id (ULID-style, time-ordered) instead.

CREATE TABLE pending_candidates (
    id                   TEXT PRIMARY KEY,
    scan_run_id          TEXT NOT NULL,
    commit_sha           TEXT NOT NULL DEFAULT '',
    lens                 TEXT NOT NULL DEFAULT '',
    file                 TEXT NOT NULL DEFAULT '',
    line                 INTEGER NOT NULL DEFAULT 0,
    title                TEXT NOT NULL DEFAULT '',
    description          TEXT NOT NULL DEFAULT '',
    severity             TEXT NOT NULL DEFAULT '',
    evidence             TEXT NOT NULL DEFAULT '',
    confidence           TEXT NOT NULL DEFAULT '',
    corroborating_lenses TEXT NOT NULL DEFAULT '[]', -- JSON array of lens names
    created_at           TEXT NOT NULL
);

CREATE INDEX idx_pending_candidates_scan_run ON pending_candidates(scan_run_id);

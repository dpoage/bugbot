-- 018_fsm_and_repro_queue.sql
--
-- Two schema changes that support the explicit finding lifecycle FSM (bugbot-34xn):
--
-- 1. needs_human_reason: explicit cause for the NeedsHuman flag, eliminating the
--    flag-combination inference that downstream code required (bugbot-34xn #2).
--    Two distinct causes are now represented:
--      ''               — not set (NeedsHuman is false, or legacy pre-migration row)
--      'prover_exhausted' — patch-prover used up its attempt budget without a fix
--      'below_quorum'   — verifier survivor fell below the genuine-verdict quorum floor
--    The column is NOT NULL with default '' so existing rows get a neutral value.
--
-- 2. repro_attempts: durable queue for reproducer dispatch with claim/skip semantics
--    and bounded infra-error retry (bugbot-34xn #5). All three dispatch paths
--    (in-run hook, daemon backlog drain, `bugbot repro` CLI) consume this table.
--
--    State machine for a repro_attempts row:
--      pending    → running   (claim: UPDATE WHERE state='pending' AND attempt_count < max_attempts)
--      running    → done      (reproduce succeeded or definitively failed)
--      running    → infra_retry (infrastructure error; attempt_count < max_attempts)
--      infra_retry→ running   (next cycle picks it up)
--      running    → abandoned (attempt_count >= max_attempts on infra error)
--
--    The fingerprint column references findings(fingerprint) for integrity; it is
--    NOT a FK constraint (SQLite FK enforcement is opt-in and we follow the existing
--    pattern of no FK constraints in this schema).

ALTER TABLE findings ADD COLUMN needs_human_reason TEXT NOT NULL DEFAULT '';

-- Backfill needs_human_reason for pre-migration rows that have needs_human=1
-- but no reason yet (the column defaulted to ''). Without a backfill, the
-- next UpsertFinding UPDATE for any such row would recover '' → NeedsHumanReasonNone
-- and then fail ValidateFindingState invariant (d): NeedsHuman=true requires a reason.
--
-- Heuristic (mirrors the two causes documented in findings_fsm.go):
--   tier=1 AND repro_path<>'' → prover_exhausted (patch-prover ran after T1 promo)
--   all remaining needs_human=1 rows → below_quorum (verifier quorum floor)
--
-- The two-step UPDATE is safe: the second step uses needs_human_reason='' to
-- catch only rows not already updated by the first step.
UPDATE findings
SET needs_human_reason = 'prover_exhausted'
WHERE needs_human = 1
  AND tier = 1
  AND repro_path IS NOT NULL
  AND repro_path <> ''
  AND needs_human_reason = '';

UPDATE findings
SET needs_human_reason = 'below_quorum'
WHERE needs_human = 1
  AND needs_human_reason = '';

CREATE TABLE repro_attempts (
    id            TEXT PRIMARY KEY,
    fingerprint   TEXT NOT NULL,
    state         TEXT NOT NULL DEFAULT 'pending',  -- pending|running|done|infra_retry|abandoned
    attempt_count INTEGER NOT NULL DEFAULT 0,
    max_attempts  INTEGER NOT NULL DEFAULT 3,
    last_error    TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE UNIQUE INDEX idx_repro_attempts_fingerprint ON repro_attempts(fingerprint);
CREATE INDEX idx_repro_attempts_state ON repro_attempts(state, updated_at);

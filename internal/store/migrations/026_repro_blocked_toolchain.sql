-- 026_repro_blocked_toolchain.sql
--
-- Extends the repro_attempts queue with two bugbot-14g0 fields:
--
-- 1. blocked_ecosystem: the missing-capability ecosystem name (e.g. "js")
--    when a row is in the new 'blocked_toolchain' state. The state itself is
--    just a string in the existing `state` column (no CHECK constraint, per
--    the existing pending|running|done|infra_retry|abandoned vocabulary
--    documented as a comment in 018_fsm_and_repro_queue.sql) — this migration
--    only adds the column that names WHICH capability was missing, so
--    aggregate reporting ("N findings blocked: image lacks node") can group
--    by it without re-deriving the ecosystem from the finding row.
--
--    New state machine edge (see repro_queue.go for the full picture):
--      pending/infra_retry/blocked_toolchain → blocked_toolchain
--        (claim-time capability gate in promote.go's promoteOne fires BEFORE
--         ClaimReproAttempt; no sandbox run, no attempt_count increment)
--      blocked_toolchain → running
--        (the same claim path as pending/infra_retry: once the capability
--         probe cache reports the ecosystem available again — e.g. after an
--         image/config change — ClaimReproAttempt claims it like any other
--         pending row)
--
-- 2. unsandboxed: set to 1 when the attempt that produced this row's current
--    state ran in the fix-C attended escape-hatch (unsandboxed, workspace-copy)
--    mode instead of the container. Lets a T1 promoted unsandboxed be
--    distinguished from a normally-sandboxed one (bugbot-14g0 acceptance 5).

ALTER TABLE repro_attempts ADD COLUMN blocked_ecosystem TEXT NOT NULL DEFAULT '';
ALTER TABLE repro_attempts ADD COLUMN unsandboxed INTEGER NOT NULL DEFAULT 0;

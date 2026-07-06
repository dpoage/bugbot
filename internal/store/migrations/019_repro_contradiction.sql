-- 019_repro_contradiction.sql
--
-- Extends repro_attempts to track exit-zero (bug-did-not-manifest) outcomes
-- as a first-class counter, distinct from infrastructure errors. Two or more
-- exit-zero outcomes signal repro-contradiction: the finding's repro test ran
-- successfully but the bug did not manifest, which is disconfirming evidence.
--
-- Design choice: a counter column on the existing repro_attempts row (rather
-- than a separate attempt-history table) because:
--   1. repro_attempts is already keyed per fingerprint — the join is trivial.
--   2. The signal we need is a count, not a per-attempt history, so a column
--      is the cheapest representation.
--   3. It stays consistent with the existing attempt_count / last_error pattern.
--
-- exit_zero_count is incremented by RecordExitZeroAttempt whenever the
-- reproducer ran (no infra error) but the test exited 0 and the bug did NOT
-- manifest. Infra errors (sandbox failure, agent crash) do NOT increment it;
-- successful repros (Promoted=true) do NOT increment it.

ALTER TABLE repro_attempts ADD COLUMN exit_zero_count INTEGER NOT NULL DEFAULT 0;

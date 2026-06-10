-- 006_finding_fix_patch.sql — adds patch-prover columns to findings.
--
-- fix_patch stores the unified diff text produced by the patch-prover stage
-- when a minimal fix candidate was witnessed by the sandbox.  It is empty
-- when the prover was not run or found no plausible fix.
--
-- needs_human is set when the patch-prover exhausted its attempt budget
-- without finding a minimal fix, signalling that the finding requires manual
-- investigation.  A fix-refusing bug is often a misdiagnosed one, so human
-- review is the appropriate escalation path.
ALTER TABLE findings ADD COLUMN fix_patch    TEXT    NOT NULL DEFAULT '';
ALTER TABLE findings ADD COLUMN needs_human  INTEGER NOT NULL DEFAULT 0;

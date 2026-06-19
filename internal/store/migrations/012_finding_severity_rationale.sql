-- 012_finding_severity_rationale.sql — impact-assessment sweep output.
--
-- verdict_detail captures the human-readable rationale produced by the
-- impact-assessment sweep (Stage F) that re-ranks each finding's severity by
-- reachability. Examples: "zero non-test callers of processBuffer found
-- repo-wide by AST reference scan", "platform-only path not compiled on Linux
-- target". The column is NOT NULL DEFAULT '' so existing rows are unaffected
-- and the sweep writes '' for findings that were not re-ranked.
ALTER TABLE findings ADD COLUMN verdict_detail TEXT NOT NULL DEFAULT '';

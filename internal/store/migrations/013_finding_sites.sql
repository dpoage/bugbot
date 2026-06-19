-- 013_finding_sites.sql — record all code locations for a merged finding.
--
-- When triage's same-root-cause merge collapses multiple sites of the same
-- defect pattern into one finding (e.g. several UTF-8 truncation calls in the
-- same file, or a declaration+definition pair across a .cpp/.hpp pair), every
-- contributing (file, line) location is recorded here. Sites[0] is the primary's
-- own location; additional entries come from the merged-away members.
-- Stored as a newline-separated "file|line" list (pipe within file names is
-- escaped as U+FFFD). Empty string when no root-cause merge occurred.
ALTER TABLE findings ADD COLUMN sites TEXT NOT NULL DEFAULT '';

-- bugbot-6gfy.2: tracker-agnostic issue identity for published_issues.
--
-- issue_key is the tracker-native issue id as text: GitHub '123', Jira
-- 'PROJ-42'. Publishing is going tracker-agnostic (epic bugbot-6gfy), and an
-- integer issue_number cannot represent non-GitHub keys, so the key is stored
-- as opaque TEXT alongside a tracker column naming which tracker owns it.
--
-- Backfill: every pre-existing row was filed on GitHub, whose native id IS
-- the integer issue_number, so CAST(issue_number AS TEXT) converts them in
-- place. tracker's DEFAULT 'github' is likewise correct for all existing rows
-- and for new rows written by the current (still GitHub-only) applier.
--
-- This migration is additive only. issue_number is dropped in 029 once the
-- publish applier speaks keys (epic bugbot-6gfy); until then new rows written
-- through UpsertPublishedIssue carry issue_key = '' (DEFAULT).
ALTER TABLE published_issues ADD COLUMN issue_key TEXT NOT NULL DEFAULT '';
UPDATE published_issues SET issue_key = CAST(issue_number AS TEXT);
ALTER TABLE published_issues ADD COLUMN tracker TEXT NOT NULL DEFAULT 'github';

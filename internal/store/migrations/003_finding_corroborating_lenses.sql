-- 003_finding_corroborating_lenses.sql — record cross-lens corroboration.
--
-- When triage's location-based cross-lens dedup collapses several lenses'
-- reports of one defect into a single finding, the other lenses' names are
-- recorded here as corroboration (reporting signal only — it does not change the
-- finding's tier or status). Stored as a comma-separated list of lens names;
-- lens names never contain commas, so the encoding round-trips losslessly. The
-- column is nullable/defaulted so existing rows and findings with no
-- corroboration carry the empty string.
ALTER TABLE findings ADD COLUMN corroborating_lenses TEXT NOT NULL DEFAULT '';

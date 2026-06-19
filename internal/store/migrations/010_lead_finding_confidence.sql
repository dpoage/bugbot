-- 010_lead_finding_confidence.sql — add confidence scores to leads and findings.
--
-- Leads: a finder agent may attach a confidence score (0..1) to a posted lead
-- so the target lens's scheduler can prioritise high-confidence tips.
--
-- Findings: confidence is derived at persist time from tier + corroboration
-- count (see findingConfidence in findings.go) and stored for fast ordering
-- without recomputing on every read.

ALTER TABLE leads    ADD COLUMN confidence REAL NOT NULL DEFAULT 0;
ALTER TABLE findings ADD COLUMN confidence REAL NOT NULL DEFAULT 0;

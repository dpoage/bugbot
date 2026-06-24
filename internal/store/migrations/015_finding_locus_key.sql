-- 015_finding_locus_key.sql — lens-independent location identity for the durable
-- cross-lens dedup fold.
--
-- The finding fingerprint is sha256(lens, file, locus): it intentionally includes
-- the lens so two lenses reporting the same defect get distinct fingerprints and
-- the cross-lens collapse is delegated to triage's in-memory clustering. That
-- clustering state is volatile — it is NOT rebuilt from already-persisted
-- findings when an interrupted run replays its pending candidates, so a
-- WAL-replayed candidate whose same-locus, different-lens sibling was persisted
-- by the prior run became a SECOND finding instead of folding in.
--
-- locus_key = sha256(normFile, locus) is the fingerprint inputs MINUS the lens.
-- Indexed so triage does a per-candidate point-lookup (OpenFindingsByLocusKey)
-- and folds a cross-lens hit as corroboration. Existing rows default to '' and do
-- not participate until their next upsert recomputes the key (forward-looking).
ALTER TABLE findings ADD COLUMN locus_key TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_findings_locus_key ON findings(locus_key);

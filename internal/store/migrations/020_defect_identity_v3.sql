-- ezmx.1: defect_kind + subject structured identity fields backing
-- Fingerprint v3 (internal/domain/identity.go). Findings gain defect_kind and
-- subject columns, empty on pre-migration (v2) rows.
--
-- Suppressions gain locus_key and legacy columns. A pre-existing suppression
-- row only ever expressed identity via (lens, file, locus) -- v2's
-- Fingerprint -- and cannot be mapped to a v3 fingerprint because
-- defect_kind/subject were never recorded for it. Every row that exists at
-- migration time is marked legacy=1 and, ONLY where a finding sharing its
-- fingerprint still exists AND carries a non-empty locus_key, backfilled
-- with that finding's locus_key. A legacy suppression with no such finding
-- (e.g. the finding was later deleted, or the suppression predates the
-- locus_key column added in 015 and was never re-upserted since) is left
-- with locus_key='' and CANNOT be recovered by this migration -- there is no
-- surviving record of the file/locus it once covered, since a fingerprint is
-- a one-way hash. Such a row's coverage is genuinely lost after the v2->v3
-- cutover; see store.TestIsSuppressed_LegacyLocusFallback_ZeroBackfillLosesCoverage
-- for the pinned (documented, not silently regressed) behavior.
-- IsSuppressed's runtime fallback matches a NEW candidate's locus_key against
-- legacy=1 rows only (never against fresh, post-migration rows), so a
-- BACKFILLABLE legacy suppression keeps suppressing across the v2->v3
-- cutover without letting it silently blanket every future defect_kind at
-- that locus -- which a locus-only match against ALL rows would do.
ALTER TABLE findings ADD COLUMN defect_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE findings ADD COLUMN subject     TEXT NOT NULL DEFAULT '';

ALTER TABLE suppressions ADD COLUMN locus_key TEXT NOT NULL DEFAULT '';
ALTER TABLE suppressions ADD COLUMN legacy    INTEGER NOT NULL DEFAULT 0;

UPDATE suppressions SET legacy = 1;
UPDATE suppressions SET locus_key = (
    SELECT f.locus_key FROM findings f
    WHERE f.fingerprint = suppressions.fingerprint AND f.locus_key != ''
    LIMIT 1
)
WHERE EXISTS (
    SELECT 1 FROM findings f
    WHERE f.fingerprint = suppressions.fingerprint AND f.locus_key != ''
);

CREATE INDEX idx_suppressions_locus_key ON suppressions(locus_key) WHERE locus_key != '';

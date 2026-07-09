-- ezmx.4: backlog reconcile's merge-close columns. When the daemon's
-- periodic reconcile cycle folds a duplicate OPEN finding into an older
-- canonical row (via the dedup arbiter seam), the duplicate is closed
-- status=superseded with superseded_by holding the CANONICAL row's
-- fingerprint -- the typed, machine-readable merge pointer -- and
-- superseded_reason carrying a prose note for operators. Machine decisions
-- never key on superseded_reason (repo invariant); only superseded_by is
-- ever read by code. Empty on every pre-migration row and on any row that
-- was never merged.
ALTER TABLE findings ADD COLUMN superseded_by     TEXT NOT NULL DEFAULT '';
ALTER TABLE findings ADD COLUMN superseded_reason TEXT NOT NULL DEFAULT '';

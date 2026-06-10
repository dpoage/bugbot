-- 004_leads.sql — cross-lens "leads blackboard" for asynchronous tip-passing.
--
-- A finder agent specialised on one lens may notice a suspicion that belongs to
-- a DIFFERENT lens (e.g. a nil-safety finder noticing inconsistent locking). It
-- can post a lead so that the target lens's next finder run picks it up. The
-- blackboard is the carrier: it survives across runs so the store is the only
-- coupling between finders.
--
-- UNIQUE(target_lens, file, line) is the near-identical-lead dedup key. Re-posting
-- the same (target_lens, file, line) triple upserts: the note and poster are
-- refreshed, created_at is preserved. If the lead was previously consumed, the
-- status is flipped back to 'posted' so a re-raised suspicion gets fresh
-- attention. This means a failed finder run loses its claim (leads are consumed
-- at claim time, before the finder runs), which is the accepted trade-off: the
-- next cycle will re-post if the suspicion is still relevant.

CREATE TABLE leads (
  id           TEXT PRIMARY KEY,
  scan_run_id  TEXT NOT NULL DEFAULT '',
  poster_lens  TEXT NOT NULL,
  target_lens  TEXT NOT NULL,
  file         TEXT NOT NULL,
  line         INTEGER NOT NULL,
  note         TEXT NOT NULL,
  status       TEXT NOT NULL DEFAULT 'posted',
  created_at   TEXT NOT NULL,
  consumed_at  TEXT,
  UNIQUE(target_lens, file, line)
);

CREATE INDEX idx_leads_target_status ON leads(target_lens, status);

-- 005_published_issues.sql — tracks GitHub issues filed for findings so that
-- the publish reconciler can keep store findings and GitHub issues in sync
-- idempotently across runs.
--
-- One row per fingerprint; on conflict the state and updated_at are refreshed
-- while created_at is preserved. issue_number is the GitHub issue number (not
-- the internal finding id) returned by the GitHub API when the issue was filed.
-- state is a small machine, not just a mirror of GitHub:
--   'pending' — recorded BEFORE the gh create call; a row stuck here means the
--               create was interrupted and the next run must recover (search
--               for the fingerprint marker) instead of filing a duplicate.
--   'open'    — issue filed and number recorded.
--   'closing' — auto-close comment posted, state PATCH not yet confirmed; the
--               resume path skips re-posting the comment.
--   'closed'  — close completed.

CREATE TABLE published_issues (
  fingerprint  TEXT PRIMARY KEY,
  issue_number INTEGER NOT NULL,
  state        TEXT NOT NULL DEFAULT 'open'
               CHECK (state IN ('pending', 'open', 'closing', 'closed')),
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);

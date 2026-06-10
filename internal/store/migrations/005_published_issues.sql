-- 005_published_issues.sql — tracks GitHub issues filed for findings so that
-- the publish reconciler can keep store findings and GitHub issues in sync
-- idempotently across runs.
--
-- One row per fingerprint; on conflict the state and updated_at are refreshed
-- while created_at is preserved. issue_number is the GitHub issue number (not
-- the internal finding id) returned by the GitHub API when the issue was filed.
-- state mirrors the GitHub issue state: 'open' or 'closed'.

CREATE TABLE published_issues (
  fingerprint  TEXT PRIMARY KEY,
  issue_number INTEGER NOT NULL,
  state        TEXT NOT NULL DEFAULT 'open',
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);

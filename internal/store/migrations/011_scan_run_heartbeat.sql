-- 011_scan_run_heartbeat.sql — advisory single-scan lock support.
--
-- heartbeat is the RFC3339Nano UTC timestamp last written by a running scan
-- process to signal liveness. A NULL or stale heartbeat indicates a process
-- that exited without finishing (crashed, killed). Nullable so existing rows
-- are unaffected.
--
-- pid is the OS process ID of the scan process that created the row. Nullable
-- so existing rows are unaffected. Used by the advisory lock check to
-- distinguish "same process re-entrant" from "foreign process still running".
ALTER TABLE scan_runs ADD COLUMN heartbeat TEXT;
ALTER TABLE scan_runs ADD COLUMN pid       INTEGER;

package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"time"
)

// ScanKind classifies why a scan ran.
type ScanKind string

const (
	// ScanSweep is a periodic full or broad sweep.
	ScanSweep ScanKind = "sweep"
	// ScanTargeted is a commit-triggered investigation of a blast radius.
	ScanTargeted ScanKind = "targeted"
	// ScanOneshot is a single manual `bugbot scan` invocation.
	ScanOneshot ScanKind = "oneshot"
	// ScanCartography is an out-of-band cartographer refresh (bugbot cartography
	// --run): the package-summary pass without any finder/verify stages.
	ScanCartography ScanKind = "cartography"
	// ScanVerifyDrain is a standalone verify-drain pass: it verifies pending
	// candidates left by interrupted runs WITHOUT invoking the finder stage.
	ScanVerifyDrain ScanKind = "verify-drain"
	// ScanImpactSweep is a standalone impact-sweep drain: it re-ranks unswept
	// open findings by reachability, decoupled from the terminal Stage F.
	ScanImpactSweep ScanKind = "impact-sweep"
)

// ScanRun records a single scan invocation. StatsJSON holds an opaque,
// caller-defined JSON blob of counters (candidates, verified, etc.).
type ScanRun struct {
	ID         string
	Kind       ScanKind
	CommitSHA  string
	StartedAt  time.Time
	FinishedAt time.Time // zero until BeginScanRun's run is finished
	StatsJSON  string
	// Heartbeat is the most recent liveness timestamp written by the running
	// process. Zero when no heartbeat has been written (pre-011 rows or
	// finished runs that were never updated).
	Heartbeat time.Time
	// PID is the OS process ID that created this run. Rows written before
	// migration 011 (which added the column) carry no recorded PID.
	PID int
}

// BeginScanRun creates a new scan-run row in the started state and returns its
// id. Pass the id to RecordSpend so spend is attributed to this run, and to
// FinishScanRun when the run completes.
//
// The row is created with the current process's PID and the start time as the
// initial heartbeat. Call UpdateHeartbeat periodically to signal liveness.
func (s *Store) BeginScanRun(ctx context.Context, kind ScanKind, commitSHA string) (string, error) {
	id := newID()
	now := nowUTC().Format(timeLayout)
	pid := os.Getpid()
	_, err := s.exec(ctx, "begin_scan_run", `
		INSERT INTO scan_runs (id, kind, commit_sha, started_at, heartbeat, pid)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, string(kind), commitSHA, now, now, pid,
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

// UpdateHeartbeat refreshes the heartbeat timestamp of the given scan run to
// now. Callers should call this periodically (e.g. every 30 s) while the scan
// is running so the advisory lock in ActiveScanRuns can distinguish live
// processes from stale/crashed ones.
func (s *Store) UpdateHeartbeat(ctx context.Context, id string) error {
	_, err := s.exec(ctx, "update_heartbeat",
		`UPDATE scan_runs SET heartbeat = ? WHERE id = ?`,
		nowUTC().Format(timeLayout), id,
	)
	if err != nil {
		return err
	}
	return nil
}

// ActiveScanRuns returns scan runs that are considered live: started, not yet
// finished, and whose heartbeat was updated within staleAfter of now. Runs
// with a NULL heartbeat or a heartbeat older than staleAfter are excluded
// (they represent crashed or pre-011 rows that have not been updated).
//
// This is the query backing the advisory single-scan lock: call it before
// BeginScanRun to detect a concurrently running scan against the same state db.
func (s *Store) ActiveScanRuns(ctx context.Context, staleAfter time.Duration) ([]ScanRun, error) {
	// Cutoff: runs whose heartbeat is older than this are considered stale.
	cutoff := nowUTC().Add(-staleAfter).Format(timeLayout)

	return queryRows(ctx, s, "active_scan_runs", `
		SELECT id, kind, commit_sha, started_at, finished_at, stats_json, heartbeat, pid
		FROM scan_runs
		WHERE finished_at IS NULL
		  AND heartbeat IS NOT NULL
		  AND heartbeat >= ?`,
		[]any{cutoff}, scanScanRun)
}

// FinishScanRun marks the run finished at now and stores its stats blob.
// Returns ErrNotFound if the id is unknown.
func (s *Store) FinishScanRun(ctx context.Context, id, statsJSON string) error {
	res, err := s.exec(ctx, "finish_scan_run",
		`UPDATE scan_runs SET finished_at = ?, stats_json = ? WHERE id = ?`,
		nowUTC().Format(timeLayout), statsJSON, id,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return annotateErr(s.path, "finish_scan_run", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetScanRun returns the scan run with the given id, or ErrNotFound.
func (s *Store) GetScanRun(ctx context.Context, id string) (ScanRun, error) {
	var r ScanRun
	var kind, started string
	var finished, heartbeat sql.NullString
	var pid sql.NullInt64
	err := s.queryRow(ctx, "get_scan_run", `
		SELECT id, kind, commit_sha, started_at, finished_at, stats_json, heartbeat, pid
		FROM scan_runs WHERE id = ?`, []any{id}, func(row *sql.Row) error {
		return row.Scan(&r.ID, &kind, &r.CommitSHA, &started, &finished, &r.StatsJSON, &heartbeat, &pid)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ScanRun{}, ErrNotFound
	}
	if err != nil {
		return ScanRun{}, err
	}
	r.Kind = ScanKind(kind)
	if r.StartedAt, err = parseTime(started); err != nil {
		return ScanRun{}, err
	}
	if finished.Valid {
		if r.FinishedAt, err = parseTime(finished.String); err != nil {
			return ScanRun{}, err
		}
	}
	if heartbeat.Valid {
		if r.Heartbeat, err = parseTime(heartbeat.String); err != nil {
			return ScanRun{}, err
		}
	}
	if pid.Valid {
		r.PID = int(pid.Int64)
	}
	return r, nil
}

// scanScanRun is the canonical row scanner for a full scan_runs SELECT
// (id, kind, commit_sha, started_at, finished_at, stats_json, heartbeat, pid).
// Used with scanRows by ActiveScanRuns and any future list query.
func scanScanRun(rows *sql.Rows) (ScanRun, error) {
	var r ScanRun
	var kind, started string
	var finished, heartbeat sql.NullString
	var pid sql.NullInt64
	if err := rows.Scan(&r.ID, &kind, &r.CommitSHA, &started, &finished, &r.StatsJSON, &heartbeat, &pid); err != nil {
		return ScanRun{}, err
	}
	r.Kind = ScanKind(kind)
	var err error
	if r.StartedAt, err = parseTime(started); err != nil {
		return ScanRun{}, err
	}
	if finished.Valid {
		if r.FinishedAt, err = parseTime(finished.String); err != nil {
			return ScanRun{}, err
		}
	}
	if heartbeat.Valid {
		if r.Heartbeat, err = parseTime(heartbeat.String); err != nil {
			return ScanRun{}, err
		}
	}
	if pid.Valid {
		r.PID = int(pid.Int64)
	}
	return r, nil
}

// LatestScanRun returns the most recently started scan run, or ErrNotFound
// when no run has ever been recorded.
//
// CAVEAT: the ORDER BY started_at DESC tiebreaks on id DESC, but started_at
// is a RFC3339 TEXT and does not sort consistently as a string across
// the second boundary (see the warning on filestate.go's epochSentinel and
// the matching caution on agent_units). In practice every scan run's
// started_at is written by nowUTC() at sub-second resolution, so the
// tiebreak is rarely exercised; the spec was "only wrap errors" for this
// file, so the ORDER BY is intentionally left as-is.
func (s *Store) LatestScanRun(ctx context.Context) (ScanRun, error) {
	var id string
	err := s.queryRow(ctx, "latest_scan_run",
		`SELECT id FROM scan_runs ORDER BY started_at DESC, id DESC LIMIT 1`,
		nil, func(row *sql.Row) error {
			return row.Scan(&id)
		})
	if errors.Is(err, sql.ErrNoRows) {
		return ScanRun{}, ErrNotFound
	}
	if err != nil {
		return ScanRun{}, err
	}
	return s.GetScanRun(ctx, id)
}

// LastFinishedSweepCommit returns the commit SHA of the most recently finished
// Sweep scan run, excluding excludeID (the in-flight run, so a sweep cycle does
// not pick itself as its own baseline). It is the 'last green' baseline for the
// daemon's regress digest: findings whose anchor is absent at this commit were
// introduced since the last full snapshot. Returns ErrNotFound when no prior
// finished sweep exists (e.g. the first sweep ever).
//
// CAVEAT: ORDER BY finished_at DESC tiebreaks on id DESC; finished_at is an
// RFC3339 TEXT written by nowUTC() at sub-second resolution, so the same
// string-sort caveat as LatestScanRun applies and is likewise rarely exercised.
func (s *Store) LastFinishedSweepCommit(ctx context.Context, excludeID string) (string, error) {
	var sha string
	err := s.queryRow(ctx, "last_finished_sweep_commit", `
		SELECT commit_sha FROM scan_runs
		WHERE kind = ? AND finished_at IS NOT NULL AND id != ?
		ORDER BY finished_at DESC, id DESC
		LIMIT 1`,
		[]any{string(ScanSweep), excludeID}, func(row *sql.Row) error {
			return row.Scan(&sha)
		})
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return sha, nil
}

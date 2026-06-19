package store

import (
	"context"
	"database/sql"
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
}

// BeginScanRun creates a new scan-run row in the started state and returns its
// id. Pass the id to RecordSpend so spend is attributed to this run, and to
// FinishScanRun when the run completes.
func (s *Store) BeginScanRun(ctx context.Context, kind ScanKind, commitSHA string) (string, error) {
	id := newID()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO scan_runs (id, kind, commit_sha, started_at)
		VALUES (?, ?, ?, ?)`,
		id, string(kind), commitSHA, nowUTC().Format(timeLayout),
	)
	if err != nil {
		return "", annotateErr(s.path, "begin_scan_run", err)
	}
	return id, nil
}

// FinishScanRun marks the run finished at now and stores its stats blob.
// Returns ErrNotFound if the id is unknown.
func (s *Store) FinishScanRun(ctx context.Context, id, statsJSON string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE scan_runs SET finished_at = ?, stats_json = ? WHERE id = ?`,
		nowUTC().Format(timeLayout), statsJSON, id,
	)
	if err != nil {
		return annotateErr(s.path, "finish_scan_run", err)
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
	var finished sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, commit_sha, started_at, finished_at, stats_json
		FROM scan_runs WHERE id = ?`, id,
	).Scan(&r.ID, &kind, &r.CommitSHA, &started, &finished, &r.StatsJSON)
	if err == sql.ErrNoRows {
		return ScanRun{}, ErrNotFound
	}
	if err != nil {
		return ScanRun{}, annotateErr(s.path, "get_scan_run", err)
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
	return r, nil
}

// LatestScanRun returns the most recently started scan run, or ErrNotFound
// when no run has ever been recorded.
//
// CAVEAT: the ORDER BY started_at DESC tiebreaks on id DESC, but started_at
// is a RFC3339Nano TEXT and does not sort consistently as a string across
// the second boundary (see the warning on filestate.go's epochSentinel and
// the matching caution on agent_units). In practice every scan run's
// started_at is written by nowUTC() at sub-second resolution, so the
// tiebreak is rarely exercised; the spec was "only wrap errors" for this
// file, so the ORDER BY is intentionally left as-is.
func (s *Store) LatestScanRun(ctx context.Context) (ScanRun, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM scan_runs ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return ScanRun{}, ErrNotFound
	}
	if err != nil {
		return ScanRun{}, annotateErr(s.path, "latest_scan_run", err)
	}
	return s.GetScanRun(ctx, id)
}

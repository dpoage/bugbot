package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrInvalidPackageSummary is returned by UpsertPackageSummaries when a row
// is missing Pkg, Fingerprint, or Summary. The empty Pkg would silently
// collide with every other empty-Pkg write in the same transaction; the
// other two would write a meaningless cache row. Callers should treat this
// as a programmer error and never produce it in normal flow.
var ErrInvalidPackageSummary = errors.New("store: invalid package summary (empty pkg, fingerprint, or summary)")

// PackageSummary is a cached natural-language summary of one package (a
// repo-relative directory), valid for the content Fingerprint it was generated
// against. A finder reads these instead of rediscovering the package via tools.
//
// The (Pkg, Fingerprint) pair is the cache key: a stored row whose Fingerprint
// matches the current package fingerprint is a valid summary for the current
// content; a mismatch (or a missing row) means the package has changed (or
// never been summarized) and the caller must regenerate before reusing. The
// Model field records which model produced the summary (informational; future
// re-tuning may prefer newer models). UpdatedAt is the wall-clock time the row
// was written and is filled in by UpsertPackageSummaries when the caller
// leaves it zero.
type PackageSummary struct {
	Pkg         string
	Fingerprint string
	Summary     string
	Model       string
	UpdatedAt   time.Time
}

// GetPackageSummaries returns summaries for the given packages; packages with
// no row are absent from the map. The query is chunked to stay under
// SQLite's sqliteMaxVars host-parameter limit (mirror ScanWatermarks in
// filestate.go).
func (s *Store) GetPackageSummaries(ctx context.Context, pkgs []string) (map[string]PackageSummary, error) {
	if len(pkgs) == 0 {
		return nil, nil
	}
	result := make(map[string]PackageSummary, len(pkgs))
	for _, chunk := range chunkStrings(pkgs, sqliteMaxVars) {
		if err := s.getPackageSummariesChunk(ctx, chunk, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Store) getPackageSummariesChunk(ctx context.Context, pkgs []string, out map[string]PackageSummary) error {
	q := `SELECT pkg, fingerprint, summary, model, updated_at
	      FROM package_summaries WHERE pkg IN (` + buildPlaceholders(len(pkgs)) + `)`
	args := make([]interface{}, len(pkgs))
	for i, p := range pkgs {
		args[i] = p
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return annotateErr(s.path, "get_package_summaries", err)
	}
	defer func() { _ = rows.Close() }()
	// Chunk-accumulation into a shared out map: scanRows returns a fresh slice
	// per call and would force the caller to merge slices across chunks.
	// The manual loop stays; annotateErr gives it the same error surface as
	// the rest of the package.
	for rows.Next() {
		var ps PackageSummary
		var updatedAt string
		if err := rows.Scan(&ps.Pkg, &ps.Fingerprint, &ps.Summary, &ps.Model, &updatedAt); err != nil {
			return annotateErr(s.path, "get_package_summaries", err)
		}
		ps.UpdatedAt, err = parseTime(updatedAt)
		if err != nil {
			return err
		}
		out[ps.Pkg] = ps
	}
	if err := rows.Err(); err != nil {
		return annotateErr(s.path, "get_package_summaries", err)
	}
	return nil
}

// UpsertPackageSummaries writes a batch in one transaction, overwriting by
// pkg (ON CONFLICT(pkg) DO UPDATE). UpdatedAt is set to now for every row
// when the caller leaves it zero (mirror UpsertFileStates in filestate.go).
//
// The empty input is a no-op (no transaction is opened). Rows with a zero
// UpdatedAt are stamped with nowUTC at the call site so a single batch uses
// one consistent timestamp.
func (s *Store) UpsertPackageSummaries(ctx context.Context, sums []PackageSummary) error {
	if len(sums) == 0 {
		return nil
	}
	// Validate up front: a row missing Pkg, Fingerprint, or Summary is almost
	// certainly a caller bug, and an empty Pkg would silently collide with
	// every other empty-Pkg write in the same transaction. Reject loudly
	// with a typed error the caller can identify.
	for _, ps := range sums {
		if ps.Pkg == "" || ps.Fingerprint == "" || ps.Summary == "" {
			return ErrInvalidPackageSummary
		}
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO package_summaries (pkg, fingerprint, summary, model, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(pkg) DO UPDATE SET
			  fingerprint = excluded.fingerprint,
			  summary     = excluded.summary,
			  model       = excluded.model,
			  updated_at  = excluded.updated_at`)
		if err != nil {
			return annotateErr(s.path, "upsert_package_summaries", err)
		}
		defer func() { _ = stmt.Close() }()

		now := nowUTC().Format(timeLayout)
		for _, ps := range sums {
			updatedAt := now
			if !ps.UpdatedAt.IsZero() {
				updatedAt = ps.UpdatedAt.UTC().Format(timeLayout)
			}
			if _, err := stmt.ExecContext(ctx,
				ps.Pkg, ps.Fingerprint, ps.Summary, ps.Model, updatedAt,
			); err != nil {
				return annotateErr(s.path, "upsert_package_summaries", err)
			}
		}
		return nil
	})
}

// ListPackageSummaries returns every persisted package summary, ordered by
// package path. Used by `bugbot cartography` to surface the cartographer's
// cached output; the scan pipeline reads by key via GetPackageSummaries.
//
// ORDER BY pkg is the unique primary key, so the ordering is already total
// and deterministic without a rowid tiebreak.
func (s *Store) ListPackageSummaries(ctx context.Context) ([]PackageSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT pkg, fingerprint, summary, model, updated_at
		FROM package_summaries ORDER BY pkg`)
	if err != nil {
		return nil, annotateErr(s.path, "list_package_summaries", err)
	}
	defer func() { _ = rows.Close() }()
	return scanRows(rows, func(r *sql.Rows) (PackageSummary, error) {
		var ps PackageSummary
		var updatedAt string
		if err := r.Scan(&ps.Pkg, &ps.Fingerprint, &ps.Summary, &ps.Model, &updatedAt); err != nil {
			return PackageSummary{}, err
		}
		var err error
		if ps.UpdatedAt, err = parseTime(updatedAt); err != nil {
			return PackageSummary{}, err
		}
		return ps, nil
	})
}

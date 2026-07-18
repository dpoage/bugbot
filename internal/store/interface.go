package store

import (
	"context"

	"github.com/dpoage/bugbot/internal/domain"
)

// StoreReader is the read-only face of *Store. Consumers that only need to
// query findings (e.g. backlog selectors, summary helpers) accept this narrow
// interface instead of *Store so tests can supply a hand-written fake without
// opening a real SQLite database.
//
// The interface intentionally starts small — three methods that cover the
// most common read paths. Grow it as test demand warrants; the compile-time
// assertion below ensures *Store stays in sync.
type StoreReader interface {
	// GetFinding returns the finding with the given id, or ErrNotFound.
	GetFinding(ctx context.Context, id string) (domain.Finding, error)

	// GetFindingByFingerprint returns the finding with the given fingerprint,
	// or ErrNotFound.
	GetFindingByFingerprint(ctx context.Context, fingerprint string) (domain.Finding, error)

	// ListFindings returns findings matching the filter, newest-updated first.
	ListFindings(ctx context.Context, filter domain.FindingFilter) ([]domain.Finding, error)

	// UnclaimableReproFingerprints returns the fingerprints of repro_attempts
	// rows that can never be claimed again (done, abandoned, or attempt budget
	// exhausted). Backlog selectors use it to avoid re-dispatching findings
	// whose reproduction already ran to completion.
	UnclaimableReproFingerprints(ctx context.Context) (map[string]struct{}, error)
}

// StoreWriter is the write face of *Store. Consumers that only mutate findings
// accept this narrow interface so tests can supply a lightweight fake without
// opening a real SQLite database.
//
// Same growth policy as StoreReader: add methods here only when a test needs
// them.
type StoreWriter interface {
	// UpsertFinding inserts the finding or, if one with the same fingerprint
	// exists, updates its mutable fields and returns the stored row.
	UpsertFinding(ctx context.Context, f domain.Finding) (domain.Finding, error)
}

// Compile-time assertions: *Store must satisfy both interfaces. If a method
// is renamed or its signature changes, the build breaks here rather than
// silently at runtime.
var _ StoreReader = (*Store)(nil)
var _ StoreWriter = (*Store)(nil)

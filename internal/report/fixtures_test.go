package report

import (
	"time"

	"github.com/dpoage/bugbot/internal/domain"
)

// fixedTime is a pinned timestamp so rendered output is deterministic.
var fixedTime = time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

// fixtureFindings returns a stable set of findings exercising the renderers:
// a T1 with a repro path, a T2 without, and a low-severity entry to check
// severity ordering. IDs and timestamps are fixed for golden comparisons.
func fixtureFindings() []domain.Finding {
	return []domain.Finding{
		{
			ID:          "aaaa111122223333",
			Fingerprint: "fp-t2-nil-deref",
			Title:       "nil pointer dereference in handler",
			Description: "req.User may be nil when auth middleware is skipped.",
			Reasoning:   "Finder flagged unchecked deref; verifier confirmed the skip path leaves User nil.",
			Severity:    "high",
			Tier:        2,
			Status:      domain.StatusOpen,
			Lens:        "nilcheck",
			File:        "internal/api/handler.go",
			Line:        42,
			CommitSHA:   "deadbeef",
			FileHash:    "h1",
		},
		{
			ID:          "bbbb444455556666",
			Fingerprint: "fp-t1-race",
			Title:       "data race on shared counter",
			Description: "counter incremented from two goroutines without a lock.",
			Reasoning:   "Reproducer generated a -race test that fails deterministically.",
			Severity:    "critical",
			Tier:        1,
			Status:      domain.StatusOpen,
			Lens:        "race",
			File:        "internal/worker/pool.go",
			Line:        108,
			CommitSHA:   "deadbeef",
			FileHash:    "h2",
			ReproPath:   ".bugbot/repros/fp-t1-race/race_test.go",
		},
		{
			ID:          "cccc777788889999",
			Fingerprint: "fp-low-style",
			Title:       "ignored error from Close",
			Description: "deferred Close error is dropped.",
			Reasoning:   "verifier agreed it is low impact.",
			Severity:    "low",
			Tier:        2,
			Status:      domain.StatusOpen,
			Lens:        "errcheck",
			File:        "internal/worker/pool.go",
			Line:        50,
			CommitSHA:   "deadbeef",
			FileHash:    "h3",
		},
	}
}

func fixtureMeta() Metadata {
	return Metadata{
		RepoPath:    "/home/user/target-repo",
		Commit:      "deadbeef",
		GeneratedAt: fixedTime,
		ScanRunID:   "run-001",
	}
}

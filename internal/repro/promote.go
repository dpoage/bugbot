package repro

import (
	"context"
	"fmt"
	"sync"

	"github.com/dpoage/bugbot/internal/store"
)

// Summary aggregates the outcome of a PromoteAll run.
type Summary struct {
	// Attempted is the number of findings a reproduction was attempted on.
	Attempted int
	// Promoted is the number promoted to Tier-1.
	Promoted int
	// Failed is the number that could not be reproduced (stayed Tier-2).
	Failed int
	// PerFinding holds the per-finding outcome in input order.
	PerFinding []FindingOutcome
}

// FindingOutcome records one finding's reproduction result for the Summary.
type FindingOutcome struct {
	FindingID    string
	Title        string
	Promoted     bool
	ArtifactPath string
	Attempts     int
	// Reason is the non-promotion category, or an infrastructure error message.
	Reason string
	// Err is the infrastructure error, if the attempt failed to run at all.
	Err error
}

// PromoteAll attempts to reproduce each finding (expected to be Tier-2
// "verified" findings) with bounded parallelism, and on success updates the
// finding's store row to Tier-1 with its repro_path. Findings that cannot be
// reproduced are left untouched (they stay Tier-2): failure demotes nothing.
//
// Bounded parallelism defaults to Options.MaxParallel (deliberately small —
// each sandbox run copies the whole repo workspace). An infrastructure error
// on one finding is recorded in that finding's outcome and does not abort the
// others; PromoteAll returns a nil error unless ctx is cancelled.
func (r *Reproducer) PromoteAll(ctx context.Context, st *store.Store, findings []store.Finding) (*Summary, error) {
	if st == nil {
		return nil, fmt.Errorf("repro: nil store")
	}

	summary := &Summary{
		Attempted:  len(findings),
		PerFinding: make([]FindingOutcome, len(findings)),
	}

	sem := make(chan struct{}, r.opts.MaxParallel)
	var wg sync.WaitGroup

	for i := range findings {
		select {
		case <-ctx.Done():
			// Record remaining findings as not attempted-to-completion and stop
			// launching new work; in-flight goroutines observe ctx too.
			summary.PerFinding[i] = FindingOutcome{
				FindingID: findings[i].ID,
				Title:     findings[i].Title,
				Reason:    "cancelled",
				Err:       ctx.Err(),
			}
			continue
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()

			f := findings[idx]
			outcome := FindingOutcome{FindingID: f.ID, Title: f.Title}

			att, err := r.Attempt(ctx, f)
			if err != nil {
				outcome.Reason = "error: " + err.Error()
				outcome.Err = err
				summary.PerFinding[idx] = outcome
				return
			}

			outcome.Attempts = att.Attempts
			if att.Promoted {
				if perr := promoteFinding(ctx, st, f, att.ArtifactPath); perr != nil {
					// The bug WAS demonstrated; only persistence failed. Surface it
					// as an error outcome rather than silently dropping the result.
					outcome.Reason = "promotion persist failed: " + perr.Error()
					outcome.Err = perr
					summary.PerFinding[idx] = outcome
					return
				}
				outcome.Promoted = true
				outcome.ArtifactPath = att.ArtifactPath
			} else {
				outcome.Reason = att.Reason
			}
			summary.PerFinding[idx] = outcome
		}(i)
	}

	wg.Wait()

	if err := ctx.Err(); err != nil {
		return summary, err
	}

	for _, o := range summary.PerFinding {
		if o.Promoted {
			summary.Promoted++
		} else {
			summary.Failed++
		}
	}
	return summary, nil
}

// promoteFinding updates a finding's store row to Tier-1 with the given
// repro_path. It is a SQL-free wrapper over the store's existing UpsertFinding,
// which keys on fingerprint, preserves id/created_at, and refreshes mutable
// fields (here tier and repro_path) in place.
//
// We read the current row back first so we never clobber fields that may have
// changed since the finding was loaded; only tier and repro_path are mutated.
func promoteFinding(ctx context.Context, st *store.Store, f store.Finding, reproPath string) error {
	current, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		return err
	}
	current.Tier = 1
	current.ReproPath = reproPath
	if _, err := st.UpsertFinding(ctx, current); err != nil {
		return err
	}
	return nil
}

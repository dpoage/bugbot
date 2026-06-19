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
	// FixWitnessed is the number promoted to Tier-0 (fix witnessed by patch-prover).
	FixWitnessed int
	// NeedsHuman is the number where patch-prover exhausted attempts without a fix.
	NeedsHuman int
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
	// FixWitnessed is true when the patch-prover successfully witnessed a fix
	// (finding promoted to Tier-0).
	FixWitnessed bool
	// NeedsHuman is true when the patch-prover exhausted attempts without
	// finding a minimal fix.
	NeedsHuman bool
}

// PromoteOne attempts to reproduce a single finding and updates the store row
// on success (Tier-1 + repro_path). It is the single-finding entry point used
// by the funnel's in-run hook (the funnel's consumer goroutine is the
// parallelism bound; calling PromoteAll per finding would multiply slots).
// PromoteAll's internal semaphore is intentionally NOT used here.
//
// Infrastructure errors (agent/LLM failure, sandbox launch failure) are
// returned; a finding that simply could not be reproduced is reported via a
// nil error with the outcome recorded in the store (tier stays 2).
//
// scanRunID may be empty when called from the daemon backlog drain (cross-run
// context); the agent_units row will carry an empty scan_run_id in that case.
func (r *Reproducer) PromoteOne(ctx context.Context, st *store.Store, finding store.Finding) (*FindingOutcome, error) {
	if st == nil {
		return nil, fmt.Errorf("repro: nil store")
	}

	outcome := &FindingOutcome{FindingID: finding.ID, Title: finding.Title}

	att, err := r.Attempt(ctx, finding)
	if err != nil {
		outcome.Reason = "error: " + err.Error()
		outcome.Err = err
		return outcome, err
	}

	outcome.Attempts = att.Attempts
	if att.Promoted {
		if perr := promoteFinding(ctx, st, finding, att.ArtifactPath); perr != nil {
			outcome.Reason = "promotion persist failed: " + perr.Error()
			outcome.Err = perr
			return outcome, perr
		}
		outcome.Promoted = true
		outcome.ArtifactPath = att.ArtifactPath

		if r.opts.PatchProver {
			patchResult, perr := r.provePatch(ctx, st, finding, att)
			if perr != nil {
				outcome.Reason = "patch-prover error: " + perr.Error()
			} else {
				outcome.FixWitnessed = patchResult.FixWitnessed
				outcome.NeedsHuman = patchResult.NeedsHuman
				if patchResult.SkippedNoSuiteCmd {
					outcome.Reason = "patch-prover skipped: toolchain not identified and repro.suite_cmd not configured"
				}
			}
		}
	} else {
		outcome.Reason = att.Reason
	}

	return outcome, nil
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

	// One-time online dependency prefetch (DepStrategyFetch only). Runs once per
	// PromoteAll before any network-none run; the hook is itself sync.Once-guarded
	// so repeated calls across a Reproducer's lifetime do not re-download. A
	// prefetch failure aborts the run: the network-none attempts that follow
	// would all fail to resolve modules, so failing fast with a clear error is
	// better than a wave of confusing per-finding module errors.
	if r.deps.Prefetch != nil {
		if err := r.deps.Prefetch(ctx); err != nil {
			return nil, fmt.Errorf("repro: dependency prefetch: %w", err)
		}
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

				// Patch-prover: if enabled, attempt to find and witness a minimal fix.
				if r.opts.PatchProver {
					patchResult, perr := r.provePatch(ctx, st, f, att)
					if perr != nil {
						// Infrastructure failure: record but do not block the T1 promotion.
						outcome.Reason = "patch-prover error: " + perr.Error()
					} else {
						outcome.FixWitnessed = patchResult.FixWitnessed
						outcome.NeedsHuman = patchResult.NeedsHuman
						if patchResult.SkippedNoSuiteCmd {
							outcome.Reason = "patch-prover skipped: toolchain not identified and repro.suite_cmd not configured"
						}
					}
				}
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
		if o.FixWitnessed {
			summary.FixWitnessed++
		}
		if o.NeedsHuman {
			summary.NeedsHuman++
		}
	}
	return summary, nil
}

// patchOutcome is the result of a patch-prover run on a single finding.
type patchOutcome struct {
	FixWitnessed bool
	NeedsHuman   bool
	// SkippedNoSuiteCmd is set when the repo's toolchain could not be
	// identified and no repro.suite_cmd is configured: the suite-green half of
	// the witness cannot be proven, so the prover declines rather than guesses.
	SkippedNoSuiteCmd bool
}

// provePatch runs the patch-prover on a finding that was just promoted to T1.
// It either witnesses a fix (T0) or records needs-human on exhaustion.
func (r *Reproducer) provePatch(ctx context.Context, st *store.Store, f store.Finding, att *Attempt) (patchOutcome, error) {
	prover := &PatchProver{
		client:      r.client,
		sb:          r.sb,
		repoDir:     r.repoDir,
		maxAttempts: r.opts.PatchMaxAttempts,
		timeout:     r.opts.Timeout,
		image:       r.opts.Image,
		artifactDir: r.opts.ArtifactDir,
		agentLimits:   r.opts.AgentLimits,
		transcriptDir: r.opts.TranscriptDir,
		suiteCmd:      r.opts.PatchSuiteCmd,
		depMounts:   r.deps.ROMounts,
		depEnv:      r.deps.Env,
		setupCmds:   r.deps.SetupCmds,
	}
	return prover.Prove(ctx, st, f, att)
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

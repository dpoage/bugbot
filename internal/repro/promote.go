package repro

import (
	"context"
	"fmt"
	"sync"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/store"
)

// Summary aggregates the outcome of a PromoteAll run.
type Summary struct {
	// Attempted is the number of findings where reproduction was genuinely
	// attempted (not skipped because the queue row was already claimed/done).
	Attempted int
	// Skipped is the number of findings bypassed because their repro_attempts
	// row was already running, done, or abandoned (claim/skip semantics).
	// These are excluded from Attempted and Failed.
	Skipped int
	// Promoted is the number promoted to Tier-1 (full repro-pathing + patch-prover).
	Promoted int
	// Witnessed is the number of below-quorum NeedsHuman findings whose repro
	// hook fired and wrote a witness bundle (ReproWitness) without promoting.
	// bugbot-w1bh.
	Witnessed int
	// Failed is the number that could not be reproduced (stayed at their prior tier).
	Failed int
	// FixWitnessed is the number promoted to Tier-0 (fix witnessed by patch-prover).
	FixWitnessed int
	// NeedsHuman is the number where patch-prover exhausted attempts without a fix.
	NeedsHuman int
	// ExitZeroCount is the number of attempts where the repro ran but the bug
	// did not manifest (exit 0). These contribute to the per-finding
	// repro-contradiction signal in the store.
	ExitZeroCount int
	// PerFinding holds the per-finding outcome in input order.
	PerFinding []FindingOutcome
}

// FindingOutcome records one finding's reproduction result for the Summary.
type FindingOutcome struct {
	FindingID string
	Title     string
	Promoted  bool
	// Witnessed is true when a below-quorum NeedsHuman finding received a
	// non-promoting repro attempt that wrote ReproWitness. Mutually
	// exclusive with Promoted: the hook either promotes OR witnesses.
	Witnessed    bool
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
	// Skipped is true when the repro_attempts queue row was already claimed,
	// done, or abandoned — no reproduction work was performed. Skipped outcomes
	// are excluded from Summary.Attempted and Summary.Failed.
	Skipped bool
	// ExitZero is true when the repro ran without infrastructure error but
	// exited 0 — the test ran and the bug did not manifest. This is
	// disconfirming evidence; >= ReproContradictionThreshold such outcomes
	// sets the repro-contradicted signal on the finding.
	ExitZero bool
}

// PromoteOne attempts to reproduce a single finding and updates the store row
// on success. It is the single-finding entry point used by the funnel's
// in-run hook (the funnel's consumer goroutine is the parallelism bound;
// calling PromoteAll per finding would multiply slots). PromoteAll's internal
// semaphore is intentionally NOT used here.
//
// Queue integration: if the finding has a repro_attempts row, PromoteOne claims
// it before attempting and marks it done or infra_retry on return. If EnqueueRepro
// or ClaimReproAttempt fails with ErrReproAlreadyClaimed, the attempt is skipped
// (another worker already owns it).
//
// Two outcomes on a demonstrated bug (Attempt.Promoted):
//   - neither NeedsHuman nor WitnessOnly → promoteFinding (Tier-1 + ReproPath)
//     and optionally the patch-prover cascade.
//   - NeedsHuman (below-quorum verifier survivor) OR WitnessOnly (the
//     detected ecosystem cannot provide an execution witness for the
//     target file — bugbot-qb4r layer b) → witnessFinding (ReproWitness
//     only; Tier, ReproPath, NeedsHuman all untouched; no patch-prover).
//     bugbot-w1bh split: repro-as-evidence vs repro-as-promotion.
//
// Infrastructure errors (agent/LLM failure, sandbox launch failure) are
// returned; a finding that simply could not be reproduced is reported via a
// nil error with the outcome recorded in the store.
//
// scanRunID may be empty when called from the daemon backlog drain (cross-run
// context); the agent_units row will carry an empty scan_run_id in that case.
func (r *Reproducer) PromoteOne(ctx context.Context, st *store.Store, finding domain.Finding) (*FindingOutcome, error) {
	if st == nil {
		return nil, fmt.Errorf("repro: nil store")
	}
	return promoteOne(ctx, r, st, finding)
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
//
// Queue integration: each finding is claimed from the repro_attempts table
// before attempting; already-claimed or exhausted findings are skipped. This
// provides claim/skip semantics across all three dispatch paths (in-run hook,
// daemon drain, `bugbot repro` CLI) without any of them duplicating the logic.
func (r *Reproducer) PromoteAll(ctx context.Context, st *store.Store, findings []domain.Finding) (*Summary, error) {
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
		PerFinding: make([]FindingOutcome, len(findings)),
	}

	sem := make(chan struct{}, r.opts.MaxParallel)
	var wg sync.WaitGroup

	for i := range findings {
		select {
		case <-ctx.Done():
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

			// promoteOne always returns a non-nil outcome.
			outcome, _ := promoteOne(ctx, r, st, findings[idx])
			summary.PerFinding[idx] = *outcome
		}(i)
	}

	wg.Wait()

	if err := ctx.Err(); err != nil {
		return summary, err
	}

	for _, o := range summary.PerFinding {
		if o.Skipped {
			summary.Skipped++
			continue
		}
		summary.Attempted++
		switch {
		case o.Promoted:
			summary.Promoted++
		case o.Witnessed:
			summary.Witnessed++
		default:
			summary.Failed++
		}
		if o.FixWitnessed {
			summary.FixWitnessed++
		}
		if o.NeedsHuman {
			summary.NeedsHuman++
		}
		if o.ExitZero {
			summary.ExitZeroCount++
		}
	}
	return summary, nil
}

// promoteOne is the shared single-finding body for both PromoteOne and PromoteAll.
// It claims the repro_attempts queue row (creating it if absent), runs the
// reproducer, writes the outcome to the store, and marks the queue row done or
// infra_retry. Callers own concurrency control; this function does not acquire
// any semaphores.
//
// Queue claim semantics: if the finding's queue row is already claimed (another
// dispatch path got there first), promoteOne returns a skipped outcome with nil
// error. This is the single implementation of claim/skip for all three dispatch
// paths.
func promoteOne(ctx context.Context, r *Reproducer, st *store.Store, finding domain.Finding) (*FindingOutcome, error) {
	outcome := &FindingOutcome{FindingID: finding.ID, Title: finding.Title}

	// Ensure the queue row exists, then claim it.
	if _, err := st.EnqueueRepro(ctx, finding.Fingerprint); err != nil {
		// Enqueue failure is an infrastructure error; surface it.
		outcome.Reason = "enqueue error: " + err.Error()
		outcome.Err = err
		return outcome, err
	}
	if _, err := st.ClaimReproAttempt(ctx, finding.Fingerprint); err != nil {
		if err == store.ErrReproAlreadyClaimed {
			// Another dispatch path already owns this attempt; skip.
			outcome.Reason = "skipped: already claimed"
			outcome.Skipped = true
			return outcome, nil
		}
		outcome.Reason = "claim error: " + err.Error()
		outcome.Err = err
		return outcome, err
	}

	att, err := r.Attempt(ctx, finding)
	if err != nil {
		// Infrastructure error: requeue for bounded retry.
		_ = st.RequeueReproAttemptOnInfraError(ctx, finding.Fingerprint, err.Error())
		outcome.Reason = "error: " + err.Error()
		outcome.Err = err
		return outcome, err
	}

	// Attempt completed (success or definitive failure): mark done.
	_ = st.FinishReproAttempt(ctx, finding.Fingerprint)
	if att.Promoted {
		// A successful reproduction is definitive positive evidence — clear any
		// prior exit-zero contradiction signal so the finding is not simultaneously
		// Tier<=1 (reproduced) and repro-contradicted.
		_ = st.ZeroExitZeroCount(ctx, finding.Fingerprint)
	}

	outcome.Attempts = att.Attempts
	if att.Promoted {
		if finding.NeedsHuman || att.WitnessOnly {
			// Below-quorum verifier survivor (NeedsHuman), OR a demonstrated
			// bug whose detected ecosystem cannot provide an execution
			// witness for the target file (att.WitnessOnly, bugbot-qb4r layer
			// b): witness, do not promote. Tier, ReproPath, and NeedsHuman all
			// stay as-is; only ReproWitness is set. No patch-prover cascade:
			// the human gate stands for NeedsHuman, and a witness-only
			// ecosystem gets no automated fix cascade either — repro-as-
			// evidence, not repro-as-promotion.
			if werr := witnessFinding(ctx, st, finding, att.ArtifactPath); werr != nil {
				outcome.Reason = "witness persist failed: " + werr.Error()
				outcome.Err = werr
				return outcome, werr
			}
			outcome.Witnessed = true
			outcome.ArtifactPath = att.ArtifactPath
		} else {
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
					outcome.FixWitnessed = patchResult.kind == patchOutcomeFixWitnessed
					outcome.NeedsHuman = patchResult.kind == patchOutcomeNeedsHuman
					if patchResult.kind == patchOutcomeSkipped {
						outcome.Reason = "patch-prover skipped: toolchain not identified and repro.suite_cmd not configured"
					}
				}
			}
		}
	} else {
		outcome.Reason = att.Reason
		// Record exit-zero outcomes (test ran, bug did not manifest) against the
		// repro_attempts row. This is distinct from infrastructure errors
		// (already handled above via RequeueReproAttemptOnInfraError) and from
		// successful repros (att.Promoted). Only exit_zero counts toward the
		// repro-contradiction signal; other non-promotion reasons (build_error,
		// toolchain_error, not_demonstrated, timeout) do not.
		if att.Reason == string(VerdictReasonExitZero) {
			outcome.ExitZero = true
			_ = st.RecordExitZeroAttempt(ctx, finding.Fingerprint)
		}
	}

	return outcome, nil
}

// patchOutcomeKind is the discriminant for a patch-prover run. Exactly one
// state applies; contradictory combinations are unrepresentable.
type patchOutcomeKind int

const (
	// iota 0 is reserved as a neutral/invalid zero value so a zero patchOutcome{}
	// (e.g. returned alongside an error) is never mistaken for FixWitnessed and
	// cannot drive a spurious Tier-0 promotion. Callers read kind only when err==nil.
	_ patchOutcomeKind = iota
	// patchOutcomeFixWitnessed: both targeted and suite runs passed with the
	// fix applied — the finding is promoted to Tier-0.
	patchOutcomeFixWitnessed
	// patchOutcomeNeedsHuman: all fix-plan attempts were exhausted without a
	// passing run — a human reviewer is required.
	patchOutcomeNeedsHuman
	// patchOutcomeSkipped: the repo's toolchain could not be identified and no
	// repro.suite_cmd is configured — the prover declines rather than guesses.
	patchOutcomeSkipped
)

// patchOutcome is the result of a patch-prover run on a single finding.
type patchOutcome struct {
	kind patchOutcomeKind
}

// provePatch runs the patch-prover on a finding that was just promoted to T1.
// It either witnesses a fix (T0) or records needs-human on exhaustion.
func (r *Reproducer) provePatch(ctx context.Context, st *store.Store, f domain.Finding, att *Attempt) (patchOutcome, error) {
	prover := &PatchProver{
		client:        r.client,
		sb:            r.sb,
		repoDir:       r.repoDir,
		maxAttempts:   r.opts.PatchMaxAttempts,
		timeout:       r.opts.Timeout,
		image:         r.opts.Image,
		artifactDir:   r.opts.ArtifactDir,
		agentLimits:   r.opts.AgentLimits,
		transcriptDir: r.opts.TranscriptDir,
		suiteCmd:      r.opts.PatchSuiteCmd,
		depMounts:     r.deps.ROMounts,
		depEnv:        r.deps.Env,
		setupCmds:     r.deps.SetupCmds,
		progress:      r.opts.Progress,
		statusNotes:   r.opts.StatusNotes,
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
func promoteFinding(ctx context.Context, st *store.Store, f domain.Finding, reproPath string) error {
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

// witnessFinding records a non-promoting repro artifact for a below-quorum
// (NeedsHuman) finding. It mirrors promoteFinding but writes ONLY repro_witness;
// Tier, ReproPath, and NeedsHuman are left untouched and the patch-prover
// cascade is intentionally NOT triggered. The human reviewer gets a concrete
// repro bundle to run; downstream automation continues to honor the human gate
// because OpenBacklog and the patch-prover still skip NeedsHuman findings.
//
// bugbot-w1bh: this is the witness half of the repro-as-evidence vs
// repro-as-promotion split. PromoteOne branches to it whenever the
// demonstrated finding has NeedsHuman set.
func witnessFinding(ctx context.Context, st *store.Store, f domain.Finding, reproPath string) error {
	current, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		return err
	}
	current.ReproWitness = reproPath
	if _, err := st.UpsertFinding(ctx, current); err != nil {
		return err
	}
	return nil
}

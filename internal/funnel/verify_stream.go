package funnel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// runVerifyAndPersist is the per-candidate unit body for the streaming
// topology. The caller runs it inside a HIGH-priority slot that the verify
// fanout holds for the whole sequential refuter panel + arbiter; it runs the
// panel + arbiter (reusing the existing runRefuters + runArbiter machinery) and
// immediately persists the outcome (survivor → Tier 2, orphaned → Tier 3
// suspected).
//
// This function preserves EVERY path from the original verify.go goroutine:
//   - unanimous kill/survive
//   - split → arbiter both ways
//   - arbiter parse-fail fallback (majorityRefuted)
//   - budget orphan mid-panel AND mid-arbiter
//   - agent_units row per candidate (KindFindingKilled/Verified emits)
//   - seat names in traces
//
// It appends the result to *allFindings under findingsMu, increments *killed,
// and calls setErr on fatal errors (ctx cancel, store I/O failure). It uses the
// shared clusterRegistry reg to attach staged corroborating lenses from triage
// at persist time.
//
// sbExecs/sbMillis are shared atomic counters across all candidates.
//
// reproQ, when non-nil, receives each survived Tier-2 finding for in-run
// reproduction; see reproQueue for the never-block contract.
func (f *Funnel) runVerifyAndPersist(
	ctx context.Context,
	verifier llm.Client,
	persona string,
	c Candidate,
	candIdx int,
	commit string,
	fps map[string]string,
	budget *budgetState,
	result *Result,
	reg *clusterRegistry,
	findingsMu *sync.Mutex,
	allFindings *[]store.Finding,
	killedPtr *int,
	sbExecs *atomic.Int32,
	sbMillis *atomic.Int64,
	setErr func(error),
	reproQ *reproQueue,
) {
	// The verify fanout already holds this candidate's HIGH-priority slot
	// (verifier is cheap, latency-sensitive, and gates everything downstream) for
	// the whole sequential refuter panel + arbiter; ctx is the fanout's runCtx.

	// Hard budget gate: orphan without verifying.
	if budget.verifyOverHard() {
		budget.stopped.Store(true)
		msg := fmt.Sprintf("hard budget reached: verification skipped for %q (%s:%d) — kept as T3 suspected", c.Title, c.File, c.Line)
		f.note(result, msg)
		progress.Emit(f.opts.Progress, progress.Event{Kind: progress.KindBudgetStopped, Message: msg})
		f.recordVerifierUnit(ctx, result.ScanRunID, c.Lens, c.File, candIdx,
			time.Time{}, time.Time{}, 0, "orphaned_budget", nil, nil, false, false, result)
		// Persist as T3 suspected.
		suspected := persistOrphan(ctx, f, c, commit, fps, result)
		if suspected != nil {
			findingsMu.Lock()
			*allFindings = append(*allFindings, *suspected)
			findingsMu.Unlock()
			// Durably kept as T3 suspected: drop the WAL row. A failed or
			// suppressed orphan (suspected == nil) leaves the row for the next
			// run, where triage self-heals it (re-orphan or suppression drop).
			f.deletePending(ctx, c.PendingID, result)
		}
		if late := reg.SignalPersisted(c.Fingerprint, suspected != nil); len(late) > 0 {
			// Lenses staged between drain and persist (TOCTOU window): attach
			// best-effort via the store path.
			if err := f.store.AddCorroboratingLenses(ctx, c.Fingerprint, late); err != nil {
				f.note(result, fmt.Sprintf("corroboration: late attach to suspected %q failed: %v", c.Title, err))
			}
		}
		return
	}

	nRefuters := f.opts.Limits.Refuters
	if budget.verifyOverSoft() {
		budget.degraded.Store(true)
		if nRefuters > degradedRefuters {
			nRefuters = degradedRefuters
			msg := fmt.Sprintf("budget degraded: %q verified with %d refuter(s)", c.Title, degradedRefuters)
			f.note(result, msg)
			progress.Emit(f.opts.Progress, progress.Event{Kind: progress.KindBudgetDegraded, Message: msg})
		}
	}

	// Verifier tools (no post_lead, looser read caps — same rationale as verify.go).
	tools, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		setErr(err)
		return
	}

	// Sandbox tools (if enabled for this candidate).
	candTools := tools
	if prefErr := f.ensureDepPrefetch(ctx); prefErr != nil {
		f.note(result, fmt.Sprintf("sandbox dependency prefetch failed: %v — sandbox_exec disabled", prefErr))
	} else {
		if sbTool := f.buildSandboxTool(c, sbExecs, sbMillis); sbTool != nil {
			candTools = append(candTools, sbTool)
		}
		if rtTool := f.buildRunTestsTool(sbExecs, sbMillis); rtTool != nil {
			candTools = append(candTools, rtTool)
		}
	}
	if t := f.maybeStatusNoteTool(progress.RoleVerifier, c.Title); t != nil {
		candTools = append(candTools, t)
	}

	sink := f.opts.Progress
	startedAt := time.Now()
	scope := progress.NewAgentScope(sink, progress.RoleVerifier, c.Title).Start()
	verdicts, seatNames, tokens, nFailed, stopped, err := f.runRefuters(ctx, verifier, candTools, persona, c, nRefuters, budget)

	// Arbiter path.
	var localArbiterRuns, localArbiterKills, localArbiterFailed int
	var arbiterReasoning string
	var arbiterVerdict *refutation
	arbiterBudgetStopped := false
	if err == nil && !stopped && isSplitVerdict(verdicts) {
		localArbiterRuns = 1
		av, aTokens, aStopped, aErr := f.runArbiter(ctx, verifier, candTools, persona, c, verdicts, seatNames, budget)
		tokens += aTokens
		if aStopped {
			arbiterBudgetStopped = true
		} else if aErr != nil && ctx.Err() == nil {
			localArbiterFailed = 1
		} else if aErr == nil {
			arbiterVerdict = av
			if av != nil && av.Refuted {
				localArbiterKills = 1
			}
			hallucinatedNote := ""
			if av != nil && av.HallucinatedRebuttal {
				hallucinatedNote = " [hallucinated rebuttal detected]"
			}
			arbiterReasoning = fmt.Sprintf("arbiter [%s, confidence=%s]%s: %s",
				verdictWord(av), av.Confidence, hallucinatedNote, strings.TrimSpace(av.Reasoning))
		}
	}

	finishedAt := time.Now()
	scope.Finish(tokens, finishedAt.Sub(startedAt), err)

	// Error path: fatal (ctx cancel or unexpected runner error).
	if err != nil {
		setErr(err)
		return
	}

	recordStatus := ""
	var candKilled bool
	var wasStopped bool

	if stopped || arbiterBudgetStopped {
		// Budget stopped mid-verification.
		budget.stopped.Store(true)
		wasStopped = true
		msg := fmt.Sprintf("budget stopped mid-verification of %q (%s:%d) — kept as T3 suspected", c.Title, c.File, c.Line)
		f.note(result, msg)
		progress.Emit(sink, progress.Event{Kind: progress.KindBudgetStopped, Message: msg})
		recordStatus = "orphaned_budget"
	} else {
		// Fold verifier-side stats (under findingsMu to keep them consistent with
		// the findings slice). We fold individual candidate stats atomically here.
		findingsMu.Lock()
		result.Stats.VerifierRuns += len(verdicts)
		result.Stats.VerifierFailures += nFailed
		result.Stats.ArbiterRuns += localArbiterRuns
		result.Stats.ArbiterKills += localArbiterKills
		result.Stats.ArbiterFailures += localArbiterFailed
		findingsMu.Unlock()

		if nFailed > 0 {
			progress.Emit(sink, progress.Event{
				Kind: progress.KindLensFailed, Role: progress.RoleVerifier, Label: c.Title,
				Message: fmt.Sprintf("%d/%d refuter(s) produced no parseable verdict for %q — treated as 'could not refute'", nFailed, len(verdicts), c.Title),
			})
		}

		if isSplitVerdict(verdicts) {
			if localArbiterFailed > 0 || arbiterVerdict == nil {
				candKilled = majorityRefuted(verdicts)
			} else {
				candKilled = arbiterVerdict.Refuted
			}
		} else {
			candKilled = majorityRefuted(verdicts)
		}

		if candKilled {
			findingsMu.Lock()
			*killedPtr++
			findingsMu.Unlock()
			recordStatus = "killed"
			progress.Emit(sink, progress.Event{
				Kind: progress.KindFindingKilled, Title: c.Title, File: c.File, Line: c.Line,
			})
		} else {
			recordStatus = "survived"
			progress.Emit(sink, progress.Event{
				Kind: progress.KindFindingVerified, Title: c.Title, File: c.File, Line: c.Line,
			})
		}
	}

	// Record the verifier agent_units row.
	arbiterRan := localArbiterRuns > 0 && localArbiterFailed == 0 && !arbiterBudgetStopped
	f.recordVerifierUnit(ctx, result.ScanRunID, c.Lens, c.File, candIdx,
		startedAt, finishedAt, tokens, recordStatus, seatNames, seatRefutedSlice(verdicts),
		arbiterRan, arbiterRefuted(arbiterVerdict), result)

	// Immediate persistence (Stage D in the streaming topology).
	if wasStopped {
		// Orphaned: persist as T3 suspected.
		suspected := persistOrphan(ctx, f, c, commit, fps, result)
		if suspected != nil {
			findingsMu.Lock()
			*allFindings = append(*allFindings, *suspected)
			findingsMu.Unlock()
			// Durably kept as T3 suspected: drop the WAL row (see the hard-budget
			// orphan above). A failed/suppressed orphan self-heals on the next run.
			f.deletePending(ctx, c.PendingID, result)
		}
		if late := reg.SignalPersisted(c.Fingerprint, suspected != nil); len(late) > 0 {
			// Lenses staged between drain and persist (TOCTOU window): attach
			// best-effort via the store path.
			if err := f.store.AddCorroboratingLenses(ctx, c.Fingerprint, late); err != nil {
				f.note(result, fmt.Sprintf("corroboration: late attach to suspected %q failed: %v", c.Title, err))
			}
		}
		return
	}
	if candKilled {
		// Killed: signal so triage can discard any staged corroboration.
		reg.SignalPersisted(c.Fingerprint, false)
		// Killed: terminal, but nothing durable is persisted (only a Stats
		// counter), so drop the WAL row or it would replay and be re-killed
		// every run.
		f.deletePending(ctx, c.PendingID, result)
		return
	}

	// Survived: build the reasoning trace with staged corroboration, then persist.
	stagedLenses := reg.DrainStagedLenses(c.Fingerprint)
	allLenses := dedupLenses(append(c.CorroboratingLenses, stagedLenses...))
	c.CorroboratingLenses = allLenses

	// Quorum check: require a strict majority of the panel to have examined the
	// code. Abstaining seats (CouldNotReadCode) do not count toward quorum.
	// Below floor: survivor is flagged NeedsHuman so a human confirms the result
	// rather than silently promoting a finding examined by too few seats.
	//
	// NeedsHuman dual meaning: this field is also set by the repro/patch-prover
	// when it exhausts its attempt budget (repro_hook.go). Both meanings cause
	// the finding to be excluded from the repro backlog (daemon/backlog.go) and
	// to render the 'needs human review' copy in CLI/report output. The verifier
	// sets it here for a different reason (below-quorum abstentions) but accepts
	// those downstream effects deliberately: a below-quorum survivor should not
	// receive a repro attempt until a human has confirmed the finding. A separate
	// bead tracks updating the downstream copy to distinguish the two causes.
	examined := examinedVerdicts(verdicts)
	needsHuman := belowQuorum(len(examined), len(verdicts))
	var quorumNote string
	if needsHuman {
		quorumNote = fmt.Sprintf("\nNOTE: only %d/%d panel seat(s) examined the code (below quorum floor); human review required.", len(examined), len(verdicts))
	}

	reasoning := buildReasoning(verdicts, seatNames, arbiterReasoning, arbiterRan) + quorumNote
	// Drain sites staged by root-cause-merged members that arrived in triage
	// before this verify goroutine reached this point.
	stagedSites := reg.DrainStagedSites(c.Fingerprint)
	allSites := candidateSitesToStore(c.Sites)
	allSites = append(allSites, stagedSites...)
	v := verified{cand: c, reasoning: reasoning}

	// nn3: fold mechanism corrections into the persisted description.
	//
	// Priority: arbiter > refuter (highest-confidence examined not-refuted seat).
	// The arbiter path fires on split panels; the refuter path fires on unanimous-
	// survive panels where a seat set corrected_description (refuted=false means
	// the seat did not claim to refute, just noted the mechanism was wrong).
	// Either way we prefer the arbiter's synthesis when it ran.
	description := c.Description
	if arbiterVerdict != nil && arbiterVerdict.CorrectedDescription != "" {
		// Split panel: arbiter synthesized the correction.
		description = arbiterVerdict.CorrectedDescription
	} else if !arbiterRan {
		// Unanimous-survive (or no-arbiter) path: fold the highest-confidence
		// non-empty CorrectedDescription from an examined not-refuted seat.
		description = bestRefuterCorrection(examined, c.Description)
	}

	finding := store.Finding{
		Fingerprint:         c.Fingerprint,
		Title:               c.Title,
		Description:         description,
		Reasoning:           appendCorroboration(v.reasoning, c.CorroboratingLenses),
		Severity:            c.Severity,
		Tier:                domain.TierVerified,
		Status:              store.StatusOpen,
		Lens:                c.Lens,
		File:                c.File,
		Line:                c.Line,
		CommitSHA:           commit,
		FileHash:            fps[c.File],
		CorroboratingLenses: c.CorroboratingLenses,
		NeedsHuman:          needsHuman,
		Sites:               allSites,
	}
	stored, err := f.store.UpsertFinding(ctx, finding)
	if err != nil {
		setErr(fmt.Errorf("funnel: upsert finding %q: %w", c.Title, err))
		reg.SignalPersisted(c.Fingerprint, false)
		return
	}
	// Honor suppression memory: a forced-dismissed finding must not be reported.
	if stored.Status != store.StatusOpen {
		// Durably written as dismissed (suppression memory): terminal. Drop the
		// WAL row so it does not replay.
		f.deletePending(ctx, c.PendingID, result)
		reg.SignalPersisted(c.Fingerprint, false)
		return
	}
	// Survived and durably persisted as T2: drop the WAL row.
	f.deletePending(ctx, c.PendingID, result)
	// Atomically mark persisted and collect any lenses AND sites staged since
	// the drain above — the TOCTOU window where a triage member arrived after
	// DrainStagedLenses/DrainStagedSites but before this signal.
	if late := reg.SignalPersisted(c.Fingerprint, true); len(late) > 0 {
		if err := f.store.AddCorroboratingLenses(ctx, c.Fingerprint, late); err != nil {
			f.note(result, fmt.Sprintf("corroboration: late attach to %q failed: %v", c.Title, err))
		}
		// Fold TOCTOU lenses into stored so allFindings reflects them in both
		// CorroboratingLenses and Reasoning. The Reasoning fold must happen here
		// (not deferred to run.go's AttachedLenses loop) because run.go computes
		// `added = lenses not already in stored.CorroboratingLenses`; once we put
		// `late` into stored.CorroboratingLenses below, run.go's `added` is []
		// and it would never append the human-readable note.
		stored.CorroboratingLenses = dedupLenses(append(stored.CorroboratingLenses, late...))
		stored.Reasoning = appendCorroboration(stored.Reasoning, late)
	}
	// TOCTOU window for sites: any staged site that arrived between DrainStagedSites
	// and SignalPersisted was moved to lateSites by SignalPersisted; retrieve,
	// persist, and fold into the in-memory stored finding.
	if lateSites := reg.DrainLateSites(c.Fingerprint); len(lateSites) > 0 {
		if err := f.store.AppendFindingSites(ctx, c.Fingerprint, lateSites); err != nil {
			f.note(result, fmt.Sprintf("sites: late attach to %q failed: %v", c.Title, err))
		} else {
			stored.Sites = append(stored.Sites, lateSites...)
		}
	}
	findingsMu.Lock()
	*allFindings = append(*allFindings, stored)
	findingsMu.Unlock()

	// Enqueue for in-run reproduction. Never blocks (see reproQueue); only
	// Tier-2 (survived, not orphaned/suspected) findings are enqueued.
	if reproQ != nil {
		reproQ.enqueue(stored)
	}
}

// persistOrphan persists a budget-orphaned candidate as a Tier 3 suspected
// finding. Returns a pointer to the stored finding on success, nil on failure
// or suppression. Best-effort: errors are noted but do not abort the run.
func persistOrphan(ctx context.Context, f *Funnel, c Candidate, commit string, fps map[string]string, result *Result) *store.Finding {
	finding := store.Finding{
		Fingerprint:         c.Fingerprint,
		Title:               c.Title,
		Description:         c.Description,
		Reasoning:           appendCorroboration(budgetStoppedReasoning, c.CorroboratingLenses),
		Severity:            c.Severity,
		Tier:                domain.TierSuspected,
		Status:              store.StatusOpen,
		Lens:                c.Lens,
		File:                c.File,
		Line:                c.Line,
		CommitSHA:           commit,
		FileHash:            fps[c.File],
		CorroboratingLenses: c.CorroboratingLenses,
		Sites:               candidateSitesToStore(c.Sites),
	}
	stored, err := f.store.UpsertFinding(ctx, finding)
	if err != nil {
		f.note(result, fmt.Sprintf("funnel: upsert suspected finding %q: %v", c.Title, err))
		return nil
	}
	if stored.Status != store.StatusOpen {
		return nil
	}
	msg := fmt.Sprintf("budget stop: %q (%s:%d) kept as T3 suspected", c.Title, c.File, c.Line)
	f.note(result, msg)
	return &stored
}

// candidateSitesToStore converts funnel.Site to store.Site.
func candidateSitesToStore(sites []Site) []store.Site {
	if len(sites) == 0 {
		return nil
	}
	out := make([]store.Site, len(sites))
	for i, s := range sites {
		out[i] = store.Site{File: s.File, Line: s.Line}
	}
	return out
}

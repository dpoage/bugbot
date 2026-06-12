package funnel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// runVerifyAndPersist is the per-candidate goroutine body for the streaming
// topology. It acquires a HIGH-priority slot, runs the refuter panel + arbiter
// (reusing the existing runRefuters + runArbiter machinery), and immediately
// persists the outcome (survivor → Tier 2, orphaned → Tier 3 suspected).
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
// reproCh, when non-nil, receives each Tier-2 finding for in-run reproduction.
// The enqueue is NON-BLOCKING: if the channel buffer is full, the finding is
// appended to overflowFindings (under overflowMu) for a post-verify catch-up
// pass. This ensures the verify goroutine is NEVER blocked by a slow repro
// backlog. overflowMu/overflowFindings may be nil when reproCh is nil.
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
	reproCh chan store.Finding,
	overflowMu *sync.Mutex,
	overflowFindings *[]store.Finding,
) {
	// Acquire a global slot (HIGH priority: verifier is cheap, latency-sensitive,
	// and gates everything downstream). One slot covers the whole sequential
	// refuter panel + arbiter for this candidate.
	if err := f.slots.acquire(ctx, slotHigh); err != nil {
		return
	}
	defer f.slots.release()

	// Hard budget gate: orphan without verifying.
	if budget.overHard() {
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

	nRefuters := f.opts.Refuters
	if budget.overSoft() {
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

	// Sandbox tool (if enabled for this candidate).
	candTools := tools
	if prefErr := f.ensureDepPrefetch(ctx); prefErr != nil {
		f.note(result, fmt.Sprintf("sandbox dependency prefetch failed: %v — sandbox_exec disabled", prefErr))
	} else if sbTool := f.buildSandboxTool(c, sbExecs, sbMillis); sbTool != nil {
		candTools = append(candTools, sbTool)
	}

	sink := f.opts.Progress
	startedAt := time.Now()
	progress.Emit(sink, progress.Event{
		Kind: progress.KindAgentStarted, Role: progress.RoleVerifier, Label: c.Title,
	})
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
			arbiterReasoning = fmt.Sprintf("arbiter [%s, confidence=%s]: %s",
				verdictWord(av), av.Confidence, strings.TrimSpace(av.Reasoning))
		}
	}

	finishedAt := time.Now()
	progress.Emit(sink, progress.Event{
		Kind: progress.KindAgentFinished, Role: progress.RoleVerifier, Label: c.Title,
		Tokens: tokens, Duration: finishedAt.Sub(startedAt), Err: errString(err),
	})

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
		return
	}

	// Survived: build the reasoning trace with staged corroboration, then persist.
	stagedLenses := reg.DrainStagedLenses(c.Fingerprint)
	allLenses := dedupLenses(append(c.CorroboratingLenses, stagedLenses...))
	c.CorroboratingLenses = allLenses

	reasoning := buildReasoning(verdicts, seatNames, arbiterReasoning, arbiterRan)
	v := verified{cand: c, reasoning: reasoning}

	finding := store.Finding{
		Fingerprint:         c.Fingerprint,
		Title:               c.Title,
		Description:         c.Description,
		Reasoning:           appendCorroboration(v.reasoning, c.CorroboratingLenses),
		Severity:            c.Severity,
		Tier:                tierVerified,
		Status:              store.StatusOpen,
		Lens:                c.Lens,
		File:                c.File,
		Line:                c.Line,
		CommitSHA:           commit,
		FileHash:            fps[c.File],
		CorroboratingLenses: c.CorroboratingLenses,
	}
	stored, err := f.store.UpsertFinding(ctx, finding)
	if err != nil {
		setErr(fmt.Errorf("funnel: upsert finding %q: %w", c.Title, err))
		reg.SignalPersisted(c.Fingerprint, false)
		return
	}
	// Honor suppression memory: a forced-dismissed finding must not be reported.
	if stored.Status != store.StatusOpen {
		reg.SignalPersisted(c.Fingerprint, false)
		return
	}
	findingsMu.Lock()
	*allFindings = append(*allFindings, stored)
	findingsMu.Unlock()
	// Atomically mark persisted and collect any lenses staged since the drain
	// above — the TOCTOU window where a triage member arrived after
	// DrainStagedLenses but before this signal. Without this, such a lens is
	// stranded: staged (so triage skipped the store path) but never attached.
	if late := reg.SignalPersisted(c.Fingerprint, true); len(late) > 0 {
		if err := f.store.AddCorroboratingLenses(ctx, c.Fingerprint, late); err != nil {
			f.note(result, fmt.Sprintf("corroboration: late attach to %q failed: %v", c.Title, err))
		}
	}

	// Enqueue for in-run reproduction (non-blocking). The verify goroutine must
	// NEVER block here: if the reproCh buffer is full, fall back to the overflow
	// slice so a slow repro backlog cannot stall verification. The claim check
	// inside runReproAttempt prevents double-attempts when both paths are active.
	// Only enqueue Tier-2 (survived, not orphaned/suspected) findings.
	if reproCh != nil {
		select {
		case reproCh <- stored:
			// fast path: slot available
		default:
			// buffer full: stage in overflow for the post-verify catch-up pass
			overflowMu.Lock()
			*overflowFindings = append(*overflowFindings, stored)
			overflowMu.Unlock()
		}
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
		Tier:                tierSuspected,
		Status:              store.StatusOpen,
		Lens:                c.Lens,
		File:                c.File,
		Line:                c.Line,
		CommitSHA:           commit,
		FileHash:            fps[c.File],
		CorroboratingLenses: c.CorroboratingLenses,
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

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
			time.Time{}, time.Time{}, 0, "orphaned_budget", nil, nil, 0, false, false, result)
		// Persist as T3 suspected.
		suspected := persistOrphan(ctx, f, c, commit, fps, budgetStoppedReasoning, result)
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

	// Refuter tools: repo-rooted read_file (no post_lead, looser read caps — same
	// rationale as verify.go). The arbiter gets the same toolbox but with the
	// dep-source read reach (GOROOT/src + Go module cache) wired onto read_file,
	// so a split arbiter can verify a cited stdlib/third-party claim by reading
	// the actual source (bugbot-mi5.17/.18); refuters stay repo-rooted to keep
	// their per-seat read scope and token cost bounded.
	refuterReadTools, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		setErr(err)
		return
	}

	// Sandbox / run_tests / status_note tools are shared verbatim by the refuter
	// panel and the arbiter (so the per-candidate sandbox exec budget spans the
	// whole panel + arbiter, exactly as before this split).
	var extra []agent.Tool
	if prefErr := f.ensureDepPrefetch(ctx); prefErr != nil {
		f.note(result, fmt.Sprintf("sandbox dependency prefetch failed: %v — sandbox_exec disabled", prefErr))
	} else {
		if sbTool := f.buildSandboxTool(c, sbExecs, sbMillis); sbTool != nil {
			extra = append(extra, sbTool)
		}
		if rtTool := f.buildRunTestsTool(sbExecs, sbMillis); rtTool != nil {
			extra = append(extra, rtTool)
		}
	}
	if t := f.maybeStatusNoteTool(progress.RoleVerifier, c.Title); t != nil {
		extra = append(extra, t)
	}
	if t := f.maybeReportToolIssueTool(result, progress.RoleVerifier, c.Title); t != nil {
		extra = append(extra, t)
	}
	// candTools is the refuter panel's tool set (repo-rooted read). The arbiter's
	// dep-reach tool set is built lazily on a split (see below), reusing the same
	// extra tool VALUES.
	candTools := append(refuterReadTools, extra...)

	sink := f.opts.Progress
	startedAt := time.Now()
	scope := progress.NewAgentScope(sink, progress.RoleVerifier, c.Title).Start()
	verdicts, seatNames, tokens, nFailed, stopped, err := f.runRefuters(ctx, verifier, candTools, persona, c, nRefuters, budget, f.toolHealthSinkFor(result, progress.RoleVerifier, c.Title))

	// Arbiter path.
	var localArbiterRuns, localArbiterKills, localArbiterFailed int
	var localArbiterTokens int64
	var arbiterReasoning string
	var arbiterVerdict *refutation
	arbiterBudgetStopped := false
	if err == nil && !stopped && isSplitVerdict(verdicts) {
		localArbiterRuns = 1
		// Build the arbiter's tool set lazily: it carries dep-source read reach
		// and is only needed on the rare split, so we do not pay to construct it
		// on every candidate. It reuses the same extra tool VALUES as the panel.
		arbiterReadTools, atErr := f.readOnlyToolsWithDepRoots(agent.ReadCaps{})
		if atErr != nil {
			setErr(atErr)
			return
		}
		arbiterTools := append(arbiterReadTools, extra...)
		av, aTokens, aStopped, aErr := f.runArbiter(ctx, verifier, arbiterTools, persona, c, verdicts, seatNames, budget, f.toolHealthSinkFor(result, progress.RoleVerifier, c.Title))
		tokens += aTokens
		localArbiterTokens = aTokens
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
			evidenceNote := ""
			if av != nil && len(av.Evidence) > 0 {
				evidenceNote = " [evidence: " + strings.Join(av.Evidence, "; ") + "]"
			}
			arbiterReasoning = fmt.Sprintf("arbiter [%s, confidence=%s]%s%s: %s",
				verdictWord(av), av.Confidence, hallucinatedNote, evidenceNote, strings.TrimSpace(av.Reasoning))
		}
	}
	if localArbiterRuns > 0 {
		// Arbiter cost + starvation accounting (bugbot-mi5.17 AC6). Folded here,
		// not in the survive/kill stats block below, so a budget-stopped arbiter
		// — which orphans the candidate and never reaches that block — still has
		// its spend and stop counted.
		findingsMu.Lock()
		result.Stats.ArbiterTokens += localArbiterTokens
		if arbiterBudgetStopped {
			result.Stats.ArbiterBudgetStops++
		}
		findingsMu.Unlock()
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
	var verifyFailed bool
	var orphanReasoning string

	if stopped || arbiterBudgetStopped {
		// Budget stopped mid-verification.
		budget.stopped.Store(true)
		wasStopped = true
		msg := fmt.Sprintf("budget stopped mid-verification of %q (%s:%d) — kept as T3 suspected", c.Title, c.File, c.Line)
		f.note(result, msg)
		progress.Emit(sink, progress.Event{Kind: progress.KindBudgetStopped, Message: msg})
		orphanReasoning = budgetStoppedReasoning
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
				Message: fmt.Sprintf("%d/%d refuter(s) produced no parseable verdict for %q", nFailed, len(verdicts), c.Title),
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
		} else if genuine := genuineVerdicts(verdicts); len(genuine) == 0 && nFailed > 0 {
			// Verification reached NO genuine verdict: every seat abstained or
			// failed, with at least one infrastructure/parse failure. The
			// candidate was never actually challenged, so promoting it as a
			// confident survivor would report a finding no refuter examined
			// (bugbot-8rd). Orphan it as T3 suspected; the next scan re-verifies.
			verifyFailed = true
			orphanReasoning = verifyFailedReasoning
			recordStatus = "orphaned_verify_failed"
			msg := fmt.Sprintf("verification failed: %d/%d refuter(s) returned no verdict for %q (%s:%d) — kept as T3 suspected", nFailed, len(verdicts), c.Title, c.File, c.Line)
			f.note(result, msg)
			progress.Emit(sink, progress.Event{
				Kind: progress.KindLensFailed, Role: progress.RoleVerifier, Label: c.Title, Message: msg,
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
		nFailed, arbiterRan, arbiterRefuted(arbiterVerdict), result)

	// Immediate persistence (Stage D in the streaming topology).
	if wasStopped || verifyFailed {
		// Orphaned (budget stop or verification failure): persist as T3 suspected.
		suspected := persistOrphan(ctx, f, c, commit, fps, orphanReasoning, result)
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
		if c.Reverify {
			// Reconstructed T3 finding refuted on re-verification: it has a
			// durable open row the normal kill path (persists nothing) would
			// leave open forever. Transition it to dismissed with a
			// re-verification-specific reason so the suppression table records
			// the event (UpdateStatus(StatusDismissed) doubles as a permanent
			// suppression, so the same fingerprint will never resurface).
			if err := f.store.UpdateStatus(ctx, c.Fingerprint, store.StatusDismissed,
				"re-verification refuted this previously-suspected (Tier 3) finding"); err != nil {
				f.note(result, fmt.Sprintf("reverify: dismiss refuted T3 %q failed: %v", c.Title, err))
			}
		}
		// Killed: terminal, but nothing durable is persisted (only a Stats
		// counter), so drop the WAL row or it would replay and be re-killed
		// every run. deletePending is a no-op when id=="" (reverify path has
		// no WAL row), so the durable dismiss above is the only state change.
		f.deletePending(ctx, c.PendingID, result)
		return
	}

	// Survived: build the reasoning trace with staged corroboration, then persist.
	stagedLenses := reg.DrainStagedLenses(c.Fingerprint)
	allLenses := dedupLenses(append(c.CorroboratingLenses, stagedLenses...))
	c.CorroboratingLenses = allLenses

	// Quorum check: require a strict majority of the panel to have produced a
	// GENUINE verdict (genuineVerdicts: neither an abstention nor a no-verdict
	// infra/parse failure). Below floor: survivor is flagged NeedsHuman so a
	// human confirms the result rather than silently promoting a finding too few
	// seats actually judged. (A panel with ZERO genuine verdicts and a failure
	// was already orphaned above; reaching here means at least one seat judged.)
	//
	// NeedsHuman dual meaning: this field is also set by the repro/patch-prover
	// when it exhausts its attempt budget (repro_hook.go). Both meanings cause
	// the finding to be excluded from the repro backlog (daemon/backlog.go) and
	// to render the 'needs human review' copy in CLI/report output. The verifier
	// sets it here for a different reason (below-quorum genuine verdicts) but
	// accepts those downstream effects deliberately: a below-quorum survivor
	// should not receive a repro attempt until a human has confirmed the
	// finding. A separate bead tracks updating the downstream copy to
	// distinguish the two causes.
	genuine := genuineVerdicts(verdicts)
	needsHuman := belowQuorum(len(genuine), len(verdicts))
	var quorumNote string
	if needsHuman {
		quorumNote = fmt.Sprintf("\nNOTE: only %d/%d panel seat(s) produced a verdict (below quorum floor); human review required.", len(genuine), len(verdicts))
	}

	reasoning := buildReasoning(verdicts, seatNames, arbiterReasoning, arbiterRan) + quorumNote
	// Drain sites staged by root-cause-merged members that arrived in triage
	// before this verify goroutine reached this point.
	stagedSites := reg.DrainStagedSites(c.Fingerprint)
	allSites := dedupSites(append(candidateSitesToStore(c.Sites), stagedSites...))
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
		description = bestRefuterCorrection(genuine, c.Description)
	}

	// Inline severity validation: run the reachability ladder now so a T2
	// survivor is persisted with its validated severity and a swept_at stamp,
	// instead of carrying the raw finder severity until the post-scan
	// drainToFixpoint pass. SweepDrain then only needs to reconcile
	// stranded/interrupted findings (bugbot-596).
	sev, verdictDetail, swept := f.validateSeverityInline(ctx, c, verifier, budget, result)
	var sweptAt time.Time
	if swept {
		sweptAt = time.Now().UTC()
	}
	finding := store.Finding{
		Fingerprint:         c.Fingerprint,
		Title:               c.Title,
		Description:         description,
		Reasoning:           appendCorroboration(v.reasoning, c.CorroboratingLenses),
		VerdictDetail:       verdictDetail,
		Severity:            sev,
		Tier:                domain.TierVerified,
		Status:              store.StatusOpen,
		Lens:                c.Lens,
		File:                c.File,
		Line:                c.Line,
		CommitSHA:           commit,
		FileHash:            fps[c.File],
		LocusKey:            c.LocusKey,
		CorroboratingLenses: c.CorroboratingLenses,
		NeedsHuman:          needsHuman,
		Sites:               allSites,
		SweptAt:             sweptAt,
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
			stored.Sites = dedupSites(append(stored.Sites, lateSites...))
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

// persistOrphan persists an unverified candidate as a Tier 3 suspected finding,
// using reasoning as its recorded verification trace (budgetStoppedReasoning
// for a budget stop, verifyFailedReasoning for a panel that produced no
// verdict). Returns a pointer to the stored finding on success, nil on failure
// or suppression. Best-effort: errors are noted but do not abort the run.
func persistOrphan(ctx context.Context, f *Funnel, c Candidate, commit string, fps map[string]string, reasoning string, result *Result) *store.Finding {
	finding := store.Finding{
		Fingerprint:         c.Fingerprint,
		Title:               c.Title,
		Description:         c.Description,
		Reasoning:           appendCorroboration(reasoning, c.CorroboratingLenses),
		Severity:            c.Severity,
		Tier:                domain.TierSuspected,
		Status:              store.StatusOpen,
		Lens:                c.Lens,
		File:                c.File,
		Line:                c.Line,
		CommitSHA:           commit,
		FileHash:            fps[c.File],
		LocusKey:            c.LocusKey,
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
	msg := fmt.Sprintf("%q (%s:%d) kept as T3 suspected", c.Title, c.File, c.Line)
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

// dedupSites removes duplicate (file,line) entries, preserving first-seen order.
// The primary's own site and a step-3 same-line collision can coincide; without
// this the finding would carry a duplicate site row (AppendFindingSites dedups
// the post-persist path, but the in-memory merge here did not).
func dedupSites(sites []store.Site) []store.Site {
	if len(sites) <= 1 {
		return sites
	}
	type key struct {
		f string
		l int
	}
	seen := make(map[key]bool, len(sites))
	out := make([]store.Site, 0, len(sites))
	for _, s := range sites {
		k := key{s.File, s.Line}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

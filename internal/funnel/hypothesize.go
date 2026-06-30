package funnel

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// diffIntentDiffCap is the maximum number of bytes of unified diff embedded in
// a diff-intent finder task. Beyond this limit the diff is truncated with an
// explicit marker so the model knows it is reading a partial diff, not the full
// change. 48 KB is large enough to cover most single-commit diffs while keeping
// the finder's context window from being dominated by raw diff bytes.
const diffIntentDiffCap = 48 * 1024

// diffIntentMsgCap is the maximum number of bytes of commit message embedded in
// a diff-intent finder task. Beyond this limit the message is truncated with an
// explicit marker. 4 KB comfortably covers any reasonable commit message body.
const diffIntentMsgCap = 4 * 1024

// unit is one finder work item: a (lens × strategy × chunk) triple. The
// customTask field is non-empty only for the diff-intent lens, which uses a
// pre-built task string rather than the standard file-list format. The struct
// is package-level so observability.go can reference it in the recording
// helpers without duplicating the definition.
type unit struct {
	lens       Lens
	strategy   Strategy
	files      []string
	langs      []ingest.Language // the chunk's language set, for prompt composition
	leads      []store.Lead      // pre-fetched leads for this lens, already consumed
	customTask string            // non-empty for diff-intent: overrides task(files, leads)
}

// buildUnits builds the unit-of-work list as (lens × strategy × chunk)
// triples in CHUNK-MAJOR order: every active lens (and applicable strategy)
// visits chunk 0 before any lens visits chunk 1. Within a chunk, lenses
// iterate in the caller-supplied yield order and strategies in builtin order
// (sweep-wide before deep).
//
// Chunk-major interleaving is a latency policy, not a budget policy: it gives
// every defect class — including low-yield lenses, whose units previously
// launched only after every higher-yield lens had covered the whole repo —
// running coverage within the first chunks of the sweep, so time-to-first-
// finding no longer scales with a lens's position in the yield ranking.
// Budget degradation is unaffected: the launch-loop gate checks each unit's
// (lens, strategy) class against the yield-ranked survivor set at launch
// time (degradedUnitClasses), which never depended on launch order. Under
// pressure the spend now distributes across all classes up to the soft
// threshold instead of exhausting the top lenses first; past the threshold
// only survivor-class units launch, exactly as before. Chunks arrive in the
// sweep's anti-starvation order (run.go), so the hottest/stalest files get
// full multi-lens coverage first.
//
// For each lens × chunk pair the default strategy (sweep-wide) is emitted
// exactly as before the strategy axis; additionally, each non-default builtin
// strategy that AppliesTo the lens emits one extra unit per chunk.
//
// diff-intent never gets chunk-based units here: it is either absent (sweeps,
// nil ChangeContext) or emitted by the caller as exactly ONE custom task
// prepended to the list. Skipping it ensures zero tasks from this lens on
// sweeps while still allowing the degradation logic to treat it as part of
// the set.
func buildUnits(lenses []Lens, strategies []Strategy, chunks []fileChunk, leadsByLens map[string][]store.Lead) []unit {
	var units []unit
	for _, c := range chunks {
		for _, l := range lenses {
			// Custom-unit lenses (per-chunk work would be the wrong
			// shape): diff-intent fires one task per commit-scoped run
			// and cross-language-boundary fires one task per seam.
			// Both are emitted by the caller as custom units adjacent
			// to this list, NEVER as chunk units, so skipping here
			// guarantees no per-chunk contamination and zero tasks on
			// runs where the caller chose not to emit them (e.g.
			// sweep with no seams).
			if l.Name == "diff-intent" || l.Name == "cross-language-boundary" {
				continue
			}
			if !lensAppliesTo(l, c.langs) {
				continue
			}
			for _, s := range strategies {
				if !s.AppliesTo(l.Name) {
					continue
				}
				units = append(units, unit{
					lens:     l,
					strategy: s,
					files:    c.files,
					langs:    c.langs,
					leads:    leadsByLens[l.Name],
				})
			}
		}
	}
	return units
}

// hypothesize runs the finder stage: for each effective lens, run a finder
// agent over each chunk of target files, emitting concrete candidates via the
// emit callback as each unit completes. Lens chunks run in parallel bounded by
// Options.MaxParallel. Budget degradation is applied as the run progresses:
// once over the soft threshold only the highest-yield lenses keep launching,
// and once over the hard threshold no new finder agents are launched.
//
// Cross-lens leads: before launching any finder units, we collect pending leads
// for each active lens, mark them consumed immediately (at claim time), and
// inject them into every finder task for that lens. Consuming at claim time
// means a failed finder run loses the lead — accepted trade-off documented
// here and in the store package. Leads targeting lenses not active this run
// stay pending and are consumed when that lens next runs.
//
// Diff-intent lens: when the run is commit-scoped (kind == ScanTargeted) AND cc
// is non-nil, exactly ONE extra finder task is emitted for the diff-intent lens
// before the per-chunk units. This task embeds the commit message, the unified
// diff (capped at diffIntentDiffCap), and the blast-radius dependents. No
// diff-intent tasks are emitted on sweeps or when cc is nil; lens selection and
// degradation tolerate a lens that emits zero chunk tasks (degradation
// survivors are computed from lenses that actually emitted units this run).
//
// fps is the per-file fingerprint map for coverage stamping. When touchCoverage
// is true (sweep path only), each finderOK unit's files are stamped via
// TouchScanCoverage immediately on completion — providing durable partial
// progress even if the run is cancelled before all units finish.
//
// Deliberate asymmetry: targeted scans pass touchCoverage=false and fps=nil so
// they never stamp coverage. Sweeps are the coverage source of truth; a file
// targeted by a commit-triggered scan still counts as due on the next sweep.
// This matches the documented asymmetry that was previously in run.go's
// run-end batch call (now replaced by this incremental approach).
//
// Files appearing in multiple finderOK units (multiple lenses × chunks) get
// stamped multiple times — TouchScanCoverage is an idempotent upsert, so this
// is safe. No cross-unit dedup state is added; the extra writes are acceptable.
// TouchScanCoverage calls happen outside mu alongside the agent_units row write,
// so the hot mutex never waits on the DB (sqlite serializes writers, but the
// mu-free write keeps sibling unit completions from serializing through mu).
//
// STREAMING TOPOLOGY: emit is called OUTSIDE mu for each candidate as the unit
// completes. This allows triage to start immediately on per-unit output rather
// than waiting for all units to finish. hypothesize blocks until all units
// finish (so the caller can close candCh after return) and returns the total
// candidate count (for stats) plus any fatal error.
func (f *Funnel) hypothesize(ctx context.Context, scanRunID string, finder llm.Client, persona string, kind store.ScanKind, cc *ChangeContext, langs []ingest.Language, targets []string, seams []ingest.Seam, budget *budgetState, result *Result, fps map[string]string, touchCoverage bool, cart *cartography, emit func(Candidate)) (int, error) {
	if len(targets) == 0 && cc == nil {
		return 0, nil
	}

	// Finders re-send their whole growing history every turn, so a fat read_file
	// result is paid for on every subsequent turn. Tightening the finder's
	// per-read caps shrinks each result at the source — slowing history growth
	// without ever mutating the conversation prefix, which (unlike history
	// compaction) preserves the prompt-cache prefix and so cuts cache-WEIGHTED
	// cost, not just raw tokens (see bugbot-3nf and DefaultFinderReadLines/Bytes).
	baseTools, err := f.readOnlyTools(f.opts.finderReadCaps())
	if err != nil {
		return 0, err
	}

	chunks := chunkByLanguage(targets, f.opts.Limits.ChunkSize)

	// Per-run lens priority: a lens's expected yield is language-dependent
	// (lensYields), so the launch order — and therefore which lenses survive
	// budget degradation — is recomputed from the repo's dominant languages
	// rather than taken from the Go-centric builtin order.
	lenses := lensesByYield(f.lenses, langs)

	// --- Pre-launch: collect and consume pending leads for each active lens ---
	//
	// This is single-threaded (before any goroutines launch) so no locking is
	// needed for the leads map. We claim all leads for active lenses upfront:
	// a lead stays "posted" for lenses not in this run and is consumed when
	// that lens next runs.
	leadsByLens := make(map[string][]store.Lead, len(lenses))
	var leadsConsumedTotal int
	for _, l := range lenses {
		// diff-intent is a change-scoped lens with no file/line lead semantics:
		// it receives its input from ChangeContext (commit message, diff, blast
		// targets), not from the lead blackboard. Including it here would silently
		// consume leads that should remain pending for the next taxonomy run.
		if l.Name == "diff-intent" {
			continue
		}
		pending, err := f.store.PendingLeads(ctx, l.Name)
		if err != nil {
			// Non-fatal: we log the error and continue without leads for this lens.
			f.note(result, fmt.Sprintf("leads: PendingLeads(%q) failed: %v", l.Name, err))
			continue
		}
		if len(pending) == 0 {
			continue
		}
		ids := make([]string, len(pending))
		for i, ld := range pending {
			ids[i] = ld.ID
		}
		// Consume at claim time — before the finder runs. If the finder fails,
		// the lead is lost for this run; the poster lens will re-post on its
		// next run if the suspicion is still relevant.
		if err := f.store.ConsumeLeads(ctx, ids); err != nil {
			f.note(result, fmt.Sprintf("leads: ConsumeLeads(%q) failed: %v", l.Name, err))
			continue
		}
		leadsByLens[l.Name] = pending
		leadsConsumedTotal += len(pending)
	}

	// Build the unit-of-work list: (lens, strategy, chunk) triples.
	strategies := builtinStrategies()
	units := buildUnits(lenses, strategies, chunks, leadsByLens)
	// Diff-intent: one extra unit at the front (highest yield => first to launch)
	// when the run is commit-scoped AND ChangeContext is populated. Only
	// ScanTargeted runs carry a ChangeContext; Sweep runs as ScanOneshot (run.go)
	// and must never fire diff-intent even if ChangeContext were somehow set.
	// Gating on kind == ScanTargeted && cc != nil captures both current
	// commit-triggered callers (daemon poll and cli --since) while leaving the
	// ScanOneshot sweep path permanently excluded.
	if kind == store.ScanTargeted && cc != nil {
		diLens := diffIntentLens()
		task := buildDiffIntentTask(cc, targets)
		units = append([]unit{{
			lens:       diLens,
			strategy:   sweepWide,
			files:      nil,
			customTask: task,
		}}, units...)
	}
	// cross-language-boundary: one custom unit per discovered seam, appended
	// to the chunk list. Seams are an EnumerateSeams(Snapshot) result; a nil
	// or empty slice produces zero units (the lens is naturally a no-op on
	// monoglots and on commits before any seam was discovered). The strategy
	// is sweepWide — the lens is custom-unit only and the strategy axis is
	// immaterial, but the launch loop expects a real strategy.
	if len(seams) > 0 {
		blLens := crossLanguageBoundaryLens()
		seamUnits := make([]unit, 0, len(seams))
		for _, s := range seams {
			seamUnits = append(seamUnits, unit{
				lens:       blLens,
				strategy:   sweepWide,
				files:      nil,
				customTask: buildSeamTask(s),
			})
		}
		units = append(units, seamUnits...)
	}

	// leadsPosted counts post_lead tool calls that succeeded across all parallel
	// finder goroutines. Atomic so parallel units can increment it safely.
	var leadsPostedAtomic atomic.Int32

	// Valid lens names for the post_lead tool, derived from all builtin lenses
	// (not just the active subset) so a finder can post to any lens including
	// inactive ones — the lead will be pending until that lens next runs.
	// diff-intent is excluded: it is a change-scoped lens with no file/line lead
	// semantics and does not consume from the lead blackboard, so a lead posted to
	// it would never be consumed (it would grow the blackboard unboundedly).
	allLensNames := make([]string, 0, len(BuiltinLenses()))
	for _, l := range BuiltinLenses() {
		if l.Name == "diff-intent" {
			continue
		}
		allLensNames = append(allLensNames, l.Name)
	}

	var (
		mu                  sync.Mutex
		coveredSet          = make(map[string]bool) // files from finderOK units only
		finderRuns          int
		finderFailed        int
		finderBudgetCut     int
		finderRateLimitedCt int
		totalCandidates     int // total candidates emitted (for stats)
		seamCovered         int // boundary-lens custom units that ran to a terminal state (incl. budget-stopped)
		firstErr            error
	)

	// --- finder-stage circuit breaker (bugbot-2uz) -------------------------
	//
	// Against an unreachable provider, each finder unit exhausts the retry
	// policy (DefaultRetryConfig: 4 attempts, ~4s of backoff) and the funnel
	// dispatches MANY finder seats for a large repo, so a fully-broken provider
	// takes (seats x ~4s) to surface MostFindersFailed instead of failing fast
	// (observed ~45s+ even with a single lens). The breaker detects
	// transport-level failures (llm.APIError with StatusCode==0 — the shape
	// produced by openai/google/anthropic adapters for timeouts, connection
	// resets, DNS failures) and aborts further launches once a threshold of
	// concurrent transport failures is observed with zero finderOK successes.
	//
	// Threshold = max(3, MaxParallel): at least 3 (a single transient blip
	// never trips it), and at least the configured concurrency (a parallel
	// batch of transport failures trips within one generation).
	//
	// anySucceeded disarms the breaker permanently for this stage: as soon as
	// ONE finder returns parseable output the provider is provably reachable
	// from this process, and any further transport failures are isolated to
	// individual flaky units rather than a systemic outage. Disarm is one-way
	// — the breaker never re-arms after a success.
	//
	// Rate-limit (ErrRateLimited) is NOT counted: a rate-limited provider is
	// still reachable and the failure is recoverable by lowering
	// --concurrency, so it must keep the existing classification (its own
	// counter) and the existing scan-exit semantics.
	//
	// The fanout owns the per-unit goroutine, the slotLow acquire/release, the
	// WaitGroup, and the breaker-cancellable runCtx (a child of ctx). All
	// runFinderWithPrompt calls and slot acquisitions go through runCtx; on a
	// breaker trip the tripping unit calls fo.stop to unblock any goroutine still
	// inside a retry loop or waiting on the slot pool. The caller's ctx is never
	// cancelled by us — only the derived child.
	fo := f.newFanout(ctx, slotLow)
	breakerThreshold := f.opts.Limits.MaxParallel
	if breakerThreshold < 3 {
		breakerThreshold = 3
	}
	var (
		transportFailures atomic.Int32 // transport-class parse failures while !anySucceeded
		anySucceeded      atomic.Bool  // true once any unit returns finderOK
		breakerTripped    atomic.Bool  // true after the breaker stopped this stage
	)

	// Compute degraded survivors from the (lens × strategy) unit-classes that
	// actually emitted at least one unit in this run. A unit-class is the pair
	// (lens.Name, strategy.Name); a lens that emitted zero units (diff-intent on
	// sweeps) must never occupy a survivor slot and starve a working lens.
	//
	// Unit-classes are ranked by effective yield = per-language lens yield ×
	// strategy.Weight, descending. The top degradedLensCount classes survive.
	// With only sweep-wide in play the result is identical to today's lens-only
	// degradation (weight 1.0 scales nothing). A deep unit-class with weight 0.9
	// ranks just below its own lens's wide class and is shed first under pressure.
	//
	// Collect active unit-classes preserving the lensesByYield order (for equal
	// effective-yield tiebreaking via stable sort inside degradedUnitClasses).
	seenClass := make(map[string]bool) // key = lensName+"@"+strategyName
	activeClasses := make([]lensStrategyClass, 0, len(units))
	// Iterate in lensesByYield order: lenses is already sorted by yield.
	// For each lens, emit its unit-classes in strategies order (sweep-wide first).
	for _, l := range lenses {
		for _, s := range strategies {
			key := l.Name + "@" + s.Name
			// Only include classes that actually emitted a unit.
			hasUnit := false
			for _, u := range units {
				if u.lens.Name == l.Name && u.strategy.Name == s.Name {
					hasUnit = true
					break
				}
			}
			if hasUnit && !seenClass[key] {
				seenClass[key] = true
				activeClasses = append(activeClasses, lensStrategyClass{
					lensName:     l.Name,
					strategyName: s.Name,
					weight:       s.Weight,
				})
			}
		}
	}
	degradedUnits := degradedUnitClasses(activeClasses, langs)

	for unitIdx, u := range units {
		mu.Lock()
		stop := firstErr != nil || breakerTripped.Load()
		mu.Unlock()
		if stop {
			// Units not launched because a prior unit errored OR because the
			// finder-stage circuit breaker tripped: we do not record rows for
			// these, as both early-break paths abort further work without
			// partial precision. The breakerTripped path keeps the
			// already-recorded FinderFailures intact (MostFindersFailed()
			// remains true) and Stats.FinderAborted is set at the end of
			// hypothesize so downstream consumers see the abort reason
			// explicitly.
			break
		}

		u := u
		unitIdx := unitIdx
		// The fanout holds a slotLow worker slot for the whole unit; the budget
		// gate and agent launch below run only once the slot is held. If the run
		// was stopped (breaker) or the caller's ctx was cancelled while this unit
		// waited, the slot is never granted and the unit never runs — same as
		// today, where cancelled runs abandon queued work without recording a row.
		// runCtx (the fanout's derived child) lets a breaker trip unblock this unit
		// even mid-run.
		fo.spawn(func(runCtx context.Context) {
			// Gate against the LIVE spend total only once we hold a worker slot, so
			// the decision reflects spend already recorded by earlier units rather
			// than a stale pre-launch snapshot. This is what makes degradation and
			// the hard stop actually bite as the run progresses.
			if budget.finderOverHard() {
				budget.stopped.Store(true)
				msg := fmt.Sprintf("hard budget reached: skipped finder lens %q on %d file(s)", u.lens.Name, len(u.files))
				f.note(result, msg)
				progress.Emit(f.opts.Progress, progress.Event{Kind: progress.KindBudgetStopped, Message: msg})
				// Record the skipped unit row (zero tokens, empty started_at). Best-effort.
				f.recordFinderUnit(ctx, scanRunID, u, unitIdx, "skipped_hard_budget", 0, 0, 0, 0, 0, result)
				return
			}
			if budget.finderOverSoft() {
				budget.degraded.Store(true)
				classKey := u.lens.Name + "@" + u.strategy.Name
				if !degradedUnits[classKey] {
					label := unitLabel(u.lens.Name, u.strategy.Name)
					msg := fmt.Sprintf("budget degraded: skipped low-yield finder lens %q on %d file(s)", label, len(u.files))
					f.note(result, msg)
					progress.Emit(f.opts.Progress, progress.Event{Kind: progress.KindBudgetDegraded, Message: msg})
					// Record the skipped unit row (zero tokens, empty started_at). Best-effort.
					f.recordFinderUnit(ctx, scanRunID, u, unitIdx, "skipped_degraded", 0, 0, 0, 0, 0, result)
					return
				}
			}

			// per-unit leads counter: folded into leadsPostedAtomic (so Stats.LeadsPosted
			// stays unchanged) and also written into the unit row. This avoids relying on
			// the global atomic for per-unit attribution (trap 4 in bugbot-mi5.10).
			// unitLeadsPosted is mutated only inside this unit's own goroutine (the
			// runner executes tool calls sequentially), so a plain int is safe.
			var unitLeadsPosted int

			// Build the tool set for this finder: read-only tools plus a per-unit
			// post_lead instance that carries this lens as the poster. The onPost
			// callback writes through f.store and increments the shared atomic
			// counter; f.store is concurrency-safe so multiple parallel units can
			// post leads simultaneously.
			postLeadTool := agent.NewPostLeadTool(u.lens.Name, allLensNames, func(targetLens, file string, line int, note string, confidence float64) error {
				if err := f.store.AddLead(ctx, store.Lead{
					ScanRunID:  scanRunID,
					PosterLens: u.lens.Name,
					TargetLens: targetLens,
					File:       file,
					Line:       line,
					Note:       note,
					Confidence: confidence,
				}); err != nil {
					return err
				}
				leadsPostedAtomic.Add(1)
				unitLeadsPosted++
				return nil
			})
			// Use a three-index slice expression to cap baseTools at its length
			// before appending, forcing a new backing array for every goroutine.
			// Without the cap expression each parallel finder goroutine calls
			// append on the same shared slice header: when baseTools has spare
			// capacity they all write into baseTools[len(baseTools)] concurrently,
			// a verified data race on the backing array. The three-index form
			// baseTools[:len(baseTools):len(baseTools)] sets cap==len, so every
			// append allocates a fresh array and goroutines never share backing
			// storage.
			// get_package_context and package_graph are FINDER-ONLY pull-based
			// cartography tools. See tools_package_context.go for the refuter-
			// independence rationale. cart may be nil ("feature off") — the
			// callbacks handle nil gracefully (clean miss / empty).
			pkgContextTool := agent.NewPackageContextTool(func(pkg string) (string, bool, error) {
				if cart == nil {
					return "", false, nil
				}
				// generate-on-miss via the lazy provider (singleflight-deduped,
				// persisted immediately). Falls back to a direct store read for
				// packages outside the spanned set (e.g. the agent asks about a
				// transitive dep we didn't fingerprint).
				if s, ok := cart.getSummary(ctx, pkg); ok {
					return s, true, nil
				}
				summaries, err := f.store.GetPackageSummaries(ctx, []string{pkg})
				if err != nil {
					return "", false, err
				}
				row, ok := summaries[pkg]
				if !ok {
					return "", false, nil
				}
				return row.Summary, true, nil
			})
			pkgGraphTool := agent.NewPackageGraphTool(func(pkg, direction string) ([]string, []string, error) {
				if cart == nil {
					return nil, nil, nil
				}
				imp, dep := cart.QueryGraph(pkg, direction)
				return imp, dep, nil
			})
			unitTools := append(baseTools[:len(baseTools):len(baseTools)], postLeadTool, pkgContextTool, pkgGraphTool)
			if t := f.maybeStatusNoteTool(progress.RoleFinder, u.lens.Name); t != nil {
				unitTools = append(unitTools, t)
			}
			if t := f.maybeReportToolIssueTool(result, progress.RoleFinder, u.lens.Name); t != nil {
				unitTools = append(unitTools, t)
			}

			// Resolve the task content. customTask takes highest priority
			// (diff-intent pre-built task). Then the strategy's BuildTask if
			// non-nil (deep strategies supply their own framing). Finally the
			// default finderTask file-list format.
			task := u.customTask
			if task == "" {
				if u.strategy.BuildTask != nil {
					task = u.strategy.BuildTask(u.files, u.leads)
				} else {
					task = finderTask(u.files, u.leads, cart.ensureContextFor(ctx, u.files))
				}
			}

			sysprompt := composeFinderSystemPrompt(persona, u.lens, u.langs, u.strategy)

			label := unitLabel(u.lens.Name, u.strategy.Name)
			startedAt := time.Now()
			// runCtx (not ctx) so a breaker trip unblocks the in-flight runner
			// without disturbing the caller's context.
			cands, status, outcome, pm, err := f.runFinderWithPrompt(runCtx, finder, unitTools, sysprompt, label, u.lens, task, budget, startedAt, f.toolHealthSinkFor(result, progress.RoleFinder, label))
			finishedAt := time.Now()

			// Emit KindAgentFinished here (not inside runFinderWithPrompt) so we
			// can carry the candidate count on the event — live status counters tick
			// as each finder unit completes rather than waiting for stage-finished.
			// Candidates is set only on finderOK; for error/parse-fail/budget-stop
			// paths the count is meaningless or unknown, so it stays zero.
			{
				var candidateCount int
				if err == nil && status == finderOK {
					candidateCount = len(cands)
				}
				emitFinderAgentFinished(f.opts.Progress, label, outcome, startedAt, err, candidateCount)
			}

			// Extract per-unit token counts directly from the Outcome's Usage
			// (not from the spend ledger — trap 1 in bugbot-mi5.10).
			var inTokens, outTokens, cacheRead int64
			if outcome != nil {
				inTokens = outcome.Usage.InputTokens
				outTokens = outcome.Usage.OutputTokens
				cacheRead = outcome.Usage.CacheReadInputTokens
			}

			// Fold stats under the lock; the agent_units row write happens AFTER
			// unlock so a sqlite insert never serializes sibling completions —
			// the skipped paths already record outside the lock, and this keeps
			// the discipline uniform. The runner-error path records no row: the
			// whole scan aborts on firstErr, so a partial unit table for an
			// aborted run would suggest precision it does not have.
			recordStatus := ""
			var candCount int64
			var unitDetail string // postmortem detail for parse_failed / budget_stopped rows
			mu.Lock()
			if err != nil {
				// If the breaker tripped, the ctx.Err() surfaced by
				// runFinderWithPrompt is a self-cancellation, not a caller
				// cancellation — swallow it so hypothesize returns nil and
				// Stats (with the already-recorded FinderFailures) report the
				// run as unreliable through MostFindersFailed() and
				// FinderAborted.
				if breakerTripped.Load() {
					mu.Unlock()
					return
				}
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			finderRuns++
			switch status {
			case finderParseFailed:
				finderFailed++
				// Build the classification label for the operator-visible warning.
				// pm is always non-nil on this path (runFinderWithPrompt sets it
				// before returning finderParseFailed).
				classLabel := ""
				if pm != nil {
					classLabel = " (" + string(pm.Class) + ")"
					unitDetail = finderPostmortemDetail(*pm)
				}
				msg := fmt.Sprintf("finder lens %q produced no parseable output on %d file(s)%s — its findings (if any) are LOST, not absent", label, len(u.files), classLabel)
				f.note(result, msg)
				progress.Emit(f.opts.Progress, progress.Event{
					Kind: progress.KindLensFailed, Role: progress.RoleFinder, Label: label, Message: msg,
				})
				recordStatus = "parse_failed"
				// Circuit-breaker accounting. Only count transport-class
				// failures while the breaker is still armed (no finderOK
				// yet). The first finderOK — regardless of which lens or
				// strategy it came from — disarms the breaker permanently,
				// because the provider is provably reachable from this
				// process and any further transport failures are isolated
				// to flaky units rather than a systemic outage. The check
				// sits under the same mu as finderRuns/finderFailed so the
				// transport counter, the disarming flag, and the
				// already-recorded stats stay mutually consistent — without
				// the lock a concurrent finderParseFailed could observe a
				// stale anySucceeded and double-count.
				if pm != nil && pm.Class == finderClassTransportError && !anySucceeded.Load() {
					if n := transportFailures.Add(1); n >= int32(breakerThreshold) && breakerTripped.CompareAndSwap(false, true) {
						// Threshold reached and the breaker was not
						// already tripped by a sibling unit. Cancel the
						// run context so any in-flight runFinderWithPrompt
						// / slot-pool waiter returns early; the outer
						// loop check (breakerTripped.Load() at the top of
						// each iteration) prevents further launches.
						// CompareAndSwap makes the fo.stop call
						// exactly-once across all sibling goroutines
						// regardless of how many reach the threshold
						// concurrently.
						fo.stop()
						f.note(result, fmt.Sprintf("finder circuit breaker tripped: %d transport failures with zero successes (threshold %d) — aborting further finder launches", n, breakerThreshold))
					}
				}
			case finderBudgetStopped:
				// A run truncated by the shared budget pool (or its own token
				// budget) whose partial output does not parse is a budget stop, NOT
				// a reliability problem: the lens did not "fail to parse", it was cut
				// short on purpose. Count it separately and note it under Skipped so a
				// budget-limited scan is never misreported as having broken finders.
				finderBudgetCut++
				if pm != nil {
					unitDetail = finderPostmortemDetail(*pm)
				}
				msg := fmt.Sprintf("finder lens %q stopped by budget on %d file(s) before emitting parseable output — partial coverage", label, len(u.files))
				f.note(result, msg)
				recordStatus = "budget_stopped"
			case finderRateLimited:
				// Provider rate-limit exhausted the retry budget. Coverage is
				// incomplete but recoverable (lower --concurrency / re-run) — NOT a
				// lost-finding failure, so we count it on its own axis instead of
				// finderFailed. Excluded from FinderReliable()/MostFindersFailed()
				// and the SCAN RELIABILITY WARNING by design (see funnel.go).
				finderRateLimitedCt++
				if pm != nil {
					unitDetail = finderPostmortemDetail(*pm)
				}
				msg := fmt.Sprintf("finder lens %q hit provider rate limit after retries on %d file(s) — coverage incomplete, re-run at lower --concurrency", label, len(u.files))
				f.note(result, msg)
				progress.Emit(f.opts.Progress, progress.Event{
					Kind: progress.KindLensFailed, Role: progress.RoleFinder, Label: label, Message: msg,
				})
				recordStatus = "rate_limited"
			default: // finderOK
				// Disarm the circuit breaker permanently: the provider
				// answered with parseable output, so it is reachable from
				// this process and any further transport failures are
				// isolated to flaky units rather than a systemic outage.
				// CompareAndSwap is a no-op once anySucceeded is true, so
				// every subsequent finderOK goroutine pays only a single
				// atomic op. Placed inside the existing mu so the disarm
				// is observed by a concurrent transport-class
				// finderParseFailed on the same lock acquisition order
				// (transport-failure goroutine takes mu first, observes
				// anySucceeded, and skips the breaker counter).
				anySucceeded.Store(true)
				recordStatus = "ok"
				candCount = int64(len(cands))
				totalCandidates += len(cands)
				// Record coverage under the existing mu so covered set stays
				// consistent with finderRuns/finderFailed collected in this same
				// lock. The diff-intent custom unit has files == nil and
				// contributes nothing; a file appearing in multiple units
				// (multiple lenses × chunks) is deduplicated by the map.
				for _, file := range u.files {
					coveredSet[file] = true
				}
			}
			// Boundary-lens custom units are "covered" when the unit
			// reaches any terminal status (ok / parse_failed / budget
			// / rate_limited). A seam that never reached the launch
			// loop is NOT covered — Stats.SeamsFound - SeamsCovered
			// is the unrun tail.
			if u.lens.Name == "cross-language-boundary" {
				seamCovered++
			}
			mu.Unlock()
			// STREAMING TOPOLOGY: emit each candidate OUTSIDE mu so the triage
			// consumer can start processing immediately without blocking sibling
			// unit completions through the hot mutex. emit may block if candCh is
			// full; that backpressure is acceptable and intentional (bounded buffer).
			// Only emit on finderOK — error/parse-fail/budget-stop paths have no
			// candidates to forward.
			if recordStatus == "ok" {
				// WAL: persist this unit's candidates to pending_candidates BEFORE
				// they enter the volatile channel pipeline, so an interrupt does
				// not lose the finder's (expensive) work. Batched per unit, same
				// per-unit durability discipline as the coverage stamp below. The
				// assigned row id is carried as PendingID so the terminal-fate
				// handlers can delete it; a clean run leaves the WAL empty.
				// Best-effort: a failed write degrades to pre-WAL volatility for
				// these candidates rather than aborting the scan.
				pcRows := make([]store.PendingCandidate, len(cands))
				for i, c := range cands {
					pcRows[i] = store.PendingCandidate{
						ScanRunID:           scanRunID,
						CommitSHA:           result.Commit,
						Lens:                c.Lens,
						File:                c.File,
						Line:                c.Line,
						Title:               c.Title,
						Description:         c.Description,
						Severity:            string(c.Severity),
						Evidence:            c.Evidence,
						Confidence:          string(c.Confidence),
						CorroboratingLenses: c.CorroboratingLenses,
					}
				}
				if perr := f.store.AddPendingCandidates(ctx, pcRows); perr != nil {
					f.note(result, fmt.Sprintf("pending: AddPendingCandidates failed (unit %d, %s): %v", unitIdx, u.lens.Name, perr))
				} else {
					for i := range cands {
						cands[i].PendingID = pcRows[i].ID
					}
				}
				for _, c := range cands {
					emit(c)
				}
			}
			f.recordFinderUnitWithTimeDetail(ctx, scanRunID, u, unitIdx, recordStatus, unitDetail, startedAt, finishedAt, inTokens, outTokens, cacheRead, candCount, unitLeadsPosted, result)

			// Per-unit coverage: stamp this unit's files immediately when finderOK
			// on a sweep run (touchCoverage=true). This replaces the old run-end
			// batch TouchScanCoverage call in Sweep, providing durable partial
			// progress: if the run is cancelled after this point, the files' coverage
			// is already persisted. The call is outside mu so the hot mutex never
			// waits on the DB write (same discipline as recordFinderUnitWithTime).
			// fps may be nil on fingerprint error; TouchScanCoverage degrades safely
			// (empty hash ≠ clobbering existing hash — see the CASE in filestate.go).
			// Best-effort on the run ctx: a unit whose coverage write loses the
			// race to cancellation (or a busy-timeout under high MaxParallel)
			// leaves an "ok" agent_units row with no coverage stamp. That fails
			// in the conservative direction — the file is simply re-scanned next
			// sweep — so no detached context here.
			if touchCoverage && recordStatus == "ok" && len(u.files) > 0 {
				if tcErr := f.store.TouchScanCoverage(ctx, u.files, result.Commit, fps); tcErr != nil {
					f.note(result, fmt.Sprintf("sweep: per-unit TouchScanCoverage failed (unit %d, %s): %v", unitIdx, u.lens.Name, tcErr))
				}
			}
		})
	}

	fo.wait()
	if firstErr != nil {
		return 0, firstErr
	}
	mu.Lock()
	result.Stats.FinderRuns = finderRuns
	result.Stats.FinderFailures = finderFailed
	result.Stats.FinderBudgetStopped = finderBudgetCut
	result.Stats.FinderRateLimited = finderRateLimitedCt
	result.Stats.FinderAborted = breakerTripped.Load()
	result.Stats.LeadsConsumed = leadsConsumedTotal
	result.Stats.SeamsCovered = seamCovered
	result.Stats.LeadsPosted = int(leadsPostedAtomic.Load())
	// Build the sorted covered-files slice from the set collected above.
	covered := make([]string, 0, len(coveredSet))
	for file := range coveredSet {
		covered = append(covered, file)
	}
	sort.Strings(covered)
	result.CoveredFiles = covered
	result.Stats.CoveredFiles = len(covered)
	n := totalCandidates
	mu.Unlock()
	return n, nil
}

// finderStatus classifies a finder run's parse outcome so the funnel can tell a
// genuine reliability failure apart from a deliberate budget stop.
type finderStatus int

const (
	// finderOK means the finder produced parseable candidates.
	finderOK finderStatus = iota
	// finderParseFailed means the finder ran to a non-budget stop but produced no
	// parseable JSON even after the repair round-trip — its findings are LOST.
	finderParseFailed
	// finderBudgetStopped means the finder was truncated by the shared budget
	// pool or its own token budget; an unparseable partial result here is an
	// expected budget stop, not a reliability failure.
	finderBudgetStopped
	// finderRateLimited means the finder exhausted retries against a
	// rate-limiting provider (llm.ErrRateLimited). Coverage is incomplete but
	// recoverable by lowering --concurrency or re-running — NOT lost like a
	// genuine parse failure, so this status is excluded from FinderFailures
	// and from the reliability gate.
	finderRateLimited
)

// runFinder executes a single finder agent for one lens over one task and maps
// its JSON output to Candidates tagged with the lens. The agent's limits are
// derived from the shared budget pool at launch (remaining-pool allowance plus a
// pre-turn budget check), so a finder launched late gets only the headroom left
// and one already in flight stops at its next turn once the pool is exhausted.
//
// task is the pre-built user message for the agent. Standard chunk-based units
// pass finderTask(files, leads); the diff-intent unit passes buildDiffIntentTask.
//
// The finderStatus return distinguishes a parse failure (the finder ran but
// produced no parseable JSON even after the repair round-trip, so its result is
// LOST, not a clean "found nothing") from a budget stop (the run was truncated
// by the budget pool / token budget, so an unparseable partial is expected). The
// funnel surfaces parse failures so a scan never silently reports "No findings"
// when a lens actually failed, while budget stops are accounted separately.
//
// This is a thin wrapper around runFinderWithPrompt that builds the system prompt
// from persona+lens+langs and uses the lens name as the progress label. Test code
// calls this directly; production code calls runFinderWithPrompt after composing
// the strategy-aware system prompt.
func (f *Funnel) runFinder(ctx context.Context, finder llm.Client, tools []agent.Tool, persona string, l Lens, langs []ingest.Language, task string, budget *budgetState) ([]Candidate, finderStatus, *finderPostmortem, error) {
	sysprompt := finderSystemPrompt(persona, l, langs)
	start := time.Now()
	cands, status, outcome, pm, err := f.runFinderWithPrompt(ctx, finder, tools, sysprompt, l.Name, l, task, budget, start)
	emitAgentFinished(f.opts.Progress, progress.RoleFinder, l.Name, outcome, start, err)
	return cands, status, pm, err
}

// runFinderWithPrompt is the core finder executor. It accepts a pre-composed
// system prompt and a progress label so callers (hypothesize) can inject
// strategy clauses and use strategy-qualified labels (lens@strategy) without
// rebuilding the prompt inside this function.
//
// startedAt is the wall-clock time the caller captured before invoking this
// function; the caller is responsible for emitting KindAgentFinished (with the
// Candidates count it derives from the returned candidates slice) after this
// function returns. runFinderWithPrompt only emits KindAgentStarted.
//
// The returned *agent.Outcome carries the agent's Usage (InputTokens /
// OutputTokens / CacheReadInputTokens) that the caller uses to populate the
// per-unit observability row. The Outcome is non-nil as long as the agent ran
// at least one turn; callers must handle nil (budget-pool pre-turn stop).
//
// On any no-parse failure path (finderParseFailed or finderBudgetStopped) the
// returned *finderPostmortem is non-nil and carries the classification,
// underlying error string, raw model output head, and token counts needed to
// diagnose the failure post-run. On finderOK the postmortem is nil. The
// postmortem is built here — where err is still live — so err is never
// silently dropped: it flows into pm.ErrString and pm.Class.
//
// Threading seam: runFinderWithPrompt has no store access and does not record
// the agent_units row; it returns the postmortem so the goroutine caller
// (hypothesize) can fold it into the Detail field of the
// recordFinderUnitWithTimeDetail call. This keeps the recording at a single
// site and avoids threading a store reference into this function.
func (f *Funnel) runFinderWithPrompt(ctx context.Context, finder llm.Client, tools []agent.Tool, sysprompt, label string, l Lens, task string, budget *budgetState, startedAt time.Time, extraOpts ...agent.Option) ([]Candidate, finderStatus, *agent.Outcome, *finderPostmortem, error) {
	progress.NewAgentScope(f.opts.Progress, progress.RoleFinder, label).Start()

	// attempt runs one finder pass: it builds the runner (layering any
	// per-attempt options on top of the standard finder set), runs RunJSON, and
	// maps the result into candidates or a classified failure + postmortem. It
	// is invoked once normally and, on a max-tokens-truncation parse failure,
	// once more at a doubled output cap (bugbot-rwe).
	attempt := func(extra ...agent.Option) ([]Candidate, finderStatus, *agent.Outcome, *finderPostmortem, error) {
		opts := append([]agent.Option{f.activitySinkFor(progress.RoleFinder, label)}, extraOpts...)
		opts = append(opts, extra...)
		runner := f.newAgentRunner(finder, tools, sysprompt, budget.finderRunnerLimits(f.opts.Limits.FinderLimits), opts...)

		var out candidateList
		outcome, err := runner.RunJSON(ctx, task, candidatesSchema, &out)
		if err != nil {
			// A finder that fails to produce parseable JSON yields no candidates
			// rather than aborting the whole scan: one lens/chunk failing must not
			// sink the others. Context cancellation is the exception — propagate it.
			if ctx.Err() != nil {
				return nil, finderOK, outcome, nil, ctx.Err()
			}
			// Distinguish a genuine parse failure from a budget stop. If the run was
			// truncated by the shared budget pool or its own token budget, an
			// unparseable partial is the expected consequence of stopping early, not a
			// reliability problem — classify it as a budget stop so it does not inflate
			// the finder-failure count. Otherwise its findings are LOST: report a parse
			// failure so a scan never silently prints "No findings" when a lens broke.
			//
			// In both cases, build a postmortem capturing the classification, the
			// underlying err (which carries the classified provider error — e.g. 429 +
			// Retry-After from llm.APIError — or the parse error message), and the raw
			// model output head. err is intentionally NOT discarded here; it flows into
			// the postmortem so the next real failure is diagnosable from stored data.
			pm := buildFinderPostmortem(outcome, err)
			if budgetStopped(outcome) {
				return nil, finderBudgetStopped, outcome, &pm, nil
			}
			// Rate-limit exhaustion is not a lost-finding failure: the provider
			// throttled us after the retry budget was spent. Coverage is incomplete
			// but recoverable (lower --concurrency / re-run) and the retry client
			// already honored Retry-After. Classify distinctly so it never
			// inflates FinderFailures or trips the SCAN RELIABILITY WARNING; the
			// postmortem already carries Class=finderClassRateLimited via
			// classifyFinderErr.
			if errors.Is(err, llm.ErrRateLimited) {
				return nil, finderRateLimited, outcome, &pm, nil
			}
			return nil, finderParseFailed, outcome, &pm, nil
		}

		cands := make([]Candidate, 0, len(out.Candidates))
		for _, rc := range out.Candidates {
			cands = append(cands, Candidate{
				Lens:        l.Name,
				File:        rc.File,
				Line:        rc.Line,
				Title:       rc.Title,
				Description: rc.Description,
				Severity:    normalizeSeverity(domain.Severity(rc.Severity)),
				Evidence:    rc.Evidence,
				Confidence:  normalizeConfidence(domain.Confidence(rc.Confidence)),
			})
		}
		return cands, finderOK, outcome, nil, nil
	}

	cands, status, outcome, pm, err := attempt()
	// bugbot-rwe: a finder unit lost to per-completion max-tokens truncation — a
	// reasoning model (e.g. MiniMax-M3) that spent the DefaultMaxOutputTokens cap
	// inside <think> blocks before emitting JSON — is recoverable. Retry ONCE at a
	// doubled per-completion cap so the model has room for think + JSON. The
	// doubled WithMaxTokens is layered last and so overrides newAgentRunner's
	// default. shouldRetryFinderCap gates this tightly (see its doc).
	if shouldRetryFinderCap(status, outcome, err) {
		cands, status, outcome, pm, err = attempt(agent.WithMaxTokens(finderRetryMaxOutputTokens))
	}
	return cands, status, outcome, pm, err
}

// finderRetryMaxOutputTokens is the doubled per-completion output cap used on the
// single retry of a finder unit lost to max-tokens truncation (bugbot-rwe). A
// reasoning model that spent the DefaultMaxOutputTokens cap inside <think> blocks
// before emitting JSON gets room for think + JSON on the retry.
const finderRetryMaxOutputTokens = 2 * DefaultMaxOutputTokens

// shouldRetryFinderCap reports whether a finder pass that produced no candidates
// should be retried once at the doubled per-completion output cap. It fires only
// for the bugbot-rwe failure mode: a parse failure whose proximate cause was the
// per-completion max-tokens cap — Outcome.LastStopReason == llm.StopMaxTokens, the
// canonical cap-truncation signal also used by truncationNote. It rejects budget
// stops (no headroom to retry), rate limits (recover by lowering concurrency),
// non-truncated malformed JSON (a bigger cap would not help), and a nil outcome.
func shouldRetryFinderCap(status finderStatus, outcome *agent.Outcome, err error) bool {
	if status != finderParseFailed {
		return false
	}
	if outcome == nil || outcome.LastStopReason != llm.StopMaxTokens {
		return false
	}
	if budgetStopped(outcome) {
		return false
	}
	return !errors.Is(err, llm.ErrRateLimited)
}

// finderFailureClass is a coarse classification of why a finder failed to
// produce parseable output. It is recorded in the agent_units Detail column so
// an operator can distinguish transient provider problems from model output
// issues without reading raw logs.
type finderFailureClass string

const (
	// finderClassRateLimited means the provider returned a 429 / rate-limit
	// response (llm.ErrRateLimited) after all retry attempts were exhausted.
	finderClassRateLimited finderFailureClass = "rate-limited"
	// finderClassEmptyOutput means the model returned an empty text body — no
	// think blocks, no JSON, nothing parseable.
	finderClassEmptyOutput finderFailureClass = "empty-output"
	// finderClassUnparseable means the model returned text but it was not valid
	// JSON even after the repair round-trip.
	finderClassUnparseable finderFailureClass = "unparseable"
	// finderClassBudgetStop means the run was cut short by the shared token
	// budget pool or the run's own token budget before producing parseable JSON.
	finderClassBudgetStop finderFailureClass = "budget-stop"
	// finderClassTransportError means the provider was unreachable: a
	// transport / connection failure surfaced as an *llm.APIError with
	// StatusCode==0 (the shape produced by the openai / google / anthropic
	// adapters for non-HTTP errors — timeout, connection reset, DNS failure).
	// Distinct from rate-limit (the provider is reachable but throttling) and
	// from parse failures (the provider answered; the model output is the
	// problem). This class drives the finder-stage circuit breaker
	// (bugbot-2uz): N concurrent transport errors with zero successes is a
	// strong signal the provider is down, and the funnel aborts further
	// launches instead of waiting for every retry to time out.
	finderClassTransportError finderFailureClass = "transport-error"
)

// finderPostmortem is captured on every no-parse finder failure (both
// finderParseFailed and finderBudgetStopped paths). It preserves enough
// evidence to diagnose the root cause post-run without re-running the scan.
//
// Storage: encoded into the Detail column of the existing agent_units row so
// the artifact is retrievable via the existing "report units" CLI path. The
// grain is already one row per unit execution — extending Detail avoids a new
// table, mirrors the existing verifier detail pattern, and keeps the schema
// unchanged. Recording failures are best-effort and never abort the scan.
type finderPostmortem struct {
	// Class is the coarse failure classification.
	Class finderFailureClass
	// ErrString is err.Error() from the runner, nil-safe (empty string when err
	// was nil, e.g. a pure budget stop with no underlying error).
	ErrString string
	// RawHead is the first finderPostmortemRawCap bytes of the model's output
	// (outcome.FinalText). Capped to prevent unbounded storage.
	RawHead string
	// RawLen is the full byte-length of outcome.FinalText before capping.
	RawLen int
	// Empty is true when outcome.FinalText was empty.
	Empty bool
	// HadThink is true when outcome.FinalText contained a <think> span (a
	// reasoning-model thought block). Detected by substring search; no parse.
	HadThink bool
	// TruncationReason is outcome.TruncationReason, nil-safe.
	TruncationReason string
	// InputTokens / OutputTokens / CacheReadTokens from outcome.Usage.
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
}

// finderPostmortemRawCap is the maximum bytes of raw model output preserved in
// the postmortem. 4 KB captures enough context for diagnosis without bloating
// the Detail column.
const finderPostmortemRawCap = 4 * 1024

// classifyFinderErr maps the underlying runner error to a finderFailureClass.
// It uses errors.Is against llm.ErrRateLimited (the sentinel produced by
// llm.APIError.Unwrap when the provider returned a 429). The outcome and err
// are both nil-safe.
func classifyFinderErr(outcome *agent.Outcome, err error) finderFailureClass {
	if budgetStopped(outcome) {
		return finderClassBudgetStop
	}
	if err != nil && errors.Is(err, llm.ErrRateLimited) {
		return finderClassRateLimited
	}
	if isTransportError(err) {
		// Provider unreachable (timeout / connection reset / DNS failure).
		// Distinct from rate-limit (provider reachable, throttling) and from
		// parse failures (the runner did not return an error at all). This
		// branch also handles the bare APIError{Kind: ErrServer, StatusCode: 0}
		// shape that the adapters return for non-HTTP transport failures.
		return finderClassTransportError
	}
	finalText := ""
	if outcome != nil {
		finalText = outcome.FinalText
	}
	if finalText == "" {
		return finderClassEmptyOutput
	}
	return finderClassUnparseable
}

// isTransportError reports whether err represents a transport / connection
// failure: an *llm.APIError with StatusCode==0 (the shape produced by the
// openai / google / anthropic adapters for non-HTTP errors — timeouts,
// connection resets, DNS failures). Both the bare
// "APIError{Kind: ErrServer, StatusCode: 0}" shape and any other
// APIError{StatusCode: 0} variant match (Kind may be ErrServer, ErrOverloaded,
// or any unrecognized transport-level failure surfaced through the adapter).
// Distinct from rate-limit (errors.Is(err, llm.ErrRateLimited) — provider
// reachable, throttling) and from parse failures (no error from the runner at
// all). nil-safe.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 0
	}
	return false
}

// buildFinderPostmortem constructs a finderPostmortem from a failed run. Both
// outcome and err may be nil (budget-pool pre-turn stop with no error).
func buildFinderPostmortem(outcome *agent.Outcome, err error) finderPostmortem {
	pm := finderPostmortem{
		Class: classifyFinderErr(outcome, err),
	}
	if err != nil {
		pm.ErrString = err.Error()
	}
	if outcome != nil {
		raw := outcome.FinalText
		pm.RawLen = len(raw)
		pm.Empty = raw == ""
		pm.HadThink = strings.Contains(raw, "<think>")
		if len(raw) > finderPostmortemRawCap {
			raw = raw[:finderPostmortemRawCap]
		}
		pm.RawHead = raw
		pm.TruncationReason = outcome.TruncationReason
		pm.InputTokens = outcome.Usage.InputTokens
		pm.OutputTokens = outcome.Usage.OutputTokens
		pm.CacheReadTokens = outcome.Usage.CacheReadInputTokens
	}
	return pm
}

// finderPostmortemDetail encodes a finderPostmortem as a compact string for
// the agent_units Detail column. The format mirrors the verifier detail style:
// structured key=value pairs, injection-safe (no model-authored free text in
// the structured portion; raw output is capped and included after a separator
// for direct inspection).
func finderPostmortemDetail(pm finderPostmortem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "class=%s empty=%v had_think=%v raw_len=%d trunc=%q in=%d out=%d cache=%d err=%q",
		pm.Class, pm.Empty, pm.HadThink, pm.RawLen,
		pm.TruncationReason, pm.InputTokens, pm.OutputTokens, pm.CacheReadTokens,
		pm.ErrString,
	)
	if pm.RawHead != "" {
		fmt.Fprintf(&b, " raw_head=%q", pm.RawHead)
	}
	return b.String()
}

// lensStrategyClass identifies a (lens × strategy) unit-class for degradation
// ranking. It carries the weight so the ranking can be computed without
// re-fetching the strategy.
type lensStrategyClass struct {
	lensName     string
	strategyName string
	weight       float64
}

// degradedUnitClasses returns the set of unit-class keys (lens@strategy) that
// survive budget degradation. It ranks each (lens, strategy) class by effective
// yield = per-language lens yield × strategy.Weight, descending, and keeps the
// head degradedLensCount classes.
//
// The sort is stable and compares ONLY the score: equal-score classes keep
// their input order, which callers supply in lensesByYield order (wide before
// deep within a lens). That makes the equal-yield tiebreak identical to the
// pre-strategy degradedLensNames semantics — head-of-lensesByYield — rather
// than introducing a new (e.g. alphabetical) tiebreak that would silently
// change survivors the next time the yield tables are retuned. A deep
// unit-class (weight 0.9) ranks just below its lens's sweep-wide class and is
// therefore shed first under pressure — intended behavior.
//
// CRITICAL INVARIANT: with only sweep-wide in play (weight 1.0), the survivors
// must be exactly the top degradedLensCount lenses by yield — identical to the
// pre-strategy degradedLensNames behavior.
//
// Callers must pass only classes that actually emitted units this run (see
// hypothesize): a zero-unit lens must never occupy a survivor slot.
func degradedUnitClasses(classes []lensStrategyClass, langs []ingest.Language) map[string]bool {
	type ranked struct {
		key   string
		score float64
	}
	r := make([]ranked, len(classes))
	for i, c := range classes {
		r[i] = ranked{
			key:   c.lensName + "@" + c.strategyName,
			score: float64(effectiveYield(c.lensName, langs)) * c.weight,
		}
	}
	sort.SliceStable(r, func(i, j int) bool {
		return r[i].score > r[j].score
	})
	keep := make(map[string]bool, degradedLensCount)
	for i, rc := range r {
		if i >= degradedLensCount {
			break
		}
		keep[rc.key] = true
	}
	return keep
}

// unitLabel returns the progress label for a finder unit. Default strategy
// (sweep-wide) units use the bare lens name to preserve existing output.
// Non-default strategy units use "lens@strategy" so they are distinguishable.
func unitLabel(lensName, strategyName string) string {
	if strategyName == sweepWide.Name {
		return lensName
	}
	return lensName + "@" + strategyName
}

// fileChunk is one finder unit's worth of target files plus the language set
// those files span (deduplicated, sorted). The language set selects the
// per-language manifestation blocks in the finder prompt; mixed chunks get the
// union of their languages' blocks.
type fileChunk struct {
	files []string
	langs []ingest.Language
}

// chunkByLanguage groups files by detected language BEFORE chunking, so chunks
// are language-homogeneous where possible and most finder prompts carry
// exactly one manifestation block. Each language's files (kept in input order,
// which Sweep may have heat-ordered) are cut into full chunks of exactly size;
// the per-language tails are then concatenated — still grouped by language, in
// first-seen order — and chunked together, so the only mixed chunks are the
// unavoidable remainders. Chunk-size semantics match chunk(): at most size
// files each, and a non-positive size yields a single chunk of everything.
func chunkByLanguage(files []string, size int) []fileChunk {
	if len(files) == 0 {
		return nil
	}
	if size <= 0 || len(files) <= size {
		return []fileChunk{{files: files, langs: chunkLangs(files)}}
	}

	var order []ingest.Language
	groups := make(map[ingest.Language][]string)
	for _, f := range files {
		l := ingest.DetectLanguage(f)
		if _, ok := groups[l]; !ok {
			order = append(order, l)
		}
		groups[l] = append(groups[l], f)
	}

	var out []fileChunk
	var tails []string
	for _, l := range order {
		g := groups[l]
		for len(g) >= size {
			// Three-index slice so a later append elsewhere can never write into
			// this chunk's backing array.
			out = append(out, fileChunk{files: g[:size:size], langs: []ingest.Language{l}})
			g = g[size:]
		}
		tails = append(tails, g...)
	}
	// The tails are language-contiguous, so chunking them keeps remainders
	// homogeneous whenever they happen to align with a chunk boundary; only the
	// genuinely unavoidable stragglers mix.
	for _, c := range chunk(tails, size) {
		out = append(out, fileChunk{files: c, langs: chunkLangs(c)})
	}

	// Restore global heat priority at chunk granularity: Sweep heat-orders the
	// input (churn x recency, bugbot-sro), and grouping by language must not
	// defer the second-hottest file of another language behind a whole cold
	// group. Sort chunks by the input rank of their hottest (earliest) member;
	// homogeneity is preserved because membership is untouched.
	rank := make(map[string]int, len(files))
	for i, f := range files {
		rank[f] = i
	}
	hottest := func(c fileChunk) int {
		best := len(files)
		for _, f := range c.files {
			if r := rank[f]; r < best {
				best = r
			}
		}
		return best
	}
	sort.SliceStable(out, func(i, j int) bool { return hottest(out[i]) < hottest(out[j]) })
	return out
}

// chunkLangs returns the deduplicated language set of files, sorted for a
// deterministic prompt (the manifestation blocks render in this order).
func chunkLangs(files []string) []ingest.Language {
	seen := make(map[ingest.Language]bool)
	var out []ingest.Language
	for _, f := range files {
		l := ingest.DetectLanguage(f)
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// chunk splits files into slices of at most size elements. The final chunk may
// be shorter. A non-positive size yields a single chunk of everything.
func chunk(files []string, size int) [][]string {
	if size <= 0 || len(files) <= size {
		if len(files) == 0 {
			return nil
		}
		return [][]string{files}
	}
	var out [][]string
	for i := 0; i < len(files); i += size {
		end := i + size
		if end > len(files) {
			end = len(files)
		}
		out = append(out, files[i:end])
	}
	return out
}

// diffIntentLens returns the Lens descriptor for the diff-intent lens.
// It is defined as a package-level var (builtinDiffIntentLens in lens.go)
// so this lookup is zero-cost and cannot panic.
func diffIntentLens() Lens {
	return builtinDiffIntentLens
}

// buildDiffIntentTask constructs the finder task message for the diff-intent
// lens. It embeds the commit message (capped at diffIntentMsgCap), the unified
// diff (capped at diffIntentDiffCap bytes with an explicit truncation marker),
// the files changed in the commit, and the blast-radius dependents (targets
// beyond the changed set) so the agent can check call sites without extra tool
// calls. The task is self-contained: the agent still has read-only tools and
// can follow up with find_references if needed.
//
// targets is the full blast-radius file list as seen by hypothesize (already
// expanded and snapshot-intersected). The blast-radius dependent section is
// built by subtracting cc.ChangedFiles from targets, so the prompt correctly
// identifies "files that MAY DEPEND ON the changed code" rather than the changed
// files themselves.
func buildDiffIntentTask(cc *ChangeContext, targets []string) string {
	var b strings.Builder
	b.WriteString("Audit this commit for intent-vs-implementation mismatches and broken caller assumptions.\n\n")

	b.WriteString("COMMIT MESSAGE:\n")
	if cc.Message != "" {
		msg := cc.Message
		if len(msg) > diffIntentMsgCap {
			msg = msg[:diffIntentMsgCap] + "\n[message truncated at 4KB]"
		}
		b.WriteString(msg)
	} else {
		b.WriteString("(not available)")
	}
	b.WriteString("\n\n")

	b.WriteString("UNIFIED DIFF:\n")
	if len(cc.Diff) == 0 {
		b.WriteString("(not available)\n")
	} else if len(cc.Diff) > diffIntentDiffCap {
		b.Write(cc.Diff[:diffIntentDiffCap])
		b.WriteString("\n[diff truncated at 48KB]\n")
	} else {
		b.Write(cc.Diff)
		b.WriteByte('\n')
	}

	// Files changed directly in this commit.
	if len(cc.ChangedFiles) > 0 {
		b.WriteString("\nFILES CHANGED IN THIS COMMIT:\n")
		for _, f := range cc.ChangedFiles {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}

	// Blast-radius dependents: targets that are NOT in the changed set. These are
	// files in scope that may depend on the changed code and whose caller
	// assumptions the change might break.
	changedSet := make(map[string]bool, len(cc.ChangedFiles))
	for _, f := range cc.ChangedFiles {
		changedSet[f] = true
	}
	var dependents []string
	for _, t := range targets {
		if !changedSet[t] {
			dependents = append(dependents, t)
		}
	}
	if len(dependents) > 0 {
		b.WriteString("\nBLAST-RADIUS DEPENDENTS (files in scope that may depend on the changed code):\n")
		for _, f := range dependents {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}

	b.WriteString("\nFor each finding: read the relevant call sites with find_references before reporting.\n")
	b.WriteString("Finding nothing is the expected outcome for most commits.\n")
	return b.String()
}

// crossLanguageBoundaryLens returns the Lens descriptor for the
// cross-language-boundary lens. It is defined as a package-level var
// (builtinCrossLanguageBoundaryLens in lens.go) so this lookup is
// zero-cost and cannot panic.
func crossLanguageBoundaryLens() Lens {
	return builtinCrossLanguageBoundaryLens
}

// buildSeamTask constructs the finder task message for the cross-language-
// boundary lens. It names the seam kind/key and every side file with its
// language and line, so the agent can read both sides end-to-end and report
// contract mismatches. The task is self-contained: the agent has read-only
// tools and can follow up with find_references on either side.
//
// The seam is a contract surface, not a commit, so there is no diff, no
// message, and no leads: the input is the two-sides contract. "Finding
// nothing" is the expected outcome on the vast majority of seams; only
// genuine cross-language drift surfaces a candidate.
func buildSeamTask(s ingest.Seam) string {
	var b strings.Builder
	switch s.Kind {
	case ingest.SeamDataFile:
		fmt.Fprintf(&b, "Audit this shared data file for cross-language contract mismatches.\n\n")
		fmt.Fprintf(&b, "SHARED DATA FILE: %s\n\n", s.Key)
	case ingest.SeamEnvVar:
		fmt.Fprintf(&b, "Audit this shared environment variable for cross-language contract mismatches.\n\n")
		fmt.Fprintf(&b, "SHARED ENVIRONMENT VARIABLE: %s\n\n", s.Key)
	default:
		fmt.Fprintf(&b, "Audit this cross-language seam.\n\nKIND: %s\nKEY: %s\n\n", s.Kind, s.Key)
	}
	b.WriteString("SIDES (every participating file; both sides must be read end-to-end):\n")
	for _, side := range s.Sides {
		if side.Line > 0 {
			fmt.Fprintf(&b, "  - %s [%s] (first reference at line %d)\n", side.File, side.Language, side.Line)
		} else {
			fmt.Fprintf(&b, "  - %s [%s]\n", side.File, side.Language)
		}
	}
	b.WriteString("\nFor each finding: confirm the mismatch by reading BOTH sides end-to-end " +
		"(use read_file on each named file) before reporting. A mismatch you have " +
		"not verified on both sides is not a finding.\n")
	b.WriteString("Finding nothing is the expected outcome for the vast majority of seams: " +
		"report an empty list when the two sides agree on the contract.\n")
	return b.String()
}

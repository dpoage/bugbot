package funnel

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

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
			// One AgentScope per unit run, minted BEFORE any tool is built so
			// maybeStatusNoteTool, runFinderWithPrompt's activity sink, and this
			// unit's Finished event all carry the SAME AgentID — see
			// progress.AgentEventKey / agent_runners.go's activitySinkFor doc
			// (bugbot-r7ub). label is resolved here (not further down) so the
			// scope, the status_note tool, and the eventual Started/Finished
			// events never disagree — a pre-existing latent mismatch when
			// strategy.Name was non-default (the status_note tool used to key
			// off u.lens.Name alone while Started/Finished used the
			// strategy-qualified label).
			label := unitLabel(u.lens.Name, u.strategy.Name)
			scope := progress.NewAgentScope(f.opts.Progress, progress.RoleFinder, label).Start()

			unitTools := append(baseTools[:len(baseTools):len(baseTools)], postLeadTool, pkgContextTool, pkgGraphTool)
			if t := f.maybeStatusNoteTool(scope); t != nil {
				unitTools = append(unitTools, t)
			}
			if t := f.maybeReportToolIssueTool(result, progress.RoleFinder, label); t != nil {
				unitTools = append(unitTools, t)
			}

			// Resolve the task content. customTask takes highest priority
			// (diff-intent pre-built task). Then the strategy's BuildTask if
			// non-nil (deep strategies supply their own framing). Finally the
			// default finderTask file-list format.
			task := u.customTask
			if task == "" {
				if u.strategy.BuildTask != nil {
					// Deep strategy unit: precompute the cross-reference closure
					// of the seed files' load-bearing symbols so the agent
					// confirms a specific set instead of rediscovering it.
					// deepRefClosure is NIL-SAFE: nil/errored nav returns
					// (nil, nil), producing byte-identical output to today.
					refs, relFiles := f.deepRefClosure(ctx, u.files)
					taskFiles := dedupFiles(u.files, relFiles)
					var b strings.Builder
					b.WriteString(u.strategy.BuildTask(taskFiles, u.leads))
					appendRefsSection(&b, refs)
					task = b.String()
				} else {
					task = finderTask(u.files, u.leads, cart.ensureContextFor(ctx, u.files))
				}
			}

			sysprompt := composeFinderSystemPrompt(persona, u.lens, u.langs, u.strategy)

			startedAt := time.Now()
			// runCtx (not ctx) so a breaker trip unblocks the in-flight runner
			// without disturbing the caller's context.
			cands, status, outcome, pm, traversal, err := f.runFinderWithPrompt(runCtx, finder, unitTools, sysprompt, label, u.lens, task, budget, startedAt, scope, f.toolHealthSinkFor(result, progress.RoleFinder, label))
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
				emitFinderAgentFinished(scope, outcome, startedAt, err, candidateCount)
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
			// Persist finder traversal audit row (best-effort, never aborts scan).
			// Only written for finderOK units that reported a non-nil traversal
			// field in their output JSON. Units that omit the optional traversal
			// field contribute zero rows, which is the expected default (the
			// traversal field is opt-in to avoid noisy empty rows).
			if recordStatus == "ok" && traversal != nil {
				ft := store.FinderTraversal{
					ScanRunID:      scanRunID,
					Lens:           u.lens.Name,
					Strategy:       u.strategy.Name,
					Files:          u.files,
					Enumerated:     traversal.Enumerated,
					Visited:        traversal.Visited,
					CandidateCount: len(cands),
				}
				if ftErr := f.store.AddFinderTraversal(ctx, ft); ftErr != nil {
					f.note(result, fmt.Sprintf("observability: AddFinderTraversal failed (unit %d, %s): %v", unitIdx, u.lens.Name, ftErr))
				}
			}

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

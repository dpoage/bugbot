package funnel

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// hypothesize runs the finder stage: for each effective lens, run a finder
// agent over each chunk of target files, collecting concrete candidates. Lens
// chunks run in parallel bounded by Options.MaxParallel. Budget degradation is
// applied as the run progresses: once over the soft threshold only the
// highest-yield lenses keep launching, and once over the hard threshold no new
// finder agents are launched.
//
// Cross-lens leads: before launching any finder units, we collect pending leads
// for each active lens, mark them consumed immediately (at claim time), and
// inject them into every finder task for that lens. Consuming at claim time
// means a failed finder run loses the lead — accepted trade-off documented
// here and in the store package. Leads targeting lenses not active this run
// stay pending and are consumed when that lens next runs.
func (f *Funnel) hypothesize(ctx context.Context, scanRunID string, finder llm.Client, persona string, targets []string, budget *budgetState, result *Result) ([]Candidate, error) {
	if len(targets) == 0 {
		return nil, nil
	}

	// Finders re-send their whole growing history every turn, so a fat read_file
	// result is paid for on every subsequent turn. Tightening the finder's
	// per-read caps shrinks each result at the source — slowing history growth
	// without ever mutating the conversation prefix, which (unlike history
	// compaction) preserves the prompt-cache prefix and so cuts cache-WEIGHTED
	// cost, not just raw tokens (see bugbot-3nf and DefaultFinderReadCaps).
	baseTools, err := f.readOnlyTools(f.opts.finderReadCaps())
	if err != nil {
		return nil, err
	}

	chunks := chunk(targets, f.opts.ChunkSize)

	// --- Pre-launch: collect and consume pending leads for each active lens ---
	//
	// This is single-threaded (before any goroutines launch) so no locking is
	// needed for the leads map. We claim all leads for active lenses upfront:
	// a lead stays "posted" for lenses not in this run and is consumed when
	// that lens next runs.
	leadsByLens := make(map[string][]store.Lead, len(f.lenses))
	var leadsConsumedTotal int
	for _, l := range f.lenses {
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

	// Build the unit-of-work list: (lens, chunk) pairs. We launch lenses in
	// yield order so that if degradation kicks in mid-run, the lower-yield lenses
	// are the ones skipped.
	type unit struct {
		lens  Lens
		files []string
		leads []store.Lead // pre-fetched leads for this lens, already consumed
	}
	var units []unit
	for _, l := range f.lenses {
		for _, c := range chunks {
			units = append(units, unit{lens: l, files: c, leads: leadsByLens[l.Name]})
		}
	}

	// leadsPosted counts post_lead tool calls that succeeded across all parallel
	// finder goroutines. Atomic so parallel units can increment it safely.
	var leadsPostedAtomic atomic.Int32

	// Valid lens names for the post_lead tool, derived from all builtin lenses
	// (not just the active subset) so a finder can post to any lens including
	// inactive ones — the lead will be pending until that lens next runs.
	allLensNames := make([]string, 0, len(BuiltinLenses()))
	for _, l := range BuiltinLenses() {
		allLensNames = append(allLensNames, l.Name)
	}

	var (
		mu              sync.Mutex
		collected       []Candidate
		finderRuns      int
		finderFailed    int
		finderBudgetCut int
		firstErr        error
	)
	sem := make(chan struct{}, f.opts.MaxParallel)
	var wg sync.WaitGroup

	degradedLenses := f.degradedLensNames()

	for _, u := range units {
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}

		wg.Add(1)
		u := u
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Gate against the LIVE spend total only once we hold a worker slot, so
			// the decision reflects spend already recorded by earlier units rather
			// than a stale pre-launch snapshot. This is what makes degradation and
			// the hard stop actually bite as the run progresses.
			if budget.overHard() {
				budget.stopped.Store(true)
				msg := fmt.Sprintf("hard budget reached: skipped finder lens %q on %d file(s)", u.lens.Name, len(u.files))
				f.note(result, msg)
				progress.Emit(f.opts.Progress, progress.Event{Kind: progress.KindBudgetStopped, Message: msg})
				return
			}
			if budget.overSoft() {
				budget.degraded.Store(true)
				if !degradedLenses[u.lens.Name] {
					msg := fmt.Sprintf("budget degraded: skipped low-yield finder lens %q on %d file(s)", u.lens.Name, len(u.files))
					f.note(result, msg)
					progress.Emit(f.opts.Progress, progress.Event{Kind: progress.KindBudgetDegraded, Message: msg})
					return
				}
			}

			// Build the tool set for this finder: read-only tools plus a per-unit
			// post_lead instance that carries this lens as the poster. The onPost
			// callback writes through f.store and increments the shared atomic
			// counter; f.store is concurrency-safe so multiple parallel units can
			// post leads simultaneously.
			postLeadTool := agent.NewPostLeadTool(u.lens.Name, allLensNames, func(targetLens, file string, line int, note string) error {
				if err := f.store.AddLead(ctx, store.Lead{
					ScanRunID:  scanRunID,
					PosterLens: u.lens.Name,
					TargetLens: targetLens,
					File:       file,
					Line:       line,
					Note:       note,
				}); err != nil {
					return err
				}
				leadsPostedAtomic.Add(1)
				return nil
			})
			unitTools := append(baseTools, postLeadTool)

			cands, status, err := f.runFinder(ctx, finder, unitTools, persona, u.lens, u.files, u.leads, budget)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			finderRuns++
			switch status {
			case finderParseFailed:
				finderFailed++
				msg := fmt.Sprintf("finder lens %q produced no parseable output on %d file(s) — its findings (if any) are LOST, not absent", u.lens.Name, len(u.files))
				f.note(result, msg)
				progress.Emit(f.opts.Progress, progress.Event{
					Kind: progress.KindLensFailed, Role: progress.RoleFinder, Label: u.lens.Name, Message: msg,
				})
				return
			case finderBudgetStopped:
				// A run truncated by the shared budget pool (or its own token
				// budget) whose partial output does not parse is a budget stop, NOT
				// a reliability problem: the lens did not "fail to parse", it was cut
				// short on purpose. Count it separately and note it under Skipped so a
				// budget-limited scan is never misreported as having broken finders.
				finderBudgetCut++
				msg := fmt.Sprintf("finder lens %q stopped by budget on %d file(s) before emitting parseable output — partial coverage", u.lens.Name, len(u.files))
				f.note(result, msg)
				return
			}
			collected = append(collected, cands...)
		}()
	}

	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	mu.Lock()
	result.Stats.FinderRuns = finderRuns
	result.Stats.FinderFailures = finderFailed
	result.Stats.FinderBudgetStopped = finderBudgetCut
	result.Stats.LeadsConsumed = leadsConsumedTotal
	result.Stats.LeadsPosted = int(leadsPostedAtomic.Load())
	mu.Unlock()
	return collected, nil
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
)

// runFinder executes a single finder agent for one lens over one chunk and maps
// its JSON output to Candidates tagged with the lens. The agent's limits are
// derived from the shared budget pool at launch (remaining-pool allowance plus a
// pre-turn budget check), so a finder launched late gets only the headroom left
// and one already in flight stops at its next turn once the pool is exhausted.
//
// The finderStatus return distinguishes a parse failure (the finder ran but
// produced no parseable JSON even after the repair round-trip, so its result is
// LOST, not a clean "found nothing") from a budget stop (the run was truncated
// by the budget pool / token budget, so an unparseable partial is expected). The
// funnel surfaces parse failures so a scan never silently reports "No findings"
// when a lens actually failed, while budget stops are accounted separately.
func (f *Funnel) runFinder(ctx context.Context, finder llm.Client, tools []agent.Tool, persona string, l Lens, files []string, leads []store.Lead, budget *budgetState) ([]Candidate, finderStatus, error) {
	sink := f.opts.Progress
	start := time.Now()
	progress.Emit(sink, progress.Event{
		Kind: progress.KindAgentStarted, Role: progress.RoleFinder, Label: l.Name,
	})

	runner := agent.NewRunner(finder, tools, finderSystemPrompt(persona, l),
		agent.WithLimits(budget.runnerLimits(f.opts.FinderLimits)),
		agent.WithMaxTokens(DefaultMaxOutputTokens),
		f.transcriptOption(),
	)

	var out candidateList
	outcome, err := runner.RunJSON(ctx, finderTask(files, leads), candidatesSchema, &out)
	emitAgentFinished(sink, progress.RoleFinder, l.Name, outcome, start, err)
	if err != nil {
		// A finder that fails to produce parseable JSON yields no candidates
		// rather than aborting the whole scan: one lens/chunk failing must not
		// sink the others. Context cancellation is the exception — propagate it.
		if ctx.Err() != nil {
			return nil, finderOK, ctx.Err()
		}
		// Distinguish a genuine parse failure from a budget stop. If the run was
		// truncated by the shared budget pool or its own token budget, an
		// unparseable partial is the expected consequence of stopping early, not a
		// reliability problem — classify it as a budget stop so it does not inflate
		// the finder-failure count. Otherwise its findings are LOST: report a parse
		// failure so a scan never silently prints "No findings" when a lens broke.
		if budgetStopped(outcome) {
			return nil, finderBudgetStopped, nil
		}
		return nil, finderParseFailed, nil
	}

	cands := make([]Candidate, 0, len(out.Candidates))
	for _, rc := range out.Candidates {
		cands = append(cands, Candidate{
			Lens:        l.Name,
			File:        rc.File,
			Line:        rc.Line,
			Title:       rc.Title,
			Description: rc.Description,
			Severity:    normalizeSeverity(rc.Severity),
			Evidence:    rc.Evidence,
			Confidence:  normalizeConfidence(rc.Confidence),
		})
	}
	return cands, finderOK, nil
}

// budgetStopped reports whether outcome was truncated by a budget limit (the
// run's own token budget or the shared cross-runner budget pool), as opposed to
// the iteration cap or no truncation at all. An unparseable result from such a
// run is an expected budget stop, not a finder reliability failure.
func budgetStopped(o *agent.Outcome) bool {
	if o == nil || !o.Truncated {
		return false
	}
	return o.TruncationReason == agent.TruncTokenBudget || o.TruncationReason == agent.TruncBudgetPool
}

// degradedLensNames returns the set of lens names that survive budget
// degradation: the top degradedLensCount lenses by yield within the effective
// lens set (which is already yield-ordered).
func (f *Funnel) degradedLensNames() map[string]bool {
	keep := make(map[string]bool, degradedLensCount)
	for i, l := range f.lenses {
		if i >= degradedLensCount {
			break
		}
		keep[l.Name] = true
	}
	return keep
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

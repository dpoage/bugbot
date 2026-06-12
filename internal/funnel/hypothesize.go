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
//
// Diff-intent lens: when the run is commit-scoped (kind == ScanTargeted) AND cc
// is non-nil, exactly ONE extra finder task is emitted for the diff-intent lens
// before the per-chunk units. This task embeds the commit message, the unified
// diff (capped at diffIntentDiffCap), and the blast-radius dependents. No
// diff-intent tasks are emitted on sweeps or when cc is nil; lens selection and
// degradation tolerate a lens that emits zero chunk tasks (degradation
// survivors are computed from lenses that actually emitted units this run).
func (f *Funnel) hypothesize(ctx context.Context, scanRunID string, finder llm.Client, persona string, kind store.ScanKind, cc *ChangeContext, langs []ingest.Language, targets []string, budget *budgetState, result *Result) ([]Candidate, error) {
	if len(targets) == 0 && cc == nil {
		return nil, nil
	}

	// Finders re-send their whole growing history every turn, so a fat read_file
	// result is paid for on every subsequent turn. Tightening the finder's
	// per-read caps shrinks each result at the source — slowing history growth
	// without ever mutating the conversation prefix, which (unlike history
	// compaction) preserves the prompt-cache prefix and so cuts cache-WEIGHTED
	// cost, not just raw tokens (see bugbot-3nf and DefaultFinderReadLines/Bytes).
	baseTools, err := f.readOnlyTools(f.opts.finderReadCaps())
	if err != nil {
		return nil, err
	}

	chunks := chunkByLanguage(targets, f.opts.ChunkSize)

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

	// Build the unit-of-work list: (lens, strategy, chunk) triples. We launch
	// lenses in yield order so that if degradation kicks in mid-run, the
	// lower-yield units are the ones skipped.
	//
	// For each lens × chunk pair the default strategy (sweep-wide) is emitted
	// exactly as today; additionally, each non-default builtin strategy that
	// AppliesTo the lens emits one extra unit per chunk. This is the strategy
	// axis: unit = (lens × strategy × chunk).
	//
	// The diff-intent lens is special: it emits exactly ONE task per commit-kind
	// run (when cc is non-nil), not one task per chunk of files. That task carries
	// its own pre-built content (diffIntentTask) rather than the standard file
	// list. The customTask field is non-empty only for that one unit; all other
	// units leave it empty and have their task built from files+leads as usual.
	// diff-intent always uses sweep-wide (no deep units); AppliesTo for the
	// non-default strategies does not match "diff-intent" in v1, so this falls
	// out naturally from the loop below.
	type unit struct {
		lens       Lens
		strategy   Strategy
		files      []string
		langs      []ingest.Language // the chunk's language set, for prompt composition
		leads      []store.Lead      // pre-fetched leads for this lens, already consumed
		customTask string            // non-empty for diff-intent: overrides task(files, leads)
	}
	strategies := builtinStrategies()
	var units []unit
	for _, l := range lenses {
		// diff-intent never gets chunk-based units: it is either absent (sweeps,
		// nil ChangeContext) or emitted as exactly ONE custom task below. Skipping
		// it here ensures zero tasks from this lens on sweeps while still allowing
		// the selectLenses / degradation logic to treat it as part of the set.
		if l.Name == "diff-intent" {
			continue
		}
		for _, c := range chunks {
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
		mu              sync.Mutex
		collected       []Candidate
		finderRuns      int
		finderFailed    int
		finderBudgetCut int
		firstErr        error
	)
	sem := make(chan struct{}, f.opts.MaxParallel)
	var wg sync.WaitGroup

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
				classKey := u.lens.Name + "@" + u.strategy.Name
				if !degradedUnits[classKey] {
					label := unitLabel(u.lens.Name, u.strategy.Name)
					msg := fmt.Sprintf("budget degraded: skipped low-yield finder lens %q on %d file(s)", label, len(u.files))
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
			// Use a three-index slice expression to cap baseTools at its length
			// before appending, forcing a new backing array for every goroutine.
			// Without the cap expression each parallel finder goroutine calls
			// append on the same shared slice header: when baseTools has spare
			// capacity they all write into baseTools[len(baseTools)] concurrently,
			// a verified data race on the backing array. The three-index form
			// baseTools[:len(baseTools):len(baseTools)] sets cap==len, so every
			// append allocates a fresh array and goroutines never share backing
			// storage.
			unitTools := append(baseTools[:len(baseTools):len(baseTools)], postLeadTool)

			// Resolve the task content. customTask takes highest priority
			// (diff-intent pre-built task). Then the strategy's BuildTask if
			// non-nil (deep strategies supply their own framing). Finally the
			// default finderTask file-list format.
			task := u.customTask
			if task == "" {
				if u.strategy.BuildTask != nil {
					task = u.strategy.BuildTask(u.files, u.leads)
				} else {
					task = finderTask(u.files, u.leads)
				}
			}

			// Compose the system prompt. For sweep-wide (empty SystemClause) the
			// prompt is byte-identical to pre-strategy output. For deep strategies
			// the clause is appended under a labeled heading.
			sysprompt := finderSystemPrompt(persona, u.lens, u.langs)
			if u.strategy.SystemClause != "" {
				sysprompt += "\n\nYOUR SEARCH STRATEGY (" + u.strategy.Name + "):\n" + u.strategy.SystemClause
			}

			label := unitLabel(u.lens.Name, u.strategy.Name)
			cands, status, err := f.runFinderWithPrompt(ctx, finder, unitTools, sysprompt, label, u.lens, task, budget)
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
				msg := fmt.Sprintf("finder lens %q produced no parseable output on %d file(s) — its findings (if any) are LOST, not absent", label, len(u.files))
				f.note(result, msg)
				progress.Emit(f.opts.Progress, progress.Event{
					Kind: progress.KindLensFailed, Role: progress.RoleFinder, Label: label, Message: msg,
				})
				return
			case finderBudgetStopped:
				// A run truncated by the shared budget pool (or its own token
				// budget) whose partial output does not parse is a budget stop, NOT
				// a reliability problem: the lens did not "fail to parse", it was cut
				// short on purpose. Count it separately and note it under Skipped so a
				// budget-limited scan is never misreported as having broken finders.
				finderBudgetCut++
				msg := fmt.Sprintf("finder lens %q stopped by budget on %d file(s) before emitting parseable output — partial coverage", label, len(u.files))
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
func (f *Funnel) runFinder(ctx context.Context, finder llm.Client, tools []agent.Tool, persona string, l Lens, langs []ingest.Language, task string, budget *budgetState) ([]Candidate, finderStatus, error) {
	sysprompt := finderSystemPrompt(persona, l, langs)
	return f.runFinderWithPrompt(ctx, finder, tools, sysprompt, l.Name, l, task, budget)
}

// runFinderWithPrompt is the core finder executor. It accepts a pre-composed
// system prompt and a progress label so callers (hypothesize) can inject
// strategy clauses and use strategy-qualified labels (lens@strategy) without
// rebuilding the prompt inside this function.
func (f *Funnel) runFinderWithPrompt(ctx context.Context, finder llm.Client, tools []agent.Tool, sysprompt, label string, l Lens, task string, budget *budgetState) ([]Candidate, finderStatus, error) {
	sink := f.opts.Progress
	start := time.Now()
	progress.Emit(sink, progress.Event{
		Kind: progress.KindAgentStarted, Role: progress.RoleFinder, Label: label,
	})

	runner := agent.NewRunner(finder, tools, sysprompt,
		agent.WithLimits(budget.runnerLimits(f.opts.FinderLimits)),
		agent.WithMaxTokens(DefaultMaxOutputTokens),
		f.transcriptOption(),
	)

	var out candidateList
	outcome, err := runner.RunJSON(ctx, task, candidatesSchema, &out)
	emitAgentFinished(sink, progress.RoleFinder, label, outcome, start, err)
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
// The sort is stable with a deterministic tiebreak (lens name then strategy
// name) so equal-yield classes never reorder between runs. A deep unit-class
// (weight 0.9) ranks just below its lens's sweep-wide class and is therefore
// shed first under pressure — intended behavior.
//
// CRITICAL INVARIANT: with only sweep-wide in play (weight 1.0), the survivors
// must be exactly the top degradedLensCount lenses by yield — identical to the
// pre-strategy degradedLensNames behavior.
//
// Callers must pass only classes that actually emitted units this run (see
// hypothesize): a zero-unit lens must never occupy a survivor slot.
func degradedUnitClasses(classes []lensStrategyClass, langs []ingest.Language) map[string]bool {
	type ranked struct {
		key          string
		score        float64
		lensName     string
		strategyName string
	}
	r := make([]ranked, len(classes))
	for i, c := range classes {
		score := float64(effectiveYield(c.lensName, langs)) * c.weight
		r[i] = ranked{
			key:          c.lensName + "@" + c.strategyName,
			score:        score,
			lensName:     c.lensName,
			strategyName: c.strategyName,
		}
	}
	sort.SliceStable(r, func(i, j int) bool {
		if r[i].score != r[j].score {
			return r[i].score > r[j].score
		}
		if r[i].lensName != r[j].lensName {
			return r[i].lensName < r[j].lensName
		}
		return r[i].strategyName < r[j].strategyName
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

// diffIntentLens returns the Lens descriptor for the diff-intent lens. It is
// defined in BuiltinLenses (lens.go) but fetched by name here so hypothesize
// does not hard-code index offsets into the lens slice.
func diffIntentLens() Lens {
	for _, l := range BuiltinLenses() {
		if l.Name == "diff-intent" {
			return l
		}
	}
	// Unreachable if BuiltinLenses is kept in sync with lens.go; panic loudly
	// so a deletion is caught immediately rather than silently dropping the lens.
	panic("funnel: diff-intent lens not found in BuiltinLenses; check lens.go")
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

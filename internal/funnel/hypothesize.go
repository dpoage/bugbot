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

	// Build the unit-of-work list: (lens, chunk) pairs. We launch lenses in
	// yield order so that if degradation kicks in mid-run, the lower-yield lenses
	// are the ones skipped.
	//
	// The diff-intent lens is special: it emits exactly ONE task per commit-kind
	// run (when cc is non-nil), not one task per chunk of files. That task carries
	// its own pre-built content (diffIntentTask) rather than the standard file
	// list. The customTask field is non-empty only for that one unit; all other
	// units leave it empty and have their task built from files+leads as usual.
	type unit struct {
		lens       Lens
		files      []string
		langs      []ingest.Language // the chunk's language set, for prompt composition
		leads      []store.Lead      // pre-fetched leads for this lens, already consumed
		customTask string            // non-empty for diff-intent: overrides finderTask(files, leads)
	}
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
			units = append(units, unit{lens: l, files: c.files, langs: c.langs, leads: leadsByLens[l.Name]})
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

	// Compute degraded survivors from the lenses that actually emitted at least
	// one unit in this run, ranked by the per-language yield order — composing
	// both halves of the design: lensesByYield supplies the language-aware
	// ranking, and the units filter ensures a zero-unit lens (diff-intent on
	// sweeps) never occupies a degradation slot and starves a taxonomy lens. On
	// commit runs diff-intent does emit a unit and legitimately competes.
	activeLensNames := make(map[string]bool, len(units))
	for _, u := range units {
		activeLensNames[u.lens.Name] = true
	}
	active := make([]Lens, 0, len(lenses))
	for _, l := range lenses {
		if activeLensNames[l.Name] {
			active = append(active, l)
		}
	}
	degradedLenses := degradedLensNames(active)

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

			// Resolve the task content. For the diff-intent lens the task is
			// pre-built (customTask) and carries the commit message, diff, and
			// blast-radius dependents. For all other lenses the task is the
			// standard file-list+leads format. The chunk's language set rides
			// alongside for manifestation-block prompt composition (nil for the
			// language-free diff-intent task → Core-only prompt).
			task := u.customTask
			if task == "" {
				task = finderTask(u.files, u.leads)
			}

			cands, status, err := f.runFinder(ctx, finder, unitTools, persona, u.lens, u.langs, task, budget)
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
func (f *Funnel) runFinder(ctx context.Context, finder llm.Client, tools []agent.Tool, persona string, l Lens, langs []ingest.Language, task string, budget *budgetState) ([]Candidate, finderStatus, error) {
	sink := f.opts.Progress
	start := time.Now()
	progress.Emit(sink, progress.Event{
		Kind: progress.KindAgentStarted, Role: progress.RoleFinder, Label: l.Name,
	})

	runner := agent.NewRunner(finder, tools, finderSystemPrompt(persona, l, langs),
		agent.WithLimits(budget.runnerLimits(f.opts.FinderLimits)),
		agent.WithMaxTokens(DefaultMaxOutputTokens),
		f.transcriptOption(),
	)

	var out candidateList
	outcome, err := runner.RunJSON(ctx, task, candidatesSchema, &out)
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
// degradation: the head degradedLensCount lenses of ordered, which must
// already be sorted by descending effective yield for this run's language mix
// (see lensesByYield). On a Python-heavy repo this keeps a different lens set
// than on a Go repo — degradation sheds the lenses that are low-yield for THIS
// repo, not for Go.
//
// Callers must pre-filter ordered to lenses that actually emitted units this
// run (see hypothesize): a zero-unit lens (diff-intent on sweeps) must never
// occupy a survivor slot and starve a working lens.
func degradedLensNames(ordered []Lens) map[string]bool {
	keep := make(map[string]bool, degradedLensCount)
	for i, l := range ordered {
		if i >= degradedLensCount {
			break
		}
		keep[l.Name] = true
	}
	return keep
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

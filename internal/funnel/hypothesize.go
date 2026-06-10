package funnel

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
)

// hypothesize runs the finder stage: for each effective lens, run a finder
// agent over each chunk of target files, collecting concrete candidates. Lens
// chunks run in parallel bounded by Options.MaxParallel. Budget degradation is
// applied as the run progresses: once over the soft threshold only the
// highest-yield lenses keep launching, and once over the hard threshold no new
// finder agents are launched.
func (f *Funnel) hypothesize(ctx context.Context, finder llm.Client, targets []string, budget *budgetState, result *Result) ([]Candidate, error) {
	if len(targets) == 0 {
		return nil, nil
	}

	tools, err := f.readOnlyTools()
	if err != nil {
		return nil, err
	}

	chunks := chunk(targets, f.opts.ChunkSize)

	// Build the unit-of-work list: (lens, chunk) pairs. We launch lenses in
	// yield order so that if degradation kicks in mid-run, the lower-yield lenses
	// are the ones skipped.
	type unit struct {
		lens  Lens
		files []string
	}
	var units []unit
	for _, l := range f.lenses {
		for _, c := range chunks {
			units = append(units, unit{lens: l, files: c})
		}
	}

	var (
		mu        sync.Mutex
		collected []Candidate
		firstErr  error
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

			cands, err := f.runFinder(ctx, finder, tools, u.lens, u.files, budget)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			collected = append(collected, cands...)
		}()
	}

	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return collected, nil
}

// runFinder executes a single finder agent for one lens over one chunk and maps
// its JSON output to Candidates tagged with the lens. The agent's limits are
// derived from the shared budget pool at launch (remaining-pool allowance plus a
// pre-turn budget check), so a finder launched late gets only the headroom left
// and one already in flight stops at its next turn once the pool is exhausted.
func (f *Funnel) runFinder(ctx context.Context, finder llm.Client, tools []agent.Tool, l Lens, files []string, budget *budgetState) ([]Candidate, error) {
	sink := f.opts.Progress
	start := time.Now()
	progress.Emit(sink, progress.Event{
		Kind: progress.KindAgentStarted, Role: progress.RoleFinder, Label: l.Name,
	})

	runner := agent.NewRunner(finder, tools, finderSystemPrompt(l),
		agent.WithLimits(budget.runnerLimits(f.opts.FinderLimits)),
		f.transcriptOption(),
	)

	var out candidateList
	outcome, err := runner.RunJSON(ctx, finderTask(files), candidatesSchema, &out)
	emitAgentFinished(sink, progress.RoleFinder, l.Name, outcome, start, err)
	if err != nil {
		// A finder that fails to produce parseable JSON yields no candidates
		// rather than aborting the whole scan: one lens/chunk failing must not
		// sink the others. Context cancellation is the exception — propagate it.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
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
	return cands, nil
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

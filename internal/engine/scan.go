package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dpoage/bugbot/internal/analyzer"
	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/miner"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// maxFixpointRounds bounds the oneshot drain loop so a pathological
// non-converging case (a finding the LLM repeatedly leaves ambiguous, so its
// swept_at never gets set) cannot spin. Round 1 does the work; a second
// confirms convergence; a third absorbs a verify-drain that surfaces a
// finding the same round's sweep then re-ranks.
const maxFixpointRounds = 3

// ScanOpts holds the parsed flag values for `bugbot scan`.
type ScanOpts struct {
	Target string
	Since  string
	// From is the inclusive lower bound of a commit range scan (regress).
	// When set, the scan scopes its blast radius to the diff from..to and
	// labels each finding INTRODUCED vs PRE-EXISTING after the run. It is
	// mutually exclusive with Since at the CLI surface (only one is ever set
	// per command).
	From string
	// To is the upper bound of a commit range scan; defaults to HEAD when
	// empty. It is only consulted when From is also set.
	To          string
	Concurrency int
	Refuters    int
	Lenses      []string
	DoRepro     bool
	DoEstimate  bool
	Force       bool

	// Out is the primary human-readable output stream (cmd.OutOrStdout()).
	Out io.Writer
	// ErrOut is the diagnostics/warnings stream (cmd.ErrOrStderr()); also
	// where the pre-refactor code printed live-pane / log-renderer output.
	ErrOut io.Writer
	// StopProgress, if non-nil, is called at the two points the pre-refactor
	// runScanCmd stopped its live pane: right before an --estimate short
	// circuit, and right before the final result is ready to print. It lets
	// CLI-owned progress renderers (which Scan never sees directly) stop
	// cleanly before terminal output. Safe to call multiple times.
	StopProgress func()
}

// ScanResult is everything the `bugbot scan` RunE needs to render its output.
// Rendering itself (printResult / printEstimate / printRegressAttribution)
// stays in internal/cli.
type ScanResult struct {
	// Result is nil when Estimate is set (an --estimate run never executes
	// the funnel).
	Result *funnel.Result
	// Estimate is non-nil only for a DoEstimate run.
	Estimate *funnel.Estimate
	// Repo is the opened target repository, exposed so the CLI can render
	// --from regress attribution without re-opening it.
	Repo *ingest.Repo
}

// Scan implements `bugbot scan`: it loads the target repository, builds the
// finder/verifier LLM clients from the role mappings, runs the funnel (a
// whole-snapshot Sweep, or a blast-radius-scoped Targeted scan when Since or
// From is given), drains verify/impact-sweep to fixpoint, and optionally runs
// the reproduce stage.
func (d *Dispatcher) Scan(ctx context.Context, opts ScanOpts) (*ScanResult, error) {
	// Advisory single-scan lock: refuse if another process is actively
	// scanning this state db (heartbeat fresh, not finished, different pid).
	// Force bypasses the check (and escalates from Observer to Owner) so an
	// operator can override a stale lock.
	if err := d.ensureOwner(ctx, opts.Force); err != nil {
		return nil, err
	}

	cfg := d.cfg
	st := d.store
	errOut := opts.ErrOut
	stopProgress := opts.StopProgress
	if stopProgress == nil {
		stopProgress = func() {}
	}

	// Repo opens before role-client resolution, matching main's
	// openRepoForScan-then-buildRoleClients order (openStoreForScan already
	// happened via engine.Open before Scan was called).
	repo, err := d.openRepo(ctx, opts.Target)
	if err != nil {
		return nil, err
	}

	if err := d.ensureRoleClients(ctx); err != nil {
		return nil, err
	}

	// Build the reproducer and wire it as an in-run hook when --repro is set.
	reproHook, reproRec, r, reproAttempted, err := buildReproHookForScan(ctx, errOut, cfg, st, opts, d.sink)
	if err != nil {
		return nil, err
	}

	funnelOpts, sandboxDegraded, sbErr := BuildFunnelOptions(cfg, FunnelOptionOverrides{
		Lenses:      opts.Lenses,
		Refuters:    opts.Refuters,
		MaxParallel: opts.Concurrency,
		Progress:    d.sink,
		Repro:       reproHook,
	})
	if sbErr != nil {
		return nil, sbErr
	}
	if sandboxDegraded {
		PrintSandboxDegradedWarning(errOut)
	}
	f, err := funnel.New(funnel.RoleClients{Finder: d.finder, Verifier: d.verifier, Cartographer: d.cartographer, Arbiter: d.arbiter}, st, repo, funnelOpts)
	if err != nil {
		return nil, err
	}
	// Shut down any language servers the code-navigation tools spawned.
	defer func() { _ = f.Close() }()

	// Resolve the scan scope: a Targeted blast-radius run when Since or the
	// (From[,To]) regress range is given, otherwise a whole-snapshot Sweep.
	// Targeted runs populate ChangeContext (for the diff-intent lens) and
	// rebuild the funnel so hypothesize sees it. Computed before seeding and
	// the estimate short-circuit so every path agrees on scope.
	var changed []string
	kind := store.ScanOneshot
	var (
		fromRef string // empty for whole-snapshot sweeps
		toRef   string // always populated when fromRef is set (defaults to HEAD)
	)
	switch {
	case opts.Since != "":
		fromRef = opts.Since
		head, herr := repo.HeadCommit(ctx)
		if herr != nil {
			return nil, fmt.Errorf("resolve HEAD: %w", herr)
		}
		toRef = head
		changes, cerr := repo.ChangedFiles(ctx, fromRef, toRef)
		if cerr != nil {
			return nil, fmt.Errorf("diff %s..%s: %w", fromRef, toRef, cerr)
		}
		changed = ingest.ChangedPaths(changes)
		_, _ = fmt.Fprintf(errOut, "Targeted scan: %d changed file(s) since %s\n", len(changed), fromRef)
	case opts.From != "":
		fromRef = opts.From
		toRef = opts.To
		if toRef == "" {
			head, herr := repo.HeadCommit(ctx)
			if herr != nil {
				return nil, fmt.Errorf("resolve HEAD: %w", herr)
			}
			toRef = head
		}
		changes, cerr := repo.ChangedFiles(ctx, fromRef, toRef)
		if cerr != nil {
			return nil, fmt.Errorf("diff %s..%s: %w", fromRef, toRef, cerr)
		}
		changed = ingest.ChangedPaths(changes)
		_, _ = fmt.Fprintf(errOut, "Regress scan: %d changed file(s) in %s..%s\n", len(changed), fromRef, toRef)
	}
	if fromRef != "" {
		// Populate ChangeContext for the diff-intent lens. Failures are
		// non-fatal: the scan still runs without diff-intent context.
		cc := buildScanChangeContext(ctx, repo, fromRef, toRef, changed)
		if cc != nil {
			funnelOpts.Discovery.ChangeContext = cc
			// Rebuild the funnel with the updated options so ChangeContext is
			// visible to hypothesize. The old funnel (f) has not run yet so no
			// language servers have been started. Only swap f after a
			// successful rebuild so a failure here cannot leave f nil and
			// cause the deferred f.Close() to panic ((*Funnel).Close has a
			// nil-receiver guard, but we still prefer not to lose f).
			f2, buildErr := funnel.New(funnel.RoleClients{Finder: d.finder, Verifier: d.verifier, Cartographer: d.cartographer, Arbiter: d.arbiter}, st, repo, funnelOpts)
			if buildErr != nil {
				return nil, buildErr
			}
			_ = f.Close()
			f = f2
		}
		kind = store.ScanTargeted
	}

	// --estimate: project this run's token spend and wall time WITHOUT any
	// LLM call (and without the analyzer/repro container work below), then
	// stop. The work breakdown is exact; the token/time figures are
	// calibrated from recorded history when available, else labeled priors.
	if opts.DoEstimate {
		est, eerr := f.EstimateScan(ctx, kind, changed)
		if eerr != nil {
			return nil, eerr
		}
		stopProgress()
		return &ScanResult{Estimate: est, Repo: repo}, nil
	}

	// Analyzer seeding: run deterministic static analyzers (staticcheck,
	// ruff) to seed the leads blackboard before the finder stage. Always-on
	// with graceful-skip: if no container runtime is available, or the
	// analyzer binary is absent from the image, the seed step is silently
	// skipped. Analyzer failures never block the scan.
	RunAnalyzerSeed(ctx, cfg, repo.Root(), st, d.sink)

	// Doc-contradiction seeding: a pure-Go, in-process pass that mines
	// documented-sentinel-vs-validator contradictions (the bugbot-ig7 class)
	// and posts them as leads. Unlike analyzer seeding it needs no container
	// runtime, so it always runs.
	RunContradictionSeed(ctx, cfg, repo, st, d.sink)

	res, err := executeScan(ctx, f, kind, changed)
	if err != nil {
		// If the funnel failed with a SQLite IOERR, run the store's
		// quick_check + reopen Diagnose so the operator log makes the
		// failure mode self-explaining (transient VFS race vs on-disk
		// corruption). The original err is returned unchanged; the
		// diagnostic lines go to stderr.
		if store.IsIOErr(err) {
			logStoreDiagnose(ctx, st, err)
		}
		return nil, err
	}

	// Drive the verify and impact-sweep drains to fixpoint so a single
	// interrupted-then-rerun scan converges. run() already replayed the
	// pending_candidates WAL at start (stranded candidates verified), so this
	// reconciles the rest: VerifyDrain clears any pending this process left,
	// and SweepDrain re-ranks every open finding not yet swept — INCLUDING
	// this scan's, since the terminal Stage F no longer re-ranks inline.
	// Bounded and convergence-safe (VerifyDrain only deletes pending,
	// SweepDrain only sets swept_at, so work-remaining shrinks
	// monotonically). Best-effort.
	reranked, drainErr := drainToFixpoint(ctx, f, st)

	// Stage F moved out of run(), so res.Findings carry PRE-sweep severities.
	// Refresh them from the re-ranked set so the oneshot summary matches the
	// store and any published issues (oracle #3).
	applyReranked(res.Findings, reranked)

	// Tear down the live pane before printing the final summary so the
	// summary is not interleaved with in-place repaints.
	stopProgress()

	if drainErr != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(errOut, "\nWarning: post-scan drain incomplete (finding severities may be stale): %v\n", drainErr)
	}

	if opts.DoRepro && r != nil {
		// Wire spend to this scan run now that we have the ID. In-run hook
		// calls already used this recorder; setting the run ID here ensures
		// any catch-up drain spend is also attributed correctly. The actual
		// catch-up drain (backlog-style PromoteAll over findings the in-run
		// hook missed) runs from ReproCatchUp, called by the CLI AFTER it
		// prints the result — main ran this drain on stderr, after the
		// findings summary and regress attribution, right before the
		// reliability gate; keeping it out of Scan preserves that ordering
		// and stream.
		if reproRec != nil {
			reproRec.SetScanRun(res.ScanRunID)
		}
		d.scanRepro = r
		d.scanReproAttempted = reproAttempted
	}

	return &ScanResult{Result: res, Repo: repo}, nil
}

// ReproCatchUp runs the post-scan repro catch-up drain a prior Scan call (with
// DoRepro set) queued: a backlog-style drain over open T2 findings from that
// scan with no prior repro attempt. This is a cheap no-op when the in-run
// hook covered everything; it exists as a safety net for findings that
// overflowed the reproCh buffer or were produced by a very fast scan with a
// slow sandbox. It is a no-op (nil error) if the preceding Scan call did not
// request DoRepro or had no reproducer available (no container runtime, etc).
//
// Callers should invoke this AFTER rendering the scan result — matching
// main's ordering — and errOut should be the same stderr stream Scan's other
// diagnostics go to.
func (d *Dispatcher) ReproCatchUp(ctx context.Context, res *ScanResult, errOut io.Writer) error {
	if d.scanRepro == nil || res == nil || res.Result == nil {
		return nil
	}
	return runReproCatchUp(ctx, errOut, d.scanRepro, d.store, res.Result.Findings, d.scanReproAttempted)
}

// executeScan runs the funnel: Targeted when changed files are provided,
// Sweep otherwise. It is the innermost independently-callable stage.
func executeScan(ctx context.Context, f *funnel.Funnel, kind store.ScanKind, changed []string) (*funnel.Result, error) {
	if kind == store.ScanTargeted {
		return f.Targeted(ctx, changed)
	}
	return f.Sweep(ctx)
}

// drainToFixpoint runs VerifyDrain then SweepDrain repeatedly until neither
// has work left (no pending candidates AND no unswept open findings) or the
// round cap is hit. It returns the union of the findings SweepDrain
// re-ranked, keyed by finding ID, so the caller can refresh the PRE-sweep
// severities its scan Result carries, plus the first drain/query error
// encountered (nil on clean convergence). The order matches the daemon:
// verify→sweep, so a candidate verified into a finding in a round is swept in
// the same round.
func drainToFixpoint(ctx context.Context, f *funnel.Funnel, st *store.Store) (map[string]domain.Finding, error) {
	reranked := make(map[string]domain.Finding)
	for round := 0; round < maxFixpointRounds; round++ {
		if err := ctx.Err(); err != nil {
			return reranked, err
		}
		if _, err := f.VerifyDrain(ctx); err != nil {
			return reranked, err
		}
		sres, err := f.SweepDrain(ctx)
		if err != nil {
			return reranked, err
		}
		for _, fnd := range sres.Findings {
			reranked[fnd.ID] = fnd
		}

		pending, err := st.ListPendingCandidates(ctx)
		if err != nil {
			return reranked, err
		}
		unswept, err := st.UnsweptOpenFindings(ctx)
		if err != nil {
			return reranked, err
		}
		if len(pending) == 0 && len(unswept) == 0 {
			return reranked, nil
		}
	}
	return reranked, nil
}

// applyReranked refreshes each finding's severity and verdict detail from the
// post-sweep set produced by drainToFixpoint, matched by finding ID. With the
// terminal Stage F removed from run(), a scan Result carries PRE-sweep
// severities; this brings the oneshot summary in line with the store and any
// published issues (oracle #3). Findings absent from the set (e.g. nothing
// unswept to re-rank) are left untouched.
func applyReranked(findings []domain.Finding, reranked map[string]domain.Finding) {
	for i := range findings {
		if rr, ok := reranked[findings[i].ID]; ok {
			findings[i].Severity = rr.Severity
			findings[i].VerdictDetail = rr.VerdictDetail
		}
	}
}

// buildScanChangeContext populates a funnel.ChangeContext for a --since
// targeted scan, enabling the diff-intent lens. Failures are best-effort and
// return nil so the scan proceeds without the extra context rather than
// aborting.
func buildScanChangeContext(ctx context.Context, repo *ingest.Repo, fromSHA, toSHA string, changed []string) *funnel.ChangeContext {
	if fromSHA == "" || toSHA == "" {
		return nil
	}
	cc := &funnel.ChangeContext{
		FromCommit:   fromSHA,
		ToCommit:     toSHA,
		ChangedFiles: changed,
		// BlastFiles intentionally omitted: derived inside hypothesize from
		// the blast-radius targets that Targeted already computed via
		// BlastRadius.
	}
	msg, err := repo.CommitMessage(ctx, toSHA)
	if err == nil {
		cc.Message = msg
	}
	diff, err := repo.UnifiedDiff(ctx, fromSHA, toSHA)
	if err == nil {
		cc.Diff = diff
	}
	return cc
}

// runAnalyzerSeed attempts to run the static-analyzer seeding step before the
// funnel. It detects the container runtime, builds a sandbox, and calls
// analyzer.Seed. All failure modes degrade to a logged skip — this function
// never returns an error and never blocks the scan.
//
// The seed step is always-on (no config knob in v1) but requires a container
// runtime: if no runtime is available the step is skipped silently.
func RunAnalyzerSeed(ctx context.Context, cfg config.Config, repoDir string, st *store.Store, sink progress.EventSink) {
	runtime, ok := sandbox.Detect()
	if !ok {
		// No container runtime: skip seeding silently. The scan still runs; it
		// just won't have static-analyzer leads to seed the finder stage with.
		return
	}
	sb, err := sandbox.NewCLI(runtime, cfg.Sandbox.Image, sandboxRunOpts(cfg)...)
	if err != nil {
		// Sandbox construction failed: emit a note and continue without seeding.
		progress.Emit(sink, progress.Event{
			Kind:    progress.KindStageStarted,
			Stage:   "analyzer_seed",
			Message: fmt.Sprintf("analyzer seed skipped: build sandbox: %s", err),
		})
		return
	}

	sum, err := analyzer.Seed(ctx, sb, repoDir, st, cfg.Sandbox.Image)
	if err != nil {
		// Store infrastructure error: emit a note. Seed already posted whatever
		// it could before the error; the scan continues normally.
		progress.Emit(sink, progress.Event{
			Kind:    progress.KindStageFinished,
			Stage:   "analyzer_seed",
			Message: fmt.Sprintf("analyzer seed partial error: %s", err),
		})
		return
	}

	if sum.TotalPosted > 0 {
		progress.Emit(sink, progress.Event{
			Kind:  progress.KindStageFinished,
			Stage: "analyzer_seed",
			Count: sum.TotalPosted,
		})
	}
}

// runContradictionSeed runs the deterministic doc-contradiction miner before
// the funnel. Unlike runAnalyzerSeed it needs no container runtime: the miner
// is a pure-Go, in-process pass over the repository snapshot, so it always
// runs. All failure modes degrade to a logged skip — it never returns an
// error and never blocks the scan.
func RunContradictionSeed(ctx context.Context, cfg config.Config, repo *ingest.Repo, st *store.Store, sink progress.EventSink) {
	snap, err := repo.Snapshot(ctx, ingest.ScanFilter{Include: cfg.Scan.Include, Exclude: cfg.Scan.Exclude})
	if err != nil {
		progress.Emit(sink, progress.Event{
			Kind:    progress.KindStageStarted,
			Stage:   "contradiction_seed",
			Message: fmt.Sprintf("contradiction seed skipped: snapshot: %s", err),
		})
		return
	}
	sum, err := miner.Seed(ctx, snap, st)
	if err != nil {
		// Store infrastructure error: the miner posted whatever it could before
		// the error; the scan continues normally.
		progress.Emit(sink, progress.Event{
			Kind:    progress.KindStageFinished,
			Stage:   "contradiction_seed",
			Message: fmt.Sprintf("contradiction seed partial error: %s", err),
		})
		return
	}
	if sum.LeadsPosted > 0 {
		progress.Emit(sink, progress.Event{
			Kind:  progress.KindStageFinished,
			Stage: "contradiction_seed",
			Count: sum.LeadsPosted,
		})
	}
}

// logStoreDiagnose is the IOERR-triage sink for scan errors. When the scan
// aborts with a SQLITE_IOERR-class error, we run the store's quick_check +
// reopen Diagnose and log the outcome to stderr so an operator can tell at a
// glance whether the failure was a transient VFS race (Diagnose clean) or a
// sign of on-disk corruption (Diagnose fails). The original scan error is the
// caller's responsibility to return — this function only emits diagnostics.
func logStoreDiagnose(ctx context.Context, st *store.Store, triggerErr error) {
	if st == nil {
		return
	}
	fmt.Fprintf(os.Stderr,
		"bugbot: store IOERR triage: trigger=%s\n",
		triggerErr.Error())
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	diagErr := st.Diagnose(dctx)
	if diagErr == nil {
		fmt.Fprintf(os.Stderr,
			"bugbot: store IOERR triage: quick_check=ok reopen=ok (transient VFS race is the most likely cause; the next process start can usually proceed)\n")
		return
	}
	fmt.Fprintf(os.Stderr,
		"bugbot: store IOERR triage: %s (db may be corrupted; inspect quick_check output above before next run)\n",
		diagErr.Error())
}

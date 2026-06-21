package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/analyzer"
	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/miner"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/repro"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/bugbot/internal/util"
)

// ScanFlags holds the parsed flag values for `bugbot scan`. It mirrors the
// per-command overrides pattern used by FunnelOptionOverrides so scan-specific
// configuration is grouped and independently testable.
type ScanFlags struct {
	Target      string
	Since       string
	IncludeT3   bool
	Concurrency int
	Refuters    int
	Lenses      []string
	DoRepro     bool
	DoEstimate  bool
	Force       bool
}

// addTargetFlag registers the shared --target flag on cmd, binding to the
// provided pointer. All scan-family commands (scan, review, repro, prime,
// cartography, design-sandbox) carry an identical --target flag; this helper
// is the single definition of its name, default, and usage text.
func addTargetFlag(cmd *cobra.Command, dest *string) {
	cmd.Flags().StringVar(dest, "target", ".", "path to the target repository")
}

// newScanCmd runs a single pass of the detection funnel over a target repo. It
// loads config, opens the state store and the target repository, builds the
// finder/verifier LLM clients from the role mappings, runs the funnel (a whole-
// snapshot Sweep, or a blast-radius-scoped Targeted scan when --since is given),
// and prints a human summary of the findings, per-stage counts, and spend.
//
// Exit code is 0 on a reliable run (regardless of whether findings were found),
// and nonzero only when the scan is untrustworthy — specifically when most
// finder agents produced no parseable output (Stats.MostFindersFailed). The
// findings count is printed so callers can detect "found something" by parsing
// the summary, and a prominent reliability warning is printed whenever any finder
// failed so an empty result is never mistaken for a clean bill of health.
func newScanCmd() *cobra.Command {
	var flags ScanFlags

	cmd := &cobra.Command{
		Use:   "scan [flags]",
		Short: "Run the detection funnel once over a target repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			// Wire SIGINT/SIGTERM → context cancellation so Ctrl-C produces an
			// interrupted finalization (scan_runs row sealed with interrupted=true)
			// rather than a hard kill that leaves the row dangling. The daemon
			// registers its own NotifyContext; this is the scan-command registration.
			// Using signal.NotifyContext (not signal.Notify) avoids double-registration
			// risk: each call returns a new channel and a distinct stop function.
			ctx, stopSignal := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stopSignal()
			return runScanCmd(ctx, cmd, flags)
		},
	}

	addTargetFlag(cmd, &flags.Target)
	cmd.Flags().StringVar(&flags.Since, "since", "", "scan only the blast radius of changes since this commit (targeted scan)")
	cmd.Flags().BoolVar(&flags.IncludeT3, "include-suspected", false, "include T3 (suspected) findings in output (reserved; this stage emits T2)")
	cmd.Flags().IntVar(&flags.Concurrency, "concurrency", funnel.DefaultMaxParallel, "number of parallel agents")
	cmd.Flags().IntVar(&flags.Refuters, "refuters", funnel.DefaultRefuters, "number of adversarial refuter agents per candidate")
	cmd.Flags().StringSliceVar(&flags.Lenses, "lens", nil, "restrict finder lenses (repeatable); default is all built-in lenses")
	cmd.Flags().BoolVar(&flags.DoRepro, "repro", false, "run the Reproduce stage: generate sandboxed failing tests and promote demonstrated findings to Tier-1")
	cmd.Flags().BoolVar(&flags.DoEstimate, "estimate", false, "estimate token spend and wall time for this scan without running it (no LLM calls)")
	cmd.Flags().BoolVar(&flags.Force, "force", false, "bypass the advisory single-scan lock and proceed even if another scan appears active")

	return cmd
}

// runScanCmd executes the scan pipeline. It is extracted from the RunE closure
// so each stage is independently callable and testable. The ctx passed in must
// already have signal cancellation wired by the caller.
func runScanCmd(ctx context.Context, cmd *cobra.Command, flags ScanFlags) error {
	cfg, st, err := openStoreForScan(ctx, cmd)
	if err != nil {
		return err
	}
	defer closeStore(st)

	// Advisory single-scan lock: refuse if another process is actively
	// scanning this state db (heartbeat fresh, not finished, different pid).
	// --force bypasses the check so an operator can override a stale lock.
	if lockErr := checkScanLock(ctx, st, flags.Force, os.Getpid()); lockErr != nil {
		return lockErr
	}

	repo, err := openRepoForScan(ctx, flags.Target)
	if err != nil {
		return err
	}

	finder, verifier, cartographer, err := buildRoleClients(ctx, &cfg)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()

	// Activity visibility: a snapshot sink (so `bugbot status` can read this
	// run from another terminal) plus a live renderer — an in-place ANSI pane
	// when stdout is a TTY, or plain log lines when piped. The pane is stopped
	// before the final summary so it leaves the terminal clean.
	snap := progress.NewSnapshotSink(storageDir(cfg))
	var (
		pane     *progress.PaneRenderer
		liveSink progress.EventSink
	)
	if progress.IsTerminal(out) {
		pane = progress.NewPaneRenderer(out, 0)
		liveSink = pane
	} else {
		liveSink = progress.NewLogRenderer(out)
	}
	progressSink := progress.NewMulti(liveSink, snap)
	stopPane := func() {
		if pane != nil {
			pane.Stop()
			pane = nil
		}
	}
	defer stopPane()

	// Build the reproducer and wire it as an in-run hook when --repro is set.
	reproHook, reproRec, r, reproAttempted, err := buildReproHookForScan(ctx, out, cfg, st, flags, progressSink)
	if err != nil {
		return err
	}

	opts, sandboxDegraded, sbErr := buildFunnelOptions(cfg, FunnelOptionOverrides{
		Lenses:      flags.Lenses,
		Refuters:    flags.Refuters,
		MaxParallel: flags.Concurrency,
		Progress:    progressSink,
		Repro:       reproHook,
	})
	if sbErr != nil {
		return sbErr
	}
	if sandboxDegraded {
		printSandboxDegradedWarning(cmd.OutOrStdout())
	}
	f, err := funnel.New(funnel.RoleClients{Finder: finder, Verifier: verifier, Cartographer: cartographer}, st, repo, opts)
	if err != nil {
		return err
	}
	// Shut down any language servers the code-navigation tools spawned.
	defer func() { _ = f.Close() }()

	// Resolve the scan scope: a Targeted blast-radius run when --since is
	// given, otherwise a whole-snapshot Sweep. Targeted runs populate
	// ChangeContext (for the diff-intent lens) and rebuild the funnel so
	// hypothesize sees it. Computed before seeding and the estimate
	// short-circuit so every path agrees on scope.
	var changed []string
	kind := store.ScanOneshot
	if flags.Since != "" {
		head, herr := repo.HeadCommit(ctx)
		if herr != nil {
			return fmt.Errorf("resolve HEAD: %w", herr)
		}
		changes, cerr := repo.ChangedFiles(ctx, flags.Since, head)
		if cerr != nil {
			return fmt.Errorf("diff %s..HEAD: %w", flags.Since, cerr)
		}
		changed = ingest.ChangedPaths(changes)
		_, _ = fmt.Fprintf(out, "Targeted scan: %d changed file(s) since %s\n", len(changed), flags.Since)
		// Populate ChangeContext for the diff-intent lens. Failures are
		// non-fatal: the scan still runs without diff-intent context.
		cc := buildScanChangeContext(ctx, repo, flags.Since, head, changed)
		if cc != nil {
			opts.Discovery.ChangeContext = cc
			// Rebuild the funnel with the updated options so ChangeContext is
			// visible to hypothesize. The old funnel (f) has not run yet so no
			// language servers have been started. Only swap f after a
			// successful rebuild so a failure here cannot leave f nil and
			// cause the deferred f.Close() to panic ((*Funnel).Close has a
			// nil-receiver guard, but we still prefer not to lose f).
			f2, buildErr := funnel.New(funnel.RoleClients{Finder: finder, Verifier: verifier, Cartographer: cartographer}, st, repo, opts)
			if buildErr != nil {
				return buildErr
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
	if flags.DoEstimate {
		est, eerr := f.EstimateScan(ctx, kind, changed)
		if eerr != nil {
			return eerr
		}
		stopPane()
		printEstimate(out, est)
		return nil
	}

	// Analyzer seeding: run deterministic static analyzers (staticcheck,
	// ruff) to seed the leads blackboard before the finder stage. Always-on
	// with graceful-skip: if no container runtime is available, or the
	// analyzer binary is absent from the image, the seed step is silently
	// skipped. Analyzer failures never block the scan.
	runAnalyzerSeed(ctx, cfg, repo.Root(), st, progressSink)

	// Doc-contradiction seeding: a pure-Go, in-process pass that mines
	// documented-sentinel-vs-validator contradictions (the bugbot-ig7
	// class) and posts them as leads. Unlike analyzer seeding it needs no
	// container runtime, so it always runs.
	runContradictionSeed(ctx, cfg, repo, st, progressSink)

	// Heartbeat goroutine: periodically updates the scan_run row so
	// ActiveScanRuns can distinguish us from a crashed/stale process.
	// The goroutine resolves our run ID by querying for the most-recently
	// started run belonging to this pid (BeginScanRun inside the funnel
	// creates it synchronously before any agent goroutines start).
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go runHeartbeat(hbCtx, st, os.Getpid())

	res, err := executeScan(ctx, f, kind, changed)
	if err != nil {
		// If the funnel failed with a SQLite IOERR, run the
		// store's quick_check + reopen Diagnose so the
		// operator log makes the failure mode
		// self-explaining (transient VFS race vs on-disk
		// corruption). The original err is returned
		// unchanged; the diagnostic lines go to stderr.
		if store.IsIOErr(err) {
			logStoreDiagnose(ctx, st, err)
		}
		return err
	}

	// Drive the verify and impact-sweep drains to fixpoint so a single
	// interrupted-then-rerun scan converges. run() already replayed the
	// pending_candidates WAL at start (stranded candidates verified), so this
	// reconciles the rest: VerifyDrain clears any pending this process left, and
	// SweepDrain re-ranks every open finding not yet swept — INCLUDING this
	// scan's, since the terminal Stage F no longer re-ranks inline. Bounded and
	// convergence-safe (VerifyDrain only deletes pending, SweepDrain only sets
	// swept_at, so work-remaining shrinks monotonically). Best-effort.
	reranked, drainErr := drainToFixpoint(ctx, f, st)

	// Stage F moved out of run(), so res.Findings carry PRE-sweep severities.
	// Refresh them from the re-ranked set so the oneshot summary matches the
	// store and any published issues (oracle #3).
	applyReranked(res.Findings, reranked)

	// Tear down the live pane before printing the final summary so the
	// summary is not interleaved with in-place repaints.
	stopPane()

	if drainErr != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(out, "\nWarning: post-scan drain incomplete (finding severities may be stale): %v\n", drainErr)
	}

	_ = flags.IncludeT3 // reserved: this stage emits T2 only; T3 filtering arrives with the report stage
	printResult(out, res)

	if flags.DoRepro && r != nil {
		// Wire spend to this scan run now that we have the ID. In-run hook
		// calls already used this recorder; setting the run ID here ensures
		// any catch-up drain spend is also attributed correctly.
		if reproRec != nil {
			reproRec.SetScanRun(res.ScanRunID)
		}
		// Catch-up drain: run a backlog-style drain over open T2 findings
		// from this scan that have no prior repro attempt. This is a cheap
		// no-op when the in-run hook covered everything; it ensures coverage
		// for findings that overflowed the reproCh buffer or were produced
		// by a very fast scan with a slow sandbox. Using the same
		// PromoteAll path here means the daemon drain's rotation logic (touch
		// failed findings) also runs for any catch-up attempts.
		if err := runReproCatchUp(ctx, out, r, st, res.Findings, reproAttempted); err != nil {
			return err
		}
	}

	// Exit nonzero when most finders failed to parse: automation must not
	// treat such a run as a clean pass. The summary (with its prominent
	// reliability warning) is already printed; we suppress cobra's usage and
	// error re-print so the warning stands as the explanation.
	if res.Stats.MostFindersFailed() {
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		return fmt.Errorf("scan unreliable: %d of %d finder agents produced no parseable output",
			res.Stats.FinderFailures, res.Stats.FinderRuns)
	}
	return nil
}

// openStoreForScan loads the config and opens the state store for the scan
// command. It is extracted so it can be called from runScanCmd without the
// cobra command being visible to the rest of the scan pipeline.
func openStoreForScan(ctx context.Context, cmd *cobra.Command) (config.Config, *store.Store, error) {
	return cmdOpenStore(ctx, configPathFromCmd(cmd))
}

// openRepoForScan opens the target repository for scanning. It wraps
// ingest.Open with a consistent error prefix so scan failures are identifiable.
func openRepoForScan(ctx context.Context, target string) (*ingest.Repo, error) {
	repo, err := ingest.Open(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("open target: %w", err)
	}
	return repo, nil
}

// buildReproHookForScan constructs the in-run reproducer hook when --repro is
// requested. It returns the hook closure and the associated reproducer state
// needed by the post-scan catch-up drain. When --repro is false or no container
// runtime is available the hook is nil and the other return values are zero.
func buildReproHookForScan(
	ctx context.Context,
	out io.Writer,
	cfg config.Config,
	st *store.Store,
	flags ScanFlags,
	prog progress.EventSink,
) (
	hook func(ctx context.Context, scanRunID string, finding store.Finding) error,
	rec *ledgerRecorder,
	r *repro.Reproducer,
	attempted *sync.Map,
	err error,
) {
	attempted = &sync.Map{}
	if !flags.DoRepro || flags.DoEstimate {
		return nil, nil, nil, attempted, nil
	}
	runtime, rtOK := sandbox.Detect()
	if !rtOK {
		_, _ = fmt.Fprintln(out, "Reproduce stage skipped: no container runtime (podman/docker) found on PATH.")
		// hook stays nil so the catch-up drain prints a note; DoRepro check in
		// the caller still runs (with r == nil) so no catch-up is attempted.
		return nil, nil, nil, attempted, nil
	}
	sb, sbErr := sandbox.NewCLI(runtime, cfg.Sandbox.Image, sandboxRunOpts(cfg)...)
	if sbErr != nil {
		return nil, nil, nil, nil, fmt.Errorf("build sandbox: %w", sbErr)
	}
	// Ledger repro + patch-prover spend; the scan run id is pinned by the
	// hook on first use (the funnel supplies it), and again after the sweep
	// for the catch-up drain.
	rec = newLedgerRecorder(ctx, st)
	reproClient, rErr := llm.ResolveRole(ctx, &cfg, "reproducer", llm.Options{Recorder: rec})
	if rErr != nil {
		return nil, nil, nil, nil, fmt.Errorf("build reproducer client: %w", rErr)
	}
	// Probe image capabilities once; result is cached per image so
	// subsequent daemon cycles and parallel scan runs are free.
	caps := sandbox.ProbeCapabilities(ctx, sb, cfg.Sandbox.Image, flags.Target)
	r, rNewErr := repro.New(reproClient, sb, flags.Target, repro.Options{
		Image:            cfg.Sandbox.Image,
		PatchProver:      cfg.Repro.PatchProver,
		PatchMaxAttempts: cfg.Repro.PatchMaxAttempts,
		PatchSuiteCmd:    cfg.Repro.SuiteCmd,
		DepStrategy:      sandbox.DepStrategy(cfg.Sandbox.DepStrategy),
		SetupCmds:        cfg.Sandbox.SetupCmds,
		LocalMounts:      localMountsFromConfig(cfg),
		Capabilities:     caps,
		Progress:         prog,
		StatusNotes:      cfg.Scan.StatusNotes,
		TranscriptDir:    cfg.Repro.TranscriptDir,
		PackageSummary:   packageSummaryProvider(st),
		Timeout:          time.Duration(cfg.Sandbox.TimeoutSeconds) * time.Second,
		SandboxMaxExecs:  cfg.Repro.SandboxMaxExecs,
	})
	if rNewErr != nil {
		return nil, nil, nil, nil, fmt.Errorf("build reproducer: %w", rNewErr)
	}
	if r == nil {
		return nil, nil, nil, attempted, nil
	}
	// Hook: called in-run for each Tier-2 finding. Uses PromoteOne
	// (one finding = one hook call = one idle slot; the funnel's
	// consumer goroutine is the parallelism bound). The hook calls
	// PromoteOne which calls Attempt internally.
	var runOnce sync.Once
	hook = func(hCtx context.Context, scanRunID string, finding store.Finding) error {
		runOnce.Do(func() { rec.SetScanRun(scanRunID) })
		attempted.Store(finding.Fingerprint, true)
		_, hErr := r.PromoteOne(hCtx, st, finding)
		return hErr
	}
	return hook, rec, r, attempted, nil
}

// executeScan runs the funnel: Targeted when changed files are provided,
// Sweep otherwise. It is the innermost independently-callable stage.
func executeScan(ctx context.Context, f *funnel.Funnel, kind store.ScanKind, changed []string) (*funnel.Result, error) {
	if kind == store.ScanTargeted {
		return f.Targeted(ctx, changed)
	}
	return f.Sweep(ctx)
}

// maxFixpointRounds bounds the oneshot drain loop so a pathological
// non-converging case (a finding the LLM repeatedly leaves ambiguous, so its
// swept_at never gets set) cannot spin. Round 1 does the work; a second confirms
// convergence; a third absorbs a verify-drain that surfaces a finding the same
// round's sweep then re-ranks.
const maxFixpointRounds = 3

// drainToFixpoint runs VerifyDrain then SweepDrain repeatedly until neither has
// work left (no pending candidates AND no unswept open findings) or the round
// cap is hit. It returns the union of the findings SweepDrain re-ranked, keyed
// by finding ID, so the caller can refresh the PRE-sweep severities its scan
// Result carries, plus the first drain/query error encountered (nil on clean
// convergence). The order matches the daemon: verify→sweep, so a candidate
// verified into a finding in a round is swept in the same round.
func drainToFixpoint(ctx context.Context, f *funnel.Funnel, st *store.Store) (map[string]store.Finding, error) {
	reranked := make(map[string]store.Finding)
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
func applyReranked(findings []store.Finding, reranked map[string]store.Finding) {
	for i := range findings {
		if rr, ok := reranked[findings[i].ID]; ok {
			findings[i].Severity = rr.Severity
			findings[i].VerdictDetail = rr.VerdictDetail
		}
	}
}

// checkScanLock queries the store for live (fresh-heartbeat, unfinished) scan
// runs. If any run belongs to a different pid, it returns an error naming the
// conflicting run and instructing the user to pass --force. Passing force=true
// skips the check entirely. Extracted as a standalone function for unit testing
// without driving the whole cobra command.
func checkScanLock(ctx context.Context, st *store.Store, force bool, selfPID int) error {
	if force {
		return nil
	}
	active, err := st.ActiveScanRuns(ctx, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("scan lock check: %w", err)
	}
	for _, r := range active {
		if r.PID != selfPID {
			return fmt.Errorf(
				"another scan is already running against this state db "+
					"(scan_run_id=%s pid=%d); wait for it to finish or pass --force to override",
				r.ID, r.PID,
			)
		}
	}
	return nil
}

// runHeartbeat periodically refreshes the heartbeat of the active scan run
// owned by selfPID. It resolves the run ID on the first tick by querying
// ActiveScanRuns for our own pid, then calls UpdateHeartbeat every ~30 s
// until the context is cancelled (i.e. until the scan finishes or is
// interrupted). The heartbeat interval matches the staleAfter window in
// checkScanLock (10 min) with comfortable margin (30 s); a missed heartbeat
// due to a slow tick does not expire within the window.
//
// This function is meant to be called as a goroutine.
func runHeartbeat(ctx context.Context, st *store.Store, selfPID int) {
	const interval = 30 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var runID string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Lazily resolve the run ID: the funnel's BeginScanRun is called
			// synchronously inside f.Sweep/f.Targeted before the first tick,
			// so by the time we reach here the row exists.
			if runID == "" {
				runs, err := st.ActiveScanRuns(ctx, 10*time.Minute)
				if err != nil || len(runs) == 0 {
					continue
				}
				for _, r := range runs {
					if r.PID == selfPID {
						runID = r.ID
						break
					}
				}
			}
			if runID == "" {
				continue
			}
			_ = st.UpdateHeartbeat(ctx, runID)
		}
	}
}

// buildSandboxOpts constructs a funnel.SandboxOpts from the config. When
// verify.sandbox_exec is false (the default) it returns a zero-value
// SandboxOpts (feature disabled). When the flag is enabled but no container
// runtime is available it also returns the zero value, with degraded=true so
// the caller can warn the user (the scan still runs, just without the
// empirical tool). An error is returned only when sandbox_exec is explicitly
// enabled and the sandbox backend cannot be constructed.
func buildSandboxOpts(cfg config.Config) (opts funnel.SandboxOpts, degraded bool, err error) {
	if !cfg.Verify.SandboxExec {
		return funnel.SandboxOpts{}, false, nil
	}
	runtime, ok := sandbox.Detect()
	if !ok {
		return funnel.SandboxOpts{}, true, nil
	}
	sb, err := sandbox.NewCLI(runtime, cfg.Sandbox.Image, sandboxRunOpts(cfg)...)
	if err != nil {
		return funnel.SandboxOpts{}, false, fmt.Errorf("build verify sandbox: %w", err)
	}
	return funnel.SandboxOpts{
		Sandbox:     sb,
		Enabled:     true,
		MinSeverity: cfg.Verify.SandboxMinSeverity,
		MaxExecs:    cfg.Verify.SandboxMaxExecs,
		DepStrategy: sandbox.DepStrategy(cfg.Sandbox.DepStrategy),
		SetupCmds:   cfg.Sandbox.SetupCmds,
		LocalMounts: localMountsFromConfig(cfg),
	}, false, nil
}

// sandboxRunOpts returns the sandbox.Option list every CLI sandbox in the app
// shares, derived from config. It enables the idle-timeout watchdog (dynamic,
// progress-based cancellation) when sandbox.idle_timeout_seconds > 0. Built once
// here so scan, verify, the analyzer seed, and the daemon stay consistent.
func sandboxRunOpts(cfg config.Config) []sandbox.Option {
	var opts []sandbox.Option
	if cfg.Sandbox.Network != "" {
		// Apply the operator's configured network as the sandbox DEFAULT for every
		// stage (probe, verify, repro, patch). A Spec that leaves Network unset
		// inherits this; stages no longer hardcode "none" and silently drop the
		// config (which broke CMake FetchContent builds under network=host).
		opts = append(opts, sandbox.WithNetwork(cfg.Sandbox.Network))
	}
	if cfg.Sandbox.IdleTimeoutSeconds > 0 {
		opts = append(opts, sandbox.WithIdleTimeout(time.Duration(cfg.Sandbox.IdleTimeoutSeconds)*time.Second))
	}
	if cfg.Sandbox.TimeoutSeconds > 0 {
		// Hard wall-clock ceiling for every sandbox run. Previously dropped: the
		// backend kept its 10-minute default and the reproducer forced 90s, so a
		// heavy build (vendored deps + engine + test) was killed long before it
		// could finish. A Spec's own Timeout still wins; repro sets it from this
		// same config value so both agree.
		opts = append(opts, sandbox.WithTimeout(time.Duration(cfg.Sandbox.TimeoutSeconds)*time.Second))
	}
	return opts
}

// packageSummaryProvider returns the lookup the reproducer uses to fetch a
// package's cached cartographer summary (store-backed). It powers the
// reproducer's task-prompt orientation and its get_package_context tool, so the
// agent reuses the finder's repo cartography instead of rediscovering the
// build/test layout from scratch. A miss (no cached row, or a query error)
// returns found=false and the reproducer degrades gracefully.
//
// Unlike the funnel's consumers (cartographer.go), this deliberately does NOT
// gate on the row's Fingerprint: the summary is orientation-only (the prompt
// tells the agent to "confirm specifics by reading files"), and within a scan
// the funnel has just refreshed summaries for the snapshot. A slightly stale
// summary at worst points the agent at the right package to read.
func packageSummaryProvider(st *store.Store) func(ctx context.Context, pkg string) (string, bool) {
	return func(ctx context.Context, pkg string) (string, bool) {
		sums, err := st.GetPackageSummaries(ctx, []string{pkg})
		if err != nil {
			return "", false
		}
		s, ok := sums[pkg]
		if !ok {
			return "", false
		}
		return s.Summary, true
	}
}

// localMountsFromConfig converts the operator's sandbox.local_mounts config
// entries into read-only sandbox.ROMounts. They are Shared=true (host-owned
// source trees that must NOT be SELinux :Z relabeled) per the local-mount
// contract; absolute-path, container-uniqueness, and existence checks already
// ran in config.Validate. Shared by the repro and funnel sandbox paths so both
// expose the same out-of-tree dependency directories offline.
func localMountsFromConfig(cfg config.Config) []sandbox.ROMount {
	if len(cfg.Sandbox.LocalMounts) == 0 {
		return nil
	}
	mounts := make([]sandbox.ROMount, len(cfg.Sandbox.LocalMounts))
	for i, m := range cfg.Sandbox.LocalMounts {
		mounts[i] = sandbox.ROMount{HostPath: m.Host, ContainerPath: m.Container, Shared: true}
	}
	return mounts
}

// runReproCatchUp runs a backlog-style drain over the run's Tier-2 findings
// that have no prior repro attempt (ReproPath empty, NeedsHuman false). This
// is a cheap no-op when the in-run hook covered everything; it acts as a safety
// net for findings that overflowed the reproCh buffer. It uses PromoteAll (the
// daemon's batch path) so the rotation logic (touch failed findings) also runs.
func runReproCatchUp(ctx context.Context, out io.Writer, r *repro.Reproducer, st *store.Store, findings []store.Finding, attempted *sync.Map) error {
	// Filter to T2 findings with no prior attempt. "Prior attempt" includes
	// in-run attempts that EXHAUSTED: a failed repro leaves no store-visible
	// marker (ReproPath stays empty, NeedsHuman stays false), so without the
	// attempted set this drain would re-burn sandbox time on exactly the
	// findings the in-run hook just failed on.
	var pending []store.Finding
	for _, f := range findings {
		if f.Tier != domain.TierVerified || f.ReproPath != "" || f.NeedsHuman {
			continue
		}
		if attempted != nil {
			if _, ok := attempted.Load(f.Fingerprint); ok {
				continue
			}
		}
		// Re-read from store to get the latest state (in-run hook may have promoted it).
		current, err := st.GetFinding(ctx, f.ID)
		if err != nil {
			continue // best-effort
		}
		if current.ReproPath == "" && !current.NeedsHuman {
			pending = append(pending, current)
		}
	}
	if len(pending) == 0 {
		return nil // no-op when in-run hook covered all findings
	}

	_, _ = fmt.Fprintf(out, "\nReproduce catch-up: %d finding(s) not yet attempted...\n", len(pending))
	summary, err := r.PromoteAll(ctx, st, pending)
	if err != nil {
		return fmt.Errorf("reproduce catch-up: %w", err)
	}
	printReproSummary(out, summary)
	return nil
}

// printReproSummary renders the promotion outcome.
func printReproSummary(out io.Writer, s *repro.Summary) {
	_, _ = fmt.Fprintf(out, "Reproduced: %d promoted to T1, %d not reproduced (of %d attempted)\n",
		s.Promoted, s.Failed, s.Attempted)
	if s.FixWitnessed > 0 || s.NeedsHuman > 0 {
		_, _ = fmt.Fprintf(out, "Patch-prover: %d fix-witnessed (T0), %d needs-human\n",
			s.FixWitnessed, s.NeedsHuman)
	}
	for _, o := range s.PerFinding {
		if o.FixWitnessed {
			_, _ = fmt.Fprintf(out, "  [T0] %s -> fix witnessed\n", o.Title)
		} else if o.Promoted {
			_, _ = fmt.Fprintf(out, "  [T1] %s -> %s\n", o.Title, o.ArtifactPath)
		} else {
			reason := o.Reason
			if reason == "" {
				reason = "not demonstrated"
			}
			_, _ = fmt.Fprintf(out, "  [T2] %s (%s)\n", o.Title, reason)
		}
		if o.NeedsHuman {
			_, _ = fmt.Fprintf(out, "       (patch-prover: needs human review)\n")
		}
	}
}

// printResult writes a human-readable summary of a funnel run: a findings table
// (tier, severity, file:line, title), per-stage counts, token spend, and any
// degradation/skip notes.
func printResult(out io.Writer, res *funnel.Result) {
	_, _ = fmt.Fprintf(out, "\nScan complete (commit %s)\n", util.ShortSHA(res.Commit))

	// Reliability gate: a scan where any finder produced no parseable output has
	// an untrustworthy result. "No findings" then means "we don't know", not
	// "clean" — so we must NEVER print a bare "No findings" in that case.
	reliable := res.Stats.FinderReliable()

	if len(res.Findings) == 0 {
		if reliable {
			_, _ = fmt.Fprintln(out, "\nNo findings.")
		} else {
			_, _ = fmt.Fprintln(out, "\nNo findings were RECOVERED — but this scan is NOT a clean bill of health (see warning below).")
		}
	} else {
		_, _ = fmt.Fprintf(out, "\n%d finding(s):\n\n", len(res.Findings))
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "TIER\tSEVERITY\tLOCATION\tTITLE")
		for _, fnd := range res.Findings {
			_, _ = fmt.Fprintf(tw, "T%d\t%s\t%s:%d\t%s\n",
				fnd.Tier, fnd.Severity, fnd.File, fnd.Line, fnd.Title)
		}
		_ = tw.Flush()
	}

	s := res.Stats
	_, _ = fmt.Fprintf(out, "\nStages: hypothesized=%d triaged=%d verified=%d killed=%d\n",
		s.Hypothesized, s.Triaged, s.Verified, s.Killed)
	if s.Resumed > 0 {
		_, _ = fmt.Fprintf(out, "Resumed: %d candidate(s) from a prior interrupted run replayed into triage/verify\n", s.Resumed)
	}
	_, _ = fmt.Fprintf(out, "Triage drops: low_confidence=%d duplicate=%d suppressed=%d out_of_scope=%d\n",
		s.DroppedLowConfidence, s.DroppedDuplicate, s.DroppedSuppressed, s.DroppedOutOfScope)
	if s.MergedWithinLens > 0 || s.MergedCrossLens > 0 {
		_, _ = fmt.Fprintf(out, "Location merges: within_lens=%d cross_lens=%d (collapsed to cluster primaries)\n",
			s.MergedWithinLens, s.MergedCrossLens)
	}
	_, _ = fmt.Fprintf(out, "Spend: input=%d output=%d total=%d tokens\n",
		s.InputTokens, s.OutputTokens, s.InputTokens+s.OutputTokens)
	if s.CacheReadTokens > 0 || s.CacheCreationTokens > 0 {
		_, _ = fmt.Fprintf(out, "Cache: read=%d created=%d tokens (of input; reads bill at a steep discount)\n",
			s.CacheReadTokens, s.CacheCreationTokens)
	}

	if s.FinderFailures > 0 || s.VerifierFailures > 0 {
		_, _ = fmt.Fprintf(out, "Agent failures: finders=%d/%d verifiers=%d/%d produced no parseable output\n",
			s.FinderFailures, s.FinderRuns, s.VerifierFailures, s.VerifierRuns)
	}
	if s.FinderRateLimited > 0 {
		_, _ = fmt.Fprintf(out, "Rate-limited finders: %d/%d (coverage incomplete; re-run at lower --concurrency)\n",
			s.FinderRateLimited, s.FinderRuns)
	}
	if s.SandboxExecs > 0 {
		_, _ = fmt.Fprintf(out, "Sandbox: execs=%d total_ms=%d\n", s.SandboxExecs, s.SandboxExecMillis)
	}
	if s.LeadsPosted > 0 || s.LeadsConsumed > 0 {
		_, _ = fmt.Fprintf(out, "Leads: posted=%d consumed=%d\n", s.LeadsPosted, s.LeadsConsumed)
	}

	if res.Degraded || res.Stopped {
		_, _ = fmt.Fprintf(out, "Budget: degraded=%v stopped=%v\n", res.Degraded, res.Stopped)
	}
	for _, note := range res.Skipped {
		_, _ = fmt.Fprintf(out, "  skipped: %s\n", note)
	}

	// A prominent, unmissable reliability warning when any finder failed to parse.
	// This is the trust fix: a silent "No findings" on a broken scan is worse than
	// a loud "we don't actually know".
	if !res.Stats.FinderReliable() {
		_, _ = fmt.Fprintf(out, "\n%s\n", reliabilityWarning(res.Stats))
	}
}

// printEstimate renders a pre-scan Estimate: the exact deterministic work
// breakdown followed by the projected token spend and wall time and the
// provenance of that projection. No LLM call was made to produce it.
func printEstimate(out io.Writer, e *funnel.Estimate) {
	_, _ = fmt.Fprintf(out, "\nScan estimate (%s, commit %s) — no LLM calls made\n", e.Kind, util.ShortSHA(e.Commit))

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "  in-scope files\t%d\n", e.Files)
	_, _ = fmt.Fprintf(tw, "  packages\t%d\n", e.Packages)
	_, _ = fmt.Fprintf(tw, "  finder chunks\t%d\n", e.Chunks)
	_, _ = fmt.Fprintf(tw, "  active lenses\t%d\n", e.Lenses)
	chunkUnits := e.FinderUnits - e.Seams
	if e.DiffIntent {
		chunkUnits--
	}
	extras := ""
	if e.DiffIntent || e.Seams > 0 {
		parts := []string{fmt.Sprintf("%d chunk", chunkUnits)}
		if e.DiffIntent {
			parts = append(parts, "1 diff-intent")
		}
		if e.Seams > 0 {
			parts = append(parts, fmt.Sprintf("%d seam", e.Seams))
		}
		extras = " (" + strings.Join(parts, " + ") + ")"
	}
	_, _ = fmt.Fprintf(tw, "  finder units\t%d%s\n", e.FinderUnits, extras)
	if e.CartographerEnabled {
		_, _ = fmt.Fprintf(tw, "  cartographer\t%d packages, %d need fresh summaries\n",
			e.CartographerPackages, e.CartographerUncached)
	}
	_ = tw.Flush()

	_, _ = fmt.Fprintf(out, "\nProjected spend: ~%s tokens (range %s–%s)\n",
		humanCount(e.EstTokens), humanCount(e.EstTokensLow), humanCount(e.EstTokensHigh))
	if e.ThroughputTokPerSec > 0 {
		_, _ = fmt.Fprintf(out, "Projected time:  ~%s (range %s–%s)\n",
			roundDuration(e.EstDuration), roundDuration(e.EstDurationLow), roundDuration(e.EstDurationHigh))
	} else {
		_, _ = fmt.Fprintln(out, "Projected time:  unknown (no throughput history yet — run one scan to calibrate)")
	}

	if e.Calibrated {
		scope := "all recent runs"
		if e.SampleMatched {
			scope = "matching-kind runs"
		}
		_, _ = fmt.Fprintf(out, "Basis: calibrated from %d %s (~%s tokens/finder-unit).\n",
			e.SampleRuns, scope, humanCount(int64(e.TokensPerUnit)))
	} else {
		_, _ = fmt.Fprintf(out, "Basis: built-in priors (~%s tokens/finder-unit) — no finished runs to calibrate from yet; the first real run will sharpen this.\n",
			humanCount(int64(e.TokensPerUnit)))
	}
	_, _ = fmt.Fprintln(out, "Note: finder-unit counts are exact; token/time figures are projections that also depend on findings volume (verification) and caching.")
}

// humanCount renders a token/count figure compactly: 1234567 -> "1.2M",
// 12345 -> "12k", small values verbatim.
func humanCount(n int64) string {
	switch {
	case n < 0:
		return "0"
	case n >= 999_500: // 999_500..999_999 would render "1000k"; promote to "1.0M"
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// roundDuration renders a duration at a human-friendly granularity for the
// estimate output. Sub-second values collapse to "<1s".
func roundDuration(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	if d >= time.Hour {
		return d.Round(time.Minute).String()
	}
	return d.Round(time.Second).String()
}

// reliabilityWarning renders the prominent banner shown when the finder stage was
// not fully reliable (no finders ran, or some produced no parseable output). It
// makes explicit that an empty/sparse finding set is untrustworthy.
func reliabilityWarning(s funnel.Stats) string {
	var b strings.Builder
	b.WriteString("!!! SCAN RELIABILITY WARNING !!!\n")
	switch {
	case s.FinderRuns == 0:
		b.WriteString("  No finder agents ran (the scan covered no files or all were skipped).\n")
		b.WriteString("  This result says NOTHING about the code's correctness.")
	case s.MostFindersFailed():
		fmt.Fprintf(&b, "  %d of %d finder agents produced NO parseable output.\n", s.FinderFailures, s.FinderRuns)
		b.WriteString("  Most lenses failed: this scan has effectively no signal. Treat any\n")
		b.WriteString("  'no findings' as UNKNOWN, not clean. Re-run, and check model/output-token settings.")
	default:
		fmt.Fprintf(&b, "  %d of %d finder agents produced NO parseable output.\n", s.FinderFailures, s.FinderRuns)
		b.WriteString("  Those lenses' findings (if any) were LOST. Coverage is incomplete —\n")
		b.WriteString("  do not read a low finding count as a clean bill of health.")
	}
	return b.String()
}

// buildScanChangeContext populates a funnel.ChangeContext for a --since targeted
// scan, enabling the diff-intent lens. Failures are best-effort and return nil
// so the scan proceeds without the extra context rather than aborting.
func buildScanChangeContext(ctx context.Context, repo *ingest.Repo, fromSHA, toSHA string, changed []string) *funnel.ChangeContext {
	if fromSHA == "" || toSHA == "" {
		return nil
	}
	cc := &funnel.ChangeContext{
		FromCommit:   fromSHA,
		ToCommit:     toSHA,
		ChangedFiles: changed,
		// BlastFiles intentionally omitted: derived inside hypothesize from the
		// blast-radius targets that Targeted already computed via BlastRadius.
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
func runAnalyzerSeed(ctx context.Context, cfg config.Config, repoDir string, st *store.Store, sink progress.EventSink) {
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
// runs. All failure modes degrade to a logged skip — it never returns an error
// and never blocks the scan.
func runContradictionSeed(ctx context.Context, cfg config.Config, repo *ingest.Repo, st *store.Store, sink progress.EventSink) {
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

// logStoreDiagnose is the IOERR-triage sink for scan errors. When the
// scan aborts with a SQLITE_IOERR-class error, we run the store's
// quick_check + reopen Diagnose and log the outcome to stderr so an
// operator can tell at a glance whether the failure was a transient
// VFS race (Diagnose clean) or a sign of on-disk corruption
// (Diagnose fails). The original scan error is the caller's
// responsibility to return — this function only emits diagnostics.
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

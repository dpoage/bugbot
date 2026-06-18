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
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/miner"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/repro"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

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
	var (
		target      string
		since       string
		includeT3   bool
		concurrency int
		refuters    int
		lenses      []string
		doRepro     bool
		doEstimate  bool
	)

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

			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}

			st, err := store.Open(ctx, cfg.Storage.Path)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer func() { _ = st.Close() }()

			repo, err := ingest.Open(ctx, target)
			if err != nil {
				return fmt.Errorf("open target: %w", err)
			}

			finder, err := llm.ResolveRole(ctx, &cfg, "finder", llm.Options{})
			if err != nil {
				return fmt.Errorf("build finder client: %w", err)
			}
			verifier, err := llm.ResolveRole(ctx, &cfg, "verifier", llm.Options{})
			if err != nil {
				return fmt.Errorf("build verifier client: %w", err)
			}
			// Cartographer client is built only when the package-summary pass is
			// enabled; otherwise it stays nil and the funnel reuses the finder.
			var cartographer llm.Client
			if cfg.Scan.Cartographer {
				cartographer, err = llm.ResolveRole(ctx, &cfg, "cartographer", llm.Options{})
				if err != nil {
					return fmt.Errorf("build cartographer client: %w", err)
				}
			}

			out := cmd.OutOrStdout()

			// Activity visibility: a snapshot sink (so `bugbot status` can read this
			// run from another terminal) plus a live renderer — an in-place ANSI pane
			// when stdout is a TTY, or plain log lines when piped. The pane is stopped
			// before the final summary so it leaves the terminal clean.
			snap := progress.NewSnapshotSink(storageDir(cfg))
			var (
				pane     *progress.PaneRenderer
				liveSink progress.Sink
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

			sandboxOpts, sandboxDegraded, sandboxErr := buildSandboxOpts(cfg)
			if sandboxErr != nil {
				return sandboxErr
			}
			if sandboxDegraded {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Warning: %s\n", sandboxDegradedWarning)
			}

			// Build the reproducer and wire it as an in-run hook when --repro is
			// set. The hook closure captures the reproducer and the spend recorder
			// so repro LLM spend flows through the same ledger as the funnel.
			// The funnel does NOT import internal/repro; the CLI owns this wiring.
			//
			// Spend attribution: the funnel passes the scan run id into the hook
			// (it is minted inside the funnel), and the closure pins it on the
			// recorder once before the first PromoteOne — so in-run repro spend is
			// attributed to the scan run like finder/verifier spend.
			//
			// reproAttempted records every fingerprint the in-run hook attempted —
			// INCLUDING exhausted attempts, which leave no store-visible marker —
			// so the catch-up drain never re-burns sandbox time on a finding this
			// run already tried (the daemon's next-cycle rotation is separate,
			// pre-existing behavior).
			var reproHook func(ctx context.Context, scanRunID string, finding store.Finding) error
			var reproAttempted sync.Map
			var reproRunOnce sync.Once
			var r *repro.Reproducer
			var reproRec *ledgerRecorder
			if doRepro && !doEstimate {
				runtime, rtOK := sandbox.Detect()
				if !rtOK {
					_, _ = fmt.Fprintln(out, "Reproduce stage skipped: no container runtime (podman/docker) found on PATH.")
					// doRepro stays true so the catch-up drain prints a note; hook stays nil.
				} else {
					sb, sbErr := sandbox.NewCLI(runtime, cfg.Sandbox.Image, sandboxRunOpts(cfg)...)
					if sbErr != nil {
						return fmt.Errorf("build sandbox: %w", sbErr)
					}
					// Ledger repro + patch-prover spend; the scan run id is pinned by
					// the hook on first use (the funnel supplies it), and again after
					// the sweep for the catch-up drain.
					reproRec = newLedgerRecorder(ctx, st)
					reproClient, rErr := llm.ResolveRole(ctx, &cfg, "reproducer", llm.Options{Recorder: reproRec})
					if rErr != nil {
						return fmt.Errorf("build reproducer client: %w", rErr)
					}
					var rNewErr error
					r, rNewErr = repro.New(reproClient, sb, target, repro.Options{
						Image:            cfg.Sandbox.Image,
						PatchProver:      cfg.Repro.PatchProver,
						PatchMaxAttempts: cfg.Repro.PatchMaxAttempts,
						PatchSuiteCmd:    cfg.Repro.SuiteCmd,
						DepStrategy:      sandbox.DepStrategy(cfg.Sandbox.DepStrategy),
					})
					if rNewErr != nil {
						return fmt.Errorf("build reproducer: %w", rNewErr)
					}
					if r != nil {
						// Hook: called in-run for each Tier-2 finding. Uses PromoteOne
						// (one finding = one hook call = one idle slot; the funnel's
						// consumer goroutine is the parallelism bound). The hook calls
						// PromoteOne which calls Attempt internally — no internal semaphore
						// is added here. Hook errors are surfaced but do not abort the scan.
						reproHook = func(hCtx context.Context, scanRunID string, finding store.Finding) error {
							reproRunOnce.Do(func() { reproRec.SetScanRun(scanRunID) })
							reproAttempted.Store(finding.Fingerprint, true)
							_, hErr := r.PromoteOne(hCtx, st, finding)
							return hErr
						}
					}
				}
			}

			opts := funnel.Options{
				Lenses:                lenses,
				Filter:                ingest.ScanFilter{Include: cfg.Scan.Include, Exclude: cfg.Scan.Exclude},
				Refuters:              refuters,
				MaxParallel:           concurrency,
				TokenBudget:           cfg.Budgets.PerCycleTokens,
				CacheReadBudgetWeight: cfg.Budgets.CacheReadWeight,
				FinderBudgetShare:     cfg.Budgets.FinderBudgetShare,
				FinderTokenClaim:      cfg.Budgets.FinderTokenClaim,
				VerifierTokenClaim:    cfg.Budgets.VerifierTokenClaim,
				FinderHistoryTokens:   cfg.Budgets.FinderHistoryTokens,
				FinderReadLines:       cfg.Budgets.FinderReadLines,
				FinderReadBytes:       cfg.Budgets.FinderReadBytes,
				Progress:              progressSink,
				SandboxOpts:           sandboxOpts,
				Repro:                 reproHook,
				Cartographer:          cfg.Scan.Cartographer,
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
			if since != "" {
				head, herr := repo.HeadCommit(ctx)
				if herr != nil {
					return fmt.Errorf("resolve HEAD: %w", herr)
				}
				changes, cerr := repo.ChangedFiles(ctx, since, head)
				if cerr != nil {
					return fmt.Errorf("diff %s..HEAD: %w", since, cerr)
				}
				changed = ingest.ChangedPaths(changes)
				_, _ = fmt.Fprintf(out, "Targeted scan: %d changed file(s) since %s\n", len(changed), since)
				// Populate ChangeContext for the diff-intent lens. Failures are
				// non-fatal: the scan still runs without diff-intent context.
				cc := buildScanChangeContext(ctx, repo, since, head, changed)
				if cc != nil {
					opts.ChangeContext = cc
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
			if doEstimate {
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

			var res *funnel.Result
			if kind == store.ScanTargeted {
				res, err = f.Targeted(ctx, changed)
			} else {
				res, err = f.Sweep(ctx)
			}
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

			// Tear down the live pane before printing the final summary so the
			// summary is not interleaved with in-place repaints.
			stopPane()

			_ = includeT3 // reserved: this stage emits T2 only; T3 filtering arrives with the report stage
			printResult(out, res)

			if doRepro && r != nil {
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
				if err := runReproCatchUp(ctx, out, r, st, res.Findings, &reproAttempted); err != nil {
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
		},
	}

	cmd.Flags().StringVar(&target, "target", ".", "path to the target repository")
	cmd.Flags().StringVar(&since, "since", "", "scan only the blast radius of changes since this commit (targeted scan)")
	cmd.Flags().BoolVar(&includeT3, "include-suspected", false, "include T3 (suspected) findings in output (reserved; this stage emits T2)")
	cmd.Flags().IntVar(&concurrency, "concurrency", funnel.DefaultMaxParallel, "number of parallel agents")
	cmd.Flags().IntVar(&refuters, "refuters", funnel.DefaultRefuters, "number of adversarial refuter agents per candidate")
	cmd.Flags().StringSliceVar(&lenses, "lens", nil, "restrict finder lenses (repeatable); default is all built-in lenses")
	cmd.Flags().BoolVar(&doRepro, "repro", false, "run the Reproduce stage: generate sandboxed failing tests and promote demonstrated findings to Tier-1")
	cmd.Flags().BoolVar(&doEstimate, "estimate", false, "estimate token spend and wall time for this scan without running it (no LLM calls)")

	return cmd
}

// sandboxDegradedWarning is printed when verify.sandbox_exec is enabled but no
// container runtime exists: the user asked for empirical refutation and must
// be told it was dropped, mirroring the repro stage's skip notice.
const sandboxDegradedWarning = "verify.sandbox_exec is enabled but no container runtime (podman/docker) was found on PATH; refuters will argue without sandbox execution"

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
	}, false, nil
}

// sandboxRunOpts returns the sandbox.Option list every CLI sandbox in the app
// shares, derived from config. It enables the idle-timeout watchdog (dynamic,
// progress-based cancellation) when sandbox.idle_timeout_seconds > 0. Built once
// here so scan, verify, the analyzer seed, and the daemon stay consistent.
func sandboxRunOpts(cfg config.Config) []sandbox.Option {
	var opts []sandbox.Option
	if cfg.Sandbox.IdleTimeoutSeconds > 0 {
		opts = append(opts, sandbox.WithIdleTimeout(time.Duration(cfg.Sandbox.IdleTimeoutSeconds)*time.Second))
	}
	return opts
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
		if f.Tier != 2 || f.ReproPath != "" || f.NeedsHuman {
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
	_, _ = fmt.Fprintf(out, "\nScan complete (commit %s)\n", shortSHA(res.Commit))

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
	_, _ = fmt.Fprintf(out, "\nScan estimate (%s, commit %s) — no LLM calls made\n", e.Kind, shortSHA(e.Commit))

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
	case n >= 1_000_000:
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

// shortSHA abbreviates a commit SHA for display, leaving short/empty values
// untouched.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// runAnalyzerSeed attempts to run the static-analyzer seeding step before the
// funnel. It detects the container runtime, builds a sandbox, and calls
// analyzer.Seed. All failure modes degrade to a logged skip — this function
// never returns an error and never blocks the scan.
//
// The seed step is always-on (no config knob in v1) but requires a container
// runtime: if no runtime is available the step is skipped silently.
func runAnalyzerSeed(ctx context.Context, cfg config.Config, repoDir string, st *store.Store, sink progress.Sink) {
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
func runContradictionSeed(ctx context.Context, cfg config.Config, repo *ingest.Repo, st *store.Store, sink progress.Sink) {
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

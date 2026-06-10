package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
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

			opts := funnel.Options{
				Lenses:      lenses,
				Filter:      ingest.ScanFilter{Include: cfg.Scan.Include, Exclude: cfg.Scan.Exclude},
				Refuters:    refuters,
				MaxParallel: concurrency,
				TokenBudget: cfg.Budgets.PerCycleTokens,
				Progress:    progressSink,
			}
			f, err := funnel.New(funnel.RoleClients{Finder: finder, Verifier: verifier}, st, repo, opts)
			if err != nil {
				return err
			}
			// Shut down any language servers the code-navigation tools spawned.
			defer func() { _ = f.Close() }()

			var res *funnel.Result
			if since != "" {
				head, herr := repo.HeadCommit(ctx)
				if herr != nil {
					return fmt.Errorf("resolve HEAD: %w", herr)
				}
				changes, cerr := repo.ChangedFiles(ctx, since, head)
				if cerr != nil {
					return fmt.Errorf("diff %s..HEAD: %w", since, cerr)
				}
				changed := ingest.ChangedPaths(changes)
				_, _ = fmt.Fprintf(out, "Targeted scan: %d changed file(s) since %s\n", len(changed), since)
				res, err = f.Targeted(ctx, changed)
			} else {
				res, err = f.Sweep(ctx)
			}
			if err != nil {
				return err
			}

			// Tear down the live pane before printing the final summary so the
			// summary is not interleaved with in-place repaints.
			stopPane()

			_ = includeT3 // reserved: this stage emits T2 only; T3 filtering arrives with the report stage
			printResult(out, res)

			if doRepro {
				if err := runRepro(ctx, out, &cfg, st, target, res.Findings); err != nil {
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

	return cmd
}

// runRepro runs the Reproduce stage over the run's findings (Tier-2 verified
// candidates), promoting any whose bug is demonstrated by a sandboxed failing
// test. It skips gracefully when no container runtime is available, and prints
// a promotion summary.
func runRepro(ctx context.Context, out io.Writer, cfg *config.Config, st *store.Store, target string, findings []store.Finding) error {
	runtime, ok := sandbox.Detect()
	if !ok {
		_, _ = fmt.Fprintln(out, "\nReproduce stage skipped: no container runtime (podman/docker) found on PATH.")
		return nil
	}

	t2 := make([]store.Finding, 0, len(findings))
	for _, f := range findings {
		if f.Tier == 2 {
			t2 = append(t2, f)
		}
	}
	if len(t2) == 0 {
		_, _ = fmt.Fprintln(out, "\nReproduce stage: no Tier-2 findings to reproduce.")
		return nil
	}

	sb, err := sandbox.NewCLI(runtime, cfg.Sandbox.Image)
	if err != nil {
		return fmt.Errorf("build sandbox: %w", err)
	}

	client, err := llm.ResolveRole(ctx, cfg, "reproducer", llm.Options{})
	if err != nil {
		return fmt.Errorf("build reproducer client: %w", err)
	}

	r, err := repro.New(client, sb, target, repro.Options{Image: cfg.Sandbox.Image})
	if err != nil {
		return fmt.Errorf("build reproducer: %w", err)
	}

	_, _ = fmt.Fprintf(out, "\nReproduce stage: attempting %d Tier-2 finding(s) (runtime=%s)...\n", len(t2), runtime)
	summary, err := r.PromoteAll(ctx, st, t2)
	if err != nil {
		return fmt.Errorf("reproduce: %w", err)
	}
	printReproSummary(out, summary)
	return nil
}

// printReproSummary renders the promotion outcome.
func printReproSummary(out io.Writer, s *repro.Summary) {
	_, _ = fmt.Fprintf(out, "Reproduced: %d promoted to T1, %d not reproduced (of %d attempted)\n",
		s.Promoted, s.Failed, s.Attempted)
	for _, o := range s.PerFinding {
		if o.Promoted {
			_, _ = fmt.Fprintf(out, "  [T1] %s -> %s\n", o.Title, o.ArtifactPath)
		} else {
			reason := o.Reason
			if reason == "" {
				reason = "not demonstrated"
			}
			_, _ = fmt.Fprintf(out, "  [T2] %s (%s)\n", o.Title, reason)
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

// shortSHA abbreviates a commit SHA for display, leaving short/empty values
// untouched.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

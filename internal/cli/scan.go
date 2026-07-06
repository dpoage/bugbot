package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/engine"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/util"
)

// ScanFlags holds the parsed flag values for `bugbot scan`. It mirrors the
// per-command overrides pattern used by engine.FunnelOptionOverrides so
// scan-specific configuration is grouped and independently testable.
type ScanFlags struct {
	Target string
	Since  string
	// From is the inclusive lower bound of a commit range scan (regress).
	// When set, the scan scopes its blast radius to the diff from..to and
	// labels each finding INTRODUCED vs PRE-EXISTING after the run. It is
	// mutually exclusive with --since at the CLI surface (only one is ever
	// set per command).
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
	cmd.Flags().IntVar(&flags.Concurrency, "concurrency", funnel.DefaultMaxParallel, "number of parallel agents")
	cmd.Flags().IntVar(&flags.Refuters, "refuters", funnel.DefaultRefuters, "number of adversarial refuter agents per candidate")
	cmd.Flags().StringSliceVar(&flags.Lenses, "lens", nil, "restrict finder lenses (repeatable); default is all built-in lenses")
	cmd.Flags().BoolVar(&flags.DoRepro, "repro", false, "run the Reproduce stage: generate sandboxed failing tests and promote demonstrated findings to Tier-1")
	cmd.Flags().BoolVar(&flags.DoEstimate, "estimate", false, "estimate token spend and wall time for this scan without running it (no LLM calls)")
	cmd.Flags().BoolVar(&flags.Force, "force", false, "bypass the advisory single-scan lock and proceed even if another scan appears active")

	return cmd
}

// runScanCmd loads config, opens an engine.Dispatcher, and delegates the scan
// pipeline to Dispatcher.Scan. It is extracted from the RunE closure so it
// stays independently callable and testable. The ctx passed in must already
// have signal cancellation wired by the caller.
func runScanCmd(ctx context.Context, cmd *cobra.Command, flags ScanFlags) error {
	cfg, err := config.Load(configPathFromCmd(cmd))
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	// Activity visibility: a snapshot sink (so `bugbot status` can read this
	// run from another terminal) plus a live renderer — an in-place ANSI pane
	// when stdout is a TTY, or plain log lines when piped. The pane is stopped
	// before the final summary so it leaves the terminal clean.
	snap := progress.NewSnapshotSink(storageDir(cfg))
	var (
		pane     *progress.PaneRenderer
		liveSink progress.EventSink
	)
	if progress.IsTerminal(errOut) {
		pane = progress.NewPaneRenderer(errOut, 0)
		liveSink = pane
	} else {
		liveSink = progress.NewLogRenderer(errOut)
	}
	progressSink := progress.NewMulti(liveSink, snap)
	stopPane := func() {
		if pane != nil {
			pane.Stop()
			pane = nil
		}
	}
	defer stopPane()

	d, err := engine.Open(ctx, cfg, progressSink)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	res, err := d.Scan(ctx, engine.ScanOpts{
		Target:       flags.Target,
		Since:        flags.Since,
		From:         flags.From,
		To:           flags.To,
		Concurrency:  flags.Concurrency,
		Refuters:     flags.Refuters,
		Lenses:       flags.Lenses,
		DoRepro:      flags.DoRepro,
		DoEstimate:   flags.DoEstimate,
		Force:        flags.Force,
		Out:          out,
		ErrOut:       errOut,
		StopProgress: stopPane,
	})
	if err != nil {
		return err
	}

	if res.Estimate != nil {
		printEstimate(out, res.Estimate)
		return nil
	}

	printResult(out, res.Result)

	if flags.From != "" {
		// Regress: label each finding INTRODUCED (anchor absent at --from) vs
		// PRE-EXISTING (anchor present at --from). Errors per anchor are
		// swallowed so a transient repo issue cannot abort the summary.
		printRegressAttribution(ctx, out, res.Repo, res.Result.Findings, flags.From)
	}

	// Exit nonzero when most finders failed to parse: automation must not
	// treat such a run as a clean pass. The summary (with its prominent
	// reliability warning) is already printed; we suppress cobra's usage and
	// error re-print so the warning stands as the explanation.
	if res.Result.Stats.MostFindersFailed() {
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		return newGateError(fmt.Sprintf("scan unreliable: %d of %d finder agents produced no parseable output",
			res.Result.Stats.FinderFailures, res.Result.Stats.FinderRuns))
	}
	return nil
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
	if s.MergedWithinLens > 0 || s.MergedCrossLens > 0 || s.MergedRootCause > 0 {
		_, _ = fmt.Fprintf(out, "Location merges: within_lens=%d cross_lens=%d root_cause=%d (collapsed to cluster primaries)\n",
			s.MergedWithinLens, s.MergedCrossLens, s.MergedRootCause)
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
	for _, ti := range s.ToolIssues {
		_, _ = fmt.Fprintf(out, "Tool health: %s reported %s x%d (%s) — results may be incomplete\n",
			ti.Tool, strings.ToUpper(ti.Severity), ti.Count, ti.Source)
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

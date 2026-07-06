package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/report"
	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/bugbot/internal/util"
)

// newReportCmd groups the report subcommands (list, show, dismiss, emit).
func newReportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Inspect, manage, and emit findings",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(
		newReportListCmd(),
		newReportShowCmd(),
		newReportDismissCmd(),
		newReportEmitCmd(),
		newReportUnitsCmd(),
	)

	return cmd
}

// newReportListCmd lists stored findings as a table (or JSON with --json).
func newReportListCmd() *cobra.Command {
	var (
		status string
		tier   int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list [flags]",
		Short: "List findings",
		Long: `List findings as a table.

--status filters by lifecycle state: open (default), dismissed, fixed, or all.
--tier filters by confidence tier (1, 2, or 3).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			_, st, err := cmdOpenStoreReadOnly(ctx, configPathFromCmd(cmd))
			if err != nil {
				return err
			}
			defer closeStore(st)

			filter := store.FindingFilter{HasTier: tier != 0, Tier: domain.Tier(tier)}
			switch strings.ToLower(strings.TrimSpace(status)) {
			case "", "open":
				filter.Status = store.StatusOpen
			case "all":
				// no status filter
			case "dismissed":
				filter.Status = store.StatusDismissed
			case "fixed":
				filter.Status = store.StatusFixed
			default:
				return fmt.Errorf("invalid --status %q (want open, dismissed, fixed, or all)", status)
			}

			findings, err := st.ListFindings(ctx, filter)
			if err != nil {
				return err
			}
			report.SortFindings(findings)

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(findings)
			}

			out := cmd.OutOrStdout()
			if len(findings) == 0 {
				_, _ = fmt.Fprintln(out, "no findings match")
				return nil
			}

			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ID\tTIER\tSEVERITY\tLOCATION\tSTATUS\tFLAGS\tTITLE")
			for _, f := range findings {
				flags := ""
				if f.ReproContradicted {
					flags = "repro-contradicted"
				}
				_, _ = fmt.Fprintf(tw, "%s\tT%d\t%s\t%s:%d\t%s\t%s\t%s\n",
					util.Truncate(f.ID, 12), f.Tier, f.Severity, f.File, f.Line, f.Status, flags, f.Title)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&status, "status", "open", "filter by status: open, dismissed, fixed, all")
	cmd.Flags().IntVar(&tier, "tier", 0, "filter by tier (1, 2, or 3; 0 means any — note: T0 fix-witnessed findings cannot be selected alone, they appear in the unfiltered list)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit findings as JSON")
	return cmd
}

// newReportShowCmd shows a single finding by id or unambiguous id prefix.
func newReportShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <id-or-prefix>",
		Short: "Show a single finding in full detail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			_, st, err := cmdOpenStoreReadOnly(ctx, configPathFromCmd(cmd))
			if err != nil {
				return err
			}
			defer closeStore(st)

			f, err := report.ResolveID(ctx, st, args[0])
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "ID:          %s\n", f.ID)
			_, _ = fmt.Fprintf(out, "Fingerprint: %s\n", f.Fingerprint)
			_, _ = fmt.Fprintf(out, "Title:       %s\n", f.Title)
			_, _ = fmt.Fprintf(out, "Tier:        %d\n", f.Tier)
			_, _ = fmt.Fprintf(out, "Severity:    %s\n", f.Severity)
			_, _ = fmt.Fprintf(out, "Status:      %s\n", f.Status)
			_, _ = fmt.Fprintf(out, "Lens:        %s\n", f.Lens)
			_, _ = fmt.Fprintf(out, "Location:    %s:%d\n", f.File, f.Line)
			_, _ = fmt.Fprintf(out, "Commit:      %s\n", f.CommitSHA)
			if f.ReproPath != "" {
				_, _ = fmt.Fprintf(out, "Repro:       %s\n", f.ReproPath)
			}
			if f.ReproContradicted {
				_, _ = fmt.Fprintf(out, "Repro-contradicted: yes (test ran >= %d times, bug did not manifest)\n", store.ReproContradictionThreshold)
			}
			_, _ = fmt.Fprintf(out, "Created:     %s\n", f.CreatedAt.Format("2006-01-02 15:04:05 MST"))
			_, _ = fmt.Fprintf(out, "Updated:     %s\n", f.UpdatedAt.Format("2006-01-02 15:04:05 MST"))
			_, _ = fmt.Fprintf(out, "\nDescription:\n%s\n", f.Description)
			_, _ = fmt.Fprintf(out, "\nReasoning (verification trace):\n%s\n", f.Reasoning)
			return nil
		},
	}
	return cmd
}

// newReportDismissCmd dismisses a finding, writing a persistent suppression so
// it is never re-reported.
func newReportDismissCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "dismiss <id-or-prefix>",
		Short: "Dismiss a finding and record a persistent suppression",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(reason) == "" {
				return fmt.Errorf("--reason is required: explain why this finding is being dismissed")
			}

			ctx := cmd.Context()
			// dismiss WRITES (AddSuppression inserts a suppression row and flips
			// the finding to dismissed inside a transaction), so it must take the
			// cross-process writer lock — unlike the read-only report subcommands.
			_, st, err := cmdOpenStore(ctx, configPathFromCmd(cmd))
			if err != nil {
				return err
			}
			defer closeStore(st)

			f, err := report.ResolveID(ctx, st, args[0])
			if err != nil {
				return err
			}

			// AddSuppression both records the fingerprint in the suppressions
			// table AND flips the finding to dismissed in one transaction. It is
			// the path that guarantees the suppression is written (UpsertFinding
			// then keeps the fingerprint dismissed forever), so it is preferred
			// over UpdateStatus here.
			if err := st.AddSuppression(ctx, f.Fingerprint, reason); err != nil {
				return err
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"dismissed %s; fingerprint suppressed — bugbot will not re-report this finding.\n",
				util.Truncate(f.ID, 12))
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why this finding is being dismissed (required)")
	return cmd
}

// newReportEmitCmd renders current open findings through the configured (or
// --sink overridden) sinks. This is the single reuse point the daemon and scan
// can call to emit a report.
func newReportEmitCmd() *cobra.Command {
	var sinkOverride []string
	cmd := &cobra.Command{
		Use:   "emit [flags]",
		Short: "Render current open findings through configured sinks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, st, err := cmdOpenStoreReadOnly(ctx, configPathFromCmd(cmd))
			if err != nil {
				return err
			}
			defer closeStore(st)

			if len(sinkOverride) > 0 {
				cfg.Report.Sinks = sinkOverride
			}

			sinks, err := report.SinksFromConfig(cfg)
			if err != nil {
				return err
			}
			// Route any stdout sink through the command's writer so output is
			// captured by tests and respects redirection.
			for _, s := range sinks {
				if ss, ok := s.(*report.StdoutSink); ok {
					ss.W = cmd.OutOrStdout()
				}
			}

			rep, err := report.CollectOpen(ctx, st, report.Metadata{
				GeneratedAt: timeNowUTC(),
			})
			if err != nil {
				return err
			}

			for _, sink := range sinks {
				if err := sink.Write(ctx, rep); err != nil {
					return fmt.Errorf("sink %s: %w", sink.Name(), err)
				}
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "emitted %d findings through %d sink(s)\n",
				len(rep.Findings), len(sinks))
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&sinkOverride, "sink", nil, "override configured sinks (fs, stdout); repeatable")
	return cmd
}

// timeNowUTC is the time source for emit, indirected so tests can pin it.
var timeNowUTC = func() time.Time { return time.Now().UTC() }

// newReportUnitsCmd renders the per-unit agent observability table for a given
// scan run. Each row represents one finder, verifier, or reproducer unit
// (including units skipped by the budget gate). The footer summarises totals,
// skipped-unit counts, and the cumulative coverage fraction.
func newReportUnitsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "units <scan-run-id>",
		Short: "Show per-unit agent observability for a scan run",
		Long: `Render a table of every agent unit launched (or skipped) in a scan run.

Each row is one finder, verifier, or reproducer execution. Skipped units
(budget gate fired before launch) have zero tokens and appear with a
skipped_* status. The footer shows coverage stats.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			_, st, err := cmdOpenStoreReadOnly(ctx, configPathFromCmd(cmd))
			if err != nil {
				return err
			}
			defer closeStore(st)

			scanRunID := args[0]
			// Allow a short prefix of the LATEST run id only (a convenience for "the
			// run that just finished"); prefixes of older runs are not resolved —
			// pass the full id for those.
			if len(scanRunID) < 32 {
				run, err := st.LatestScanRun(ctx)
				if err == nil && strings.HasPrefix(run.ID, scanRunID) {
					scanRunID = run.ID
				}
			}

			units, err := st.ListAgentUnits(ctx, scanRunID)
			if err != nil {
				return fmt.Errorf("list agent units: %w", err)
			}

			out := cmd.OutOrStdout()
			if len(units) == 0 {
				_, _ = fmt.Fprintln(out, "no agent unit rows found for this scan run (run may predate mi5.10 recording)")
				return nil
			}

			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ORDER\tROLE\tLENS@STRATEGY\tFILES\tSTATUS\tTOKENS\tCACHED\tCANDS\tDURATION")
			for _, u := range units {
				lensStrat := u.Lens
				if u.Strategy != "" && u.Strategy != "sweep-wide" {
					lensStrat = u.Lens + "@" + string(u.Strategy)
				}
				dur := "-"
				if !u.StartedAt.IsZero() && !u.FinishedAt.IsZero() {
					dur = u.FinishedAt.Sub(u.StartedAt).Round(time.Millisecond).String()
				}
				_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%d\t%d\t%d\t%s\n",
					u.LaunchOrder, u.Role, lensStrat, len(u.Files),
					u.Status, u.InputTokens+u.OutputTokens, u.CacheReadTokens,
					u.Candidates, dur)
			}
			_ = tw.Flush()

			// Footer: totals, skip counts, coverage fraction. Coverage is a
			// FINDER metric: the denominator is files targeted by finder units
			// only (verifier/reproducer rows carry the candidate's file, which
			// would inflate the denominator and make the fraction conflate two
			// different populations); the numerator is files some finderOK unit
			// actually covered.
			var totalUnits, skippedHard, skippedDeg, totalTokens int64
			coveredFiles := make(map[string]bool)
			finderFiles := make(map[string]bool)
			for _, u := range units {
				totalUnits++
				totalTokens += u.InputTokens + u.OutputTokens
				switch u.Status {
				case "skipped_hard_budget":
					skippedHard++
				case "skipped_degraded":
					skippedDeg++
				}
				if u.Role != "finder" {
					continue
				}
				for _, f := range u.Files {
					finderFiles[f] = true
					if u.Status == "ok" {
						coveredFiles[f] = true
					}
				}
			}
			_, _ = fmt.Fprintf(out, "\ntotal=%d skipped_hard=%d skipped_degraded=%d tokens=%d covered=%d/%d\n",
				totalUnits, skippedHard, skippedDeg, totalTokens, len(coveredFiles), len(finderFiles))
			return nil
		},
	}
	return cmd
}

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/report"
	"github.com/dpoage/bugbot/internal/store"
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
	)

	return cmd
}

// openStore loads config and opens the state store. Callers must Close it.
func openStore(ctx context.Context) (config.Config, *store.Store, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return config.Config{}, nil, err
	}
	st, err := store.Open(ctx, cfg.Storage.Path)
	if err != nil {
		return config.Config{}, nil, err
	}
	return cfg, st, nil
}

// shortID returns the first 12 hex chars of an id for compact table display.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
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
			_, st, err := openStore(ctx)
			if err != nil {
				return err
			}
			defer st.Close()

			filter := store.FindingFilter{Tier: tier}
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
				fmt.Fprintln(out, "no findings match")
				return nil
			}

			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTIER\tSEVERITY\tLOCATION\tSTATUS\tTITLE")
			for _, f := range findings {
				fmt.Fprintf(tw, "%s\tT%d\t%s\t%s:%d\t%s\t%s\n",
					shortID(f.ID), f.Tier, f.Severity, f.File, f.Line, f.Status, f.Title)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&status, "status", "open", "filter by status: open, dismissed, fixed, all")
	cmd.Flags().IntVar(&tier, "tier", 0, "filter by tier (1, 2, or 3; 0 means any)")
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
			_, st, err := openStore(ctx)
			if err != nil {
				return err
			}
			defer st.Close()

			f, err := report.ResolveID(ctx, st, args[0])
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "ID:          %s\n", f.ID)
			fmt.Fprintf(out, "Fingerprint: %s\n", f.Fingerprint)
			fmt.Fprintf(out, "Title:       %s\n", f.Title)
			fmt.Fprintf(out, "Tier:        %d\n", f.Tier)
			fmt.Fprintf(out, "Severity:    %s\n", f.Severity)
			fmt.Fprintf(out, "Status:      %s\n", f.Status)
			fmt.Fprintf(out, "Lens:        %s\n", f.Lens)
			fmt.Fprintf(out, "Location:    %s:%d\n", f.File, f.Line)
			fmt.Fprintf(out, "Commit:      %s\n", f.CommitSHA)
			if f.ReproPath != "" {
				fmt.Fprintf(out, "Repro:       %s\n", f.ReproPath)
			}
			fmt.Fprintf(out, "Created:     %s\n", f.CreatedAt.Format("2006-01-02 15:04:05 MST"))
			fmt.Fprintf(out, "Updated:     %s\n", f.UpdatedAt.Format("2006-01-02 15:04:05 MST"))
			fmt.Fprintf(out, "\nDescription:\n%s\n", f.Description)
			fmt.Fprintf(out, "\nReasoning (verification trace):\n%s\n", f.Reasoning)
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
			_, st, err := openStore(ctx)
			if err != nil {
				return err
			}
			defer st.Close()

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

			fmt.Fprintf(cmd.OutOrStdout(),
				"dismissed %s; fingerprint suppressed — bugbot will not re-report this finding.\n",
				shortID(f.ID))
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
			cfg, st, err := openStore(ctx)
			if err != nil {
				return err
			}
			defer st.Close()

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
			fmt.Fprintf(cmd.OutOrStdout(), "emitted %d findings through %d sink(s)\n",
				len(rep.Findings), len(sinks))
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&sinkOverride, "sink", nil, "override configured sinks (fs, stdout); repeatable")
	return cmd
}

// timeNowUTC is the time source for emit, indirected so tests can pin it.
var timeNowUTC = func() time.Time { return time.Now().UTC() }

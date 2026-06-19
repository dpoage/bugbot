package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// newCartographyCmd shows the cartographer's cached per-package summaries and,
// with --run, regenerates them out of band of a scan — the package-summary pass
// alone, no finders or verification. View mode is read-only; --run spends
// tokens on the cartographer model and ledgers them to a "cartography" scan run
// (visible in `bugbot metrics`). Summaries are cached by content fingerprint, so
// a --run only re-summarizes packages that changed since the last pass.
func newCartographyCmd() *cobra.Command {
	var (
		run    bool
		full   bool
		target string
	)
	cmd := &cobra.Command{
		Use:   "cartography",
		Short: "Show (or --run) the cartographer's cached package summaries",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			cfg, st, err := cmdOpenStore(ctx)
			if err != nil {
				return err
			}
			defer closeStore(st)
			out := cmd.OutOrStdout()

			if run {
				repo, err := ingest.Open(ctx, target)
				if err != nil {
					return fmt.Errorf("open target: %w", err)
				}
				client, err := llm.ResolveRole(ctx, &cfg, "cartographer", llm.Options{})
				if err != nil {
					return fmt.Errorf("build cartographer client: %w", err)
				}
				// funnel.New requires non-nil finder/verifier; a cartography-only
				// refresh never invokes them, so the single client fills all slots.
				f, err := funnel.New(funnel.RoleClients{Finder: client, Verifier: client, Cartographer: client}, st, repo, funnel.Options{
					Cartographer: true,
					Filter:       ingest.ScanFilter{Include: cfg.Scan.Include, Exclude: cfg.Scan.Exclude},
				})
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				rep, err := f.RefreshCartography(ctx, client)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "cartography refresh: %d packages — %d summarized, %d reused, %d failed (%d in / %d out tokens)\n\n",
					rep.Packages, rep.Summarized, rep.Reused, rep.Failed, rep.InputTokens, rep.OutputTokens)
			}

			summaries, err := st.ListPackageSummaries(ctx)
			if err != nil {
				return err
			}
			if len(summaries) == 0 {
				_, _ = fmt.Fprintln(out, "no package summaries stored yet (run a scan with cartographer enabled, or `bugbot cartography --run`)")
				return nil
			}
			printCartography(out, summaries, full)
			return nil
		},
	}
	cmd.Flags().BoolVar(&run, "run", false, "regenerate summaries out of band before showing them (spends tokens on the cartographer model)")
	cmd.Flags().BoolVar(&full, "full", false, "print each full summary instead of a one-line preview")
	cmd.Flags().StringVar(&target, "target", ".", "path to the target repository (used with --run)")
	return cmd
}

// printCartography renders the stored summaries: a one-line-per-package table by
// default, or the full text of each summary under --full.
func printCartography(out io.Writer, summaries []store.PackageSummary, full bool) {
	if full {
		for _, s := range summaries {
			_, _ = fmt.Fprintf(out, "=== %s  (fingerprint %s, updated %s) ===\n%s\n\n",
				s.Pkg, shortFingerprint(s.Fingerprint), s.UpdatedAt.Format("2006-01-02 15:04"), s.Summary)
		}
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PACKAGE\tUPDATED\tSUMMARY")
	for _, s := range summaries {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Pkg, s.UpdatedAt.Format("2006-01-02 15:04"), previewLine(s.Summary, 90))
	}
	_ = tw.Flush()
}

// previewLine collapses a summary's whitespace to a single line and truncates it
// to n runes for the table view.
func previewLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// shortFingerprint trims a content fingerprint to its first 12 hex chars for
// display.
func shortFingerprint(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

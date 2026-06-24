package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/store"
)

// newMetricsCmd reports valid-findings-per-token per scan run and a pooled
// cartographer on/off comparison. It is the read surface over the per-run
// counters the funnel persists in scan_runs.stats_json (Verified, token
// totals, cartographer_enabled), so the detection-efficiency trend is
// inspectable without hand-writing json_extract SQL. Purely informational:
// exits 0 even with no runs recorded.
func newMetricsCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Show valid-findings-per-token per scan run, with a cartographer on/off comparison",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			_, st, err := cmdOpenStoreReadOnly(ctx, configPathFromCmd(cmd))
			if err != nil {
				return err
			}
			defer closeStore(st)

			rows, err := st.RunMetrics(ctx, limit)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no finished scan runs recorded yet")
				return nil
			}
			printRunMetrics(out, rows)
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "max scan runs to include (0 = all)")
	return cmd
}

// printRunMetrics renders the per-run table followed by the pooled
// cartographer on/off comparison. The pooled rate (sum verified / sum tokens)
// is used rather than averaging per-run ratios: it weights runs by their token
// spend, so a handful of tiny runs can't dominate the headline efficiency
// number.
func printRunMetrics(out io.Writer, rows []store.RunMetric) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "STARTED\tKIND\tCARTO\tHYP\tVERIFIED\tKILLED\tTOKENS\tVERIFIED/1K")
	var onV, offV, onTok, offTok int64
	var onN, offN int
	for _, m := range rows {
		carto := "off"
		if m.CartographerEnabled {
			carto = "on"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%.3f\n",
			m.StartedAt.Format("2006-01-02 15:04"), m.Kind, carto,
			m.Hypothesized, m.Verified, m.Killed, m.TotalTokens(), m.VerifiedPer1K())
		if m.CartographerEnabled {
			onV += int64(m.Verified)
			onTok += m.TotalTokens()
			onN++
		} else {
			offV += int64(m.Verified)
			offTok += m.TotalTokens()
			offN++
		}
	}
	_ = tw.Flush()

	per1k := func(v, tok int64) float64 {
		if tok == 0 {
			return 0
		}
		return 1000 * float64(v) / float64(tok)
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "cartographer on/off (pooled verified findings per 1k tokens):")
	_, _ = fmt.Fprintf(out, "  on : %d runs, %d verified, %d tokens -> %.3f verified/1k\n", onN, onV, onTok, per1k(onV, onTok))
	_, _ = fmt.Fprintf(out, "  off: %d runs, %d verified, %d tokens -> %.3f verified/1k\n", offN, offV, offTok, per1k(offV, offTok))
}

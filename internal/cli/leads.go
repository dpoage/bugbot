package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/bugbot/internal/util"
)

// newLeadsCmd implements `bugbot leads`: the drill-down view of the cross-lens
// blackboard. Finders post out-of-lens suspicions here (post_lead tool); the
// target lens consumes them at the start of its next run. Consumed leads are
// deleted from the blackboard, so every row this command shows is still
// waiting for its target lens's next run.
func newLeadsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "leads",
		Short: "Show the cross-lens blackboard (tips finders posted for other lenses)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			_, st, err := cmdOpenStore(ctx, configPathFromCmd(cmd))
			if err != nil {
				return err
			}
			defer closeStore(st)

			leads, err := st.ListLeads(ctx)
			if err != nil {
				return fmt.Errorf("leads: %w", err)
			}
			renderLeads(cmd.OutOrStdout(), leads, time.Now())
			return nil
		},
	}
	return cmd
}

// renderLeads prints the blackboard newest-first. now is injectable for tests.
// Every lead in the slice is pending — consumed leads are deleted at claim
// time, so there is no consumed row to render.
func renderLeads(out io.Writer, leads []store.Lead, now time.Time) {
	if len(leads) == 0 {
		_, _ = fmt.Fprintln(out, "blackboard: empty (no pending cross-lens leads)")
		return
	}

	_, _ = fmt.Fprintf(out, "blackboard: %d pending lead(s)\n\n", len(leads))

	for _, l := range leads {
		_, _ = fmt.Fprintf(out, "  [PENDING] %s -> %s\n", l.PosterLens, l.TargetLens)
		_, _ = fmt.Fprintf(out, "    %s:%d — %s\n", l.File, l.Line, util.TruncateRunes(util.CollapseWhitespace(l.Note), 100))
		_, _ = fmt.Fprintf(out, "    posted %s\n", age(l.CreatedAt, now))
	}
}

// age renders a compact "Xm ago" / "Xh ago" / date for older entries.
func age(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("2006-01-02")
	}
}

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/store"
)

// newLeadsCmd implements `bugbot leads`: the drill-down view of the cross-lens
// blackboard. Finders post out-of-lens suspicions here (post_lead tool); the
// target lens consumes them at the start of its next run. This command is the
// only way to watch that conversation: pending rows are the tip queue for the
// next cycle, consumed rows are the history.
func newLeadsCmd() *cobra.Command {
	var pendingOnly bool

	cmd := &cobra.Command{
		Use:   "leads",
		Short: "Show the cross-lens blackboard (tips finders posted for other lenses)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			_, st, err := openStore(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			leads, err := st.ListLeads(ctx, pendingOnly)
			if err != nil {
				return fmt.Errorf("leads: %w", err)
			}
			renderLeads(cmd.OutOrStdout(), leads, pendingOnly, time.Now())
			return nil
		},
	}

	cmd.Flags().BoolVar(&pendingOnly, "pending", false, "show only unconsumed leads (the next cycle's tip queue)")
	return cmd
}

// renderLeads prints the blackboard newest-first. now is injectable for tests.
func renderLeads(out interface{ Write([]byte) (int, error) }, leads []store.Lead, pendingOnly bool, now time.Time) {
	if len(leads) == 0 {
		if pendingOnly {
			_, _ = fmt.Fprintln(out, "blackboard: no pending leads (nothing queued for the next cycle)")
		} else {
			_, _ = fmt.Fprintln(out, "blackboard: empty (no finder has posted a cross-lens lead yet)")
		}
		return
	}

	pending := 0
	for _, l := range leads {
		if l.Status == "posted" {
			pending++
		}
	}
	_, _ = fmt.Fprintf(out, "blackboard: %d lead(s), %d pending\n\n", len(leads), pending)

	for _, l := range leads {
		status := "consumed"
		when := l.ConsumedAt
		if l.Status == "posted" {
			status = "PENDING"
			when = l.CreatedAt
		}
		_, _ = fmt.Fprintf(out, "  [%s] %s -> %s\n", status, l.PosterLens, l.TargetLens)
		_, _ = fmt.Fprintf(out, "    %s:%d — %s\n", l.File, l.Line, truncateNote(l.Note, 100))
		_, _ = fmt.Fprintf(out, "    posted %s%s\n", age(l.CreatedAt, now), consumedNote(l.Status, when, now))
	}
}

func consumedNote(status string, consumedAt, now time.Time) string {
	if status == "posted" || consumedAt.IsZero() {
		return ""
	}
	return ", consumed " + age(consumedAt, now)
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

// truncateNote bounds a note for single-screen rendering.
func truncateNote(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

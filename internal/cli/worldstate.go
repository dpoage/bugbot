package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/store"
)

// worldState is the accumulated picture of a bugbot installation, as opposed
// to the live-activity snapshot: what stands found, what needs a human, what
// the blackboard holds for the next cycle, what is synced to GitHub, and what
// today cost. Every field is best-effort; render what was fetched.
type worldState struct {
	Tallies    store.FindingTallies
	HasTallies bool

	PendingLeads      []store.Lead // newest-first; render at most leadPreviewMax
	PendingLeadsTotal int

	Published map[string]int // state -> count; empty = never published

	DaySpend       store.SpendTotals
	HasDaySpend    bool
	DayBudgetLimit int64 // budgets.per_day_tokens (0 = unlimited)

	LastRun    store.ScanRun
	HasLastRun bool
}

// leadPreviewMax bounds the pending-lead preview in the status pane; the full
// list lives behind `bugbot leads`.
const leadPreviewMax = 3

// fetchWorldState gathers the world state from the store. It deliberately
// refuses to CREATE a store: a missing state DB means bugbot has never run
// here, and `bugbot status` must not leave a .bugbot directory behind as a
// side effect. The bool reports whether a store existed at all. Section
// failures degrade to their zero values — status is informational.
func fetchWorldState(ctx context.Context, cfg config.Config) (worldState, bool) {
	if _, err := os.Stat(cfg.Storage.Path); err != nil {
		return worldState{}, false
	}
	st, err := store.Open(ctx, cfg.Storage.Path)
	if err != nil {
		return worldState{}, false
	}
	defer closeStore(st)

	var ws worldState

	if t, err := st.CountFindings(ctx); err == nil {
		ws.Tallies = t
		ws.HasTallies = true
	}
	if leads, err := st.ListLeads(ctx); err == nil {
		ws.PendingLeadsTotal = len(leads)
		if len(leads) > leadPreviewMax {
			leads = leads[:leadPreviewMax]
		}
		ws.PendingLeads = leads
	}
	if pub, err := st.CountPublishedIssues(ctx); err == nil && len(pub) > 0 {
		ws.Published = pub
	}
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if totals, err := st.TotalsSince(ctx, midnight); err == nil {
		ws.DaySpend = totals
		ws.HasDaySpend = totals.InputTokens > 0 || totals.OutputTokens > 0
	}
	ws.DayBudgetLimit = cfg.Budgets.PerDayTokens
	if run, err := st.LatestScanRun(ctx); err == nil {
		ws.LastRun = run
		ws.HasLastRun = true
	}

	return ws, true
}

// renderWorldState prints the accumulated-state block of the status pane. now
// is injectable for tests.
func renderWorldState(out io.Writer, ws worldState, now time.Time) {
	_, _ = fmt.Fprintln(out, "\nWorld state:")

	if ws.HasTallies {
		_, _ = fmt.Fprintf(out, "  findings:     %s\n", findingsLine(ws.Tallies))
		if ws.Tallies.NeedsHuman > 0 {
			_, _ = fmt.Fprintf(out, "  needs human:  %d finding(s) the patch-prover could not fix — possibly misdiagnosed (bugbot report list)\n",
				ws.Tallies.NeedsHuman)
		}
	}

	if ws.PendingLeadsTotal == 0 {
		_, _ = fmt.Fprintln(out, "  blackboard:   empty (no pending cross-lens leads)")
	} else {
		_, _ = fmt.Fprintf(out, "  blackboard:   %d pending lead(s) for the next cycle (bugbot leads)\n", ws.PendingLeadsTotal)
		for _, l := range ws.PendingLeads {
			_, _ = fmt.Fprintf(out, "    -> %s: %s:%d — %s\n", l.TargetLens, l.File, l.Line, truncateNote(l.Note, 70))
		}
	}

	if len(ws.Published) > 0 {
		_, _ = fmt.Fprintf(out, "  github sync:  %s\n", publishedLine(ws.Published))
	}

	if ws.HasDaySpend {
		line := fmt.Sprintf("in=%d out=%d total=%d tokens",
			ws.DaySpend.InputTokens, ws.DaySpend.OutputTokens,
			ws.DaySpend.InputTokens+ws.DaySpend.OutputTokens)
		if ws.DayBudgetLimit > 0 {
			// One decimal so small spends don't render as a misleading 0%.
			pct := float64(ws.DaySpend.InputTokens+ws.DaySpend.OutputTokens) * 100 / float64(ws.DayBudgetLimit)
			line += fmt.Sprintf(" (%.1f%% of day budget)", pct)
		}
		_, _ = fmt.Fprintf(out, "  spend today:  %s\n", line)
	}

	if ws.HasLastRun {
		when := "running"
		if !ws.LastRun.FinishedAt.IsZero() {
			when = "finished " + age(ws.LastRun.FinishedAt, now)
		}
		_, _ = fmt.Fprintf(out, "  last run:     %s commit=%s %s\n",
			ws.LastRun.Kind, shortSHA(ws.LastRun.CommitSHA), when)
	}
}

// findingsLine renders "open: T0=1 T1=2 T2=3 | fixed=4 dismissed=1", omitting
// zero tiers and showing "none" for an empty open set.
func findingsLine(t store.FindingTallies) string {
	tiers := make([]int, 0, len(t.OpenByTier))
	for tier := range t.OpenByTier {
		tiers = append(tiers, tier)
	}
	sort.Ints(tiers)

	open := ""
	for _, tier := range tiers {
		if n := t.OpenByTier[tier]; n > 0 {
			open += fmt.Sprintf(" T%d=%d", tier, n)
		}
	}
	if open == "" {
		open = " none"
	}
	return fmt.Sprintf("open:%s | fixed=%d dismissed=%d", open, t.Fixed, t.Dismissed)
}

// publishedLine renders issue-sync counts in a stable order, omitting zeros.
func publishedLine(pub map[string]int) string {
	out := ""
	for _, state := range []string{"open", "closing", "pending", "closed"} {
		if n := pub[state]; n > 0 {
			if out != "" {
				out += " "
			}
			out += fmt.Sprintf("%s=%d", state, n)
		}
	}
	if out == "" {
		return "none"
	}
	return "issues " + out
}

package tui

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/store"
)

// fetchWorldState gathers WorldState from st. It mirrors
// internal/cli/worldstate.go's fetchWorldState (which is not importable from
// here; see the WorldState doc comment). Section failures degrade to their
// zero values — the caller renders whatever it got.
func fetchWorldState(ctx context.Context, st *store.Store, cfg config.Config) WorldState {
	var ws WorldState

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
	ws.CacheReadWeight = cfg.Budgets.CacheReadWeight
	if run, err := st.LatestScanRun(ctx); err == nil {
		ws.LastRun = run
		ws.HasLastRun = true
	}

	return ws
}

// storageDir returns the directory holding the state DB, which is also where
// the status.json snapshot lives (a sibling of state.db). Mirrors
// internal/cli/status.go's storageDir.
func storageDir(cfg config.Config) string {
	return filepath.Dir(cfg.Storage.Path)
}

// storeExists reports whether cfg's state DB file is present, without
// creating anything — a missing DB means bugbot has never run against this
// config, and the TUI must not leave a .bugbot directory behind as a side
// effect of merely being launched.
func storeExists(cfg config.Config) bool {
	_, err := os.Stat(cfg.Storage.Path)
	return err == nil
}

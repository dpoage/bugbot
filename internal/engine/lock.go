package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/dpoage/bugbot/internal/store"
)

// staleScanWindow is the heartbeat-freshness cutoff ActiveScanRuns uses to
// decide whether a scan_runs row still belongs to a live process. Shared by
// Open's Mode probe and checkScanLock so both agree on what "active" means.
const staleScanWindow = 10 * time.Minute

// checkScanLock queries the store for live (fresh-heartbeat, unfinished) scan
// runs. If any run belongs to a different pid, it returns an error naming the
// conflicting run and instructing the user to pass --force. Passing force=true
// skips the check entirely.
//
// Relocated verbatim from internal/cli (scan.go); ensureOwner is the only
// caller now, but the standalone signature is kept so it remains
// independently unit-testable without driving a whole Dispatcher.
func checkScanLock(ctx context.Context, st *store.Store, force bool, selfPID int) error {
	if force {
		return nil
	}
	active, err := st.ActiveScanRuns(ctx, staleScanWindow)
	if err != nil {
		return fmt.Errorf("scan lock check: %w", err)
	}
	for _, r := range active {
		if r.PID != selfPID {
			return fmt.Errorf(
				"another scan is already running against this state db "+
					"(scan_run_id=%s pid=%d); wait for it to finish or pass --force to override",
				r.ID, r.PID,
			)
		}
	}
	return nil
}

// runHeartbeat periodically refreshes the heartbeat of the active scan run
// owned by selfPID. It resolves the run ID on the first tick by querying
// ActiveScanRuns for our own pid, then calls UpdateHeartbeat every ~30s until
// the context is cancelled (i.e. until the scan finishes or is interrupted).
// The heartbeat interval matches staleScanWindow (10 min) with comfortable
// margin (30s); a missed heartbeat due to a slow tick does not expire within
// the window.
//
// This function is meant to be called as a goroutine. Relocated verbatim from
// internal/cli (scan.go); now started once per Owner-mode Dispatcher instead
// of being duplicated across scan/verify/sweep's RunE closures.
func runHeartbeat(ctx context.Context, st *store.Store, selfPID int) {
	const interval = 30 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var runID string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Lazily resolve the run ID: the funnel's BeginScanRun is called
			// synchronously inside f.Sweep/f.Targeted before the first tick,
			// so by the time we reach here the row exists.
			if runID == "" {
				runs, err := st.ActiveScanRuns(ctx, staleScanWindow)
				if err != nil || len(runs) == 0 {
					continue
				}
				for _, r := range runs {
					if r.PID == selfPID {
						runID = r.ID
						break
					}
				}
			}
			if runID == "" {
				continue
			}
			_ = st.UpdateHeartbeat(ctx, runID)
		}
	}
}

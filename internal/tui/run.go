package tui

import (
	"context"
	"errors"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/engine"
	"github.com/dpoage/bugbot/internal/store"
)

// Run auto-selects Owner or Observer mode for cfg and drives a full-screen
// bubbletea program until the user quits (q / ctrl-c). See selectFeed for
// the mode-selection contract.
func Run(ctx context.Context, cfg config.Config) error {
	feed, disp, cleanup, err := selectFeed(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	return runProgram(ctx, feed, disp)
}

// selectFeed picks the Feed to drive Model with and returns the Dispatcher
// backing it (nil outside Owner mode — see below) and a cleanup func the
// caller MUST run on quit (it releases whatever store handle(s) and
// Dispatcher the chosen mode acquired). Split out of Run so mode selection
// is exercised directly in tests without spinning up a real tea.Program.
//
// Mode selection mirrors every other Bugbot command's advisory-lock
// behavior (internal/engine.Open), gated by the same no-create-on-launch
// contract SnapshotFeed's storeExists check has always enforced: a missing
// state DB means bugbot has never run here, and merely launching the TUI
// must not scaffold a .bugbot directory or take the writer lock as a side
// effect.
//
//   - No store yet (storeExists is false): always Observer/SnapshotFeed,
//     regardless of lock availability — engine.Open is never called, so
//     nothing is created, and the returned Dispatcher is nil (dispatch is
//     disabled; a fresh repo's first dispatch will need to create the store
//     itself, which is out of scope for the four local dispatch verbs this
//     child implements).
//   - Owner (a store exists AND the writer lock is free, or already ours):
//     a LiveFeed is built and handed to engine.Open as its progress sink,
//     so the SAME Dispatcher that decided Owner mode is also the one whose
//     events the cockpit renders AND the one returned here for the dispatch
//     palette to call. The returned cleanup closes the LiveFeed's own store
//     handle AND the Dispatcher, releasing the writer lock promptly.
//   - Observer (a store exists but another process holds the lock and
//     looks actively alive): the Dispatcher is closed immediately
//     (Owner-only concerns like the heartbeat never started) and the
//     cockpit falls back to SnapshotFeed, the pre-existing read-only path.
//     The returned Dispatcher is nil; dispatch is disabled in this mode.
//   - ErrLocked (the writer lock is held but by an Owner cockpit sitting
//     idle with no active scan_runs row — see engine.Dispatcher's
//     heartbeat, which no-ops until a verb runs): engine.Open cannot open
//     the store writable, so this process falls back to SnapshotFeed
//     exactly as in the Observer case (Dispatcher nil), rather than
//     crashing.
func selectFeed(ctx context.Context, cfg config.Config) (Feed, *engine.Dispatcher, func(), error) {
	// A missing state DB means bugbot has never run here: never engage Owner
	// mode (which would create it via engine.Open's store.Open) just because
	// the operator glanced at the TUI. See worldstate.go's storeExists doc.
	if !storeExists(cfg) {
		feed, cleanup, err := newObserverFeed(ctx, cfg)
		return feed, nil, cleanup, err
	}

	liveFeed := NewLiveFeed(cfg)

	d, err := engine.Open(ctx, cfg, liveFeed)
	switch {
	case err == nil && d.Mode() == engine.Owner:
		if openErr := liveFeed.Open(ctx); openErr != nil {
			_ = d.Close()
			return nil, nil, nil, openErr
		}
		cleanup := func() {
			_ = liveFeed.Close()
			_ = d.Close()
		}
		return liveFeed, d, cleanup, nil

	case err == nil && d.Mode() == engine.Observer:
		_ = d.Close()
		feed, cleanup, ferr := newObserverFeed(ctx, cfg)
		return feed, nil, cleanup, ferr

	case isLocked(err):
		feed, cleanup, ferr := newObserverFeed(ctx, cfg)
		return feed, nil, cleanup, ferr

	default:
		return nil, nil, nil, err
	}
}

// newObserverFeed builds the read-only fallback feed, shared by the
// Observer and ErrLocked-fallback branches of selectFeed.
func newObserverFeed(ctx context.Context, cfg config.Config) (Feed, func(), error) {
	feed, err := NewSnapshotFeed(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return feed, func() { _ = feed.Close() }, nil
}

// isLocked reports whether err is (or wraps) *store.ErrLocked: another
// process holds the writer lock, e.g. an idle Owner cockpit whose heartbeat
// has not yet written a scan_runs row. Falling back to Observer here is the
// only choice that never crashes the TUI.
func isLocked(err error) bool {
	var locked *store.ErrLocked
	return errors.As(err, &locked)
}

// runProgram drives a full-screen bubbletea program over feed until the user
// quits. Shared by every mode: Model needs zero changes to render either
// feed's Frames. disp is nil outside Owner mode, disabling the dispatch
// palette.
func runProgram(ctx context.Context, feed Feed, disp *engine.Dispatcher) error {
	var d dispatcher
	if disp != nil {
		d = disp
	}
	p := tea.NewProgram(NewModel(ctx, feed, d), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

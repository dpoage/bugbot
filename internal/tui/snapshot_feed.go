package tui

import (
	"context"
	"os"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// snapshotInterval is how often SnapshotFeed re-reads status.json and the
// store, matching the cadence progress.SnapshotSink writes at.
const snapshotInterval = time.Second

// SnapshotFeed is the Observer-mode Feed: it owns a read-only store handle
// (opened once, never a writer) and, on each tick, re-reads status.json plus
// the world-state/agent_units helpers. It never opens the store for writing
// and never touches the network or an LLM.
type SnapshotFeed struct {
	st            *store.Store // nil when no state DB exists yet
	storageDir    string
	transcriptDir string
	cfg           config.Config

	interval time.Duration
	now      func() time.Time
}

// NewSnapshotFeed builds the Observer feed for cfg. It deliberately refuses
// to CREATE a store: a missing state DB means bugbot has never run here, and
// launching the TUI must not leave a .bugbot directory behind as a side
// effect (mirrors cli.fetchWorldState's contract). In that case st is nil and
// every Frame degrades to an empty/idle world-state.
func NewSnapshotFeed(ctx context.Context, cfg config.Config) (*SnapshotFeed, error) {
	f := &SnapshotFeed{
		storageDir:    storageDir(cfg),
		transcriptDir: cfg.Repro.TranscriptDir,
		cfg:           cfg,
		interval:      snapshotInterval,
		now:           time.Now,
	}
	if storeExists(cfg) {
		st, err := store.OpenReadOnly(ctx, cfg.Storage.Path)
		if err != nil {
			return nil, err
		}
		f.st = st
	}
	return f, nil
}

// Mode implements Feed.
func (f *SnapshotFeed) Mode() Mode { return Observer }

// Close implements Feed.
func (f *SnapshotFeed) Close() error {
	if f.st == nil {
		return nil
	}
	return f.st.Close()
}

// Next implements Feed: a ticker that resolves to a freshly built FrameMsg.
func (f *SnapshotFeed) Next() tea.Cmd {
	return tea.Tick(f.interval, func(time.Time) tea.Msg {
		return FrameMsg(f.buildFrame(context.Background()))
	})
}

// buildFrame gathers one Frame. Every section is best-effort: a read failure
// degrades that section to its zero value rather than failing the whole
// frame, since the TUI must always have something to render.
func (f *SnapshotFeed) buildFrame(ctx context.Context) Frame {
	var fr Frame

	path := progress.StatusPath(f.storageDir)
	if st, err := progress.ReadStatus(path); err == nil {
		fr.Snapshot = st
		fr.HasSnapshot = true
		fr.Stale = isStale(st, f.now())
	} else {
		// Missing or unparsable status.json: no live snapshot, render static.
		fr.Stale = true
	}

	if f.st == nil {
		return fr
	}

	fr.World = gatherWorldState(ctx, f.st, f.cfg)
	hist := gatherHistoricalAgents(ctx, f.st, fr.World)

	var live []progress.AgentStatus
	if fr.HasSnapshot && !fr.Stale {
		live = fr.Snapshot.ActiveAgents
	}
	fr.Agents = mergeAgents(live, hist, f.transcriptDir)

	return fr
}

// isStale reports whether a status snapshot looks dead: either its last
// update is older than staleAfter, or its writer process is gone. Mirrors
// internal/cli/status.go's isStale/processAlive exactly (package cli is not
// importable from here).
func isStale(st progress.Status, now time.Time) bool {
	if !st.LastUpdated.IsZero() && now.Sub(st.LastUpdated) > staleAfter {
		return true
	}
	return !processAlive(st.PID)
}

// processAlive reports whether a process with the given pid exists, via
// signal 0 (error-checks without delivering a signal). A pid of 0 or a
// not-found process reports false.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

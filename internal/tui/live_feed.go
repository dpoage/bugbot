package tui

import (
	"context"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// liveFeedInterval is the idle-ticker fallback cadence: even with no events
// flowing, Next() re-checks the store this often so world-state and
// agent_units (which change independently of progress events, e.g. a
// separate `bugbot review` call against the same store) eventually surface.
// Matches SnapshotFeed's cadence so the two modes feel identical.
const liveFeedInterval = time.Second

// LiveFeed is the Owner-mode Feed: it is BOTH a progress.EventSink (fed
// directly by the in-process engine.Dispatcher it shares a sink with) and a
// Feed (driving Model exactly like SnapshotFeed does). Handle folds every
// event into a progress.StatusAccumulator — the SAME fold status.json's
// SnapshotSink uses — so the cockpit and status.json can never disagree
// about what an event means. Next merges the folded Status with the store's
// world-state/agent_units, matching SnapshotFeed's buildFrame apart from the
// live-activity source (an in-memory accumulator here, status.json there).
//
// Handle must never block or fail: it takes a short mutex (inside acc) to
// fold the event, then sends a non-blocking, coalescing wakeup so Next's
// blocked goroutine rebuilds a Frame from the latest state. Dropping a
// redundant wakeup is safe — Next always rebuilds from whatever the
// accumulator currently holds — but dropping an EVENT (the fold itself)
// never happens: Apply always runs, unconditionally, before the wakeup send.
type LiveFeed struct {
	cfg           config.Config
	transcriptDir string

	acc  *progress.StatusAccumulator
	wake chan struct{} // 1-buffered; coalescing wakeup signal

	closeOnce sync.Once
	closed    chan struct{}

	stMu sync.Mutex
	st   *store.Store // nil until Open succeeds; guarded by stMu

	interval time.Duration // ticker fallback cadence, injectable for tests

	// actionMu guards actionState, which is updated in Handle (any goroutine)
	// and read in buildFrame (Next's goroutine).
	actionMu    sync.Mutex
	actionState ActionFeedState
}

// Compile-time assertions that LiveFeed satisfies both seams it bridges.
var _ Feed = (*LiveFeed)(nil)
var _ progress.EventSink = (*LiveFeed)(nil)

// NewLiveFeed builds the Owner-mode feed for cfg. It performs NO I/O: the
// read-only store handle is opened lazily by Open, called only once the
// caller has confirmed Owner mode (engine.Open already decided to hold the
// writer lock, so the store is guaranteed to exist by then) — constructing a
// LiveFeed to hand to engine.Open as its sink must not itself create a
// store, since engine.Open may end up choosing Observer mode or failing with
// ErrLocked instead.
func NewLiveFeed(cfg config.Config) *LiveFeed {
	return &LiveFeed{
		cfg:           cfg,
		transcriptDir: cfg.Repro.TranscriptDir,
		acc:           progress.NewStatusAccumulator(),
		wake:          make(chan struct{}, 1),
		closed:        make(chan struct{}),
		interval:      liveFeedInterval,
		actionState:   newActionFeedState(),
	}
}

// Open acquires LiveFeed's own read-only store handle. Safe alongside the
// engine's writable handle in the same process (WAL permits one writer and
// many readers); LiveFeed never reaches into the engine's Dispatcher/store.
// Must be called once, after the caller has confirmed Owner mode, before the
// feed is driven.
func (f *LiveFeed) Open(ctx context.Context) error {
	st, err := store.OpenReadOnly(ctx, f.cfg.Storage.Path)
	if err != nil {
		return err
	}
	f.stMu.Lock()
	f.st = st
	f.stMu.Unlock()
	return nil
}

// Handle implements progress.EventSink. It folds ev into the accumulator
// (never dropped, never blocking beyond the accumulator's short internal
// mutex) and fires a non-blocking, coalescing wakeup for Next.
func (f *LiveFeed) Handle(ev progress.Event) {
	f.acc.Apply(ev)
	switch ev.Kind {
	case progress.KindToolCall:
		f.actionMu.Lock()
		f.actionState.ApplyToolCallEvent(ev)
		f.actionMu.Unlock()
	case progress.KindAgentFinished:
		// Prune the finished agent's ring to prevent unbounded growth across
		// many scan runs, mirroring snapshot.go's delete(s.agents, key) on
		// KindAgentFinished.
		f.actionMu.Lock()
		f.actionState.PruneAgent(feedKeyForEvent(ev))
		f.actionMu.Unlock()
	}
	select {
	case f.wake <- struct{}{}:
	default:
		// A wakeup is already pending: Next will rebuild from the latest
		// folded Status when it fires, so this event is not lost — only the
		// redundant notification is coalesced away.
	}
}

// Mode implements Feed.
func (f *LiveFeed) Mode() Mode { return Owner }

// Close implements Feed: it unblocks any goroutine currently parked in a
// Next() cmd and releases the read-only store handle.
func (f *LiveFeed) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	f.stMu.Lock()
	st := f.st
	f.st = nil
	f.stMu.Unlock()
	if st != nil {
		return st.Close()
	}
	return nil
}

// Next implements Feed: a tea.Cmd that blocks off the Update thread until
// EITHER a wakeup fires (an event was folded) OR the idle ticker fires
// (~1s, so a store change with no accompanying event still surfaces), then
// builds and returns a fresh FrameMsg. A timer is created fresh per call
// (mirroring tea.Tick's own per-call pattern) rather than kept on the
// struct, so there is no persistent ticker goroutine to leak: Close simply
// closes f.closed to unblock whichever single call is currently waiting.
func (f *LiveFeed) Next() tea.Cmd {
	return func() tea.Msg {
		timer := time.NewTimer(f.interval)
		defer timer.Stop()
		select {
		case <-f.wake:
		case <-timer.C:
		case <-f.closed:
			return nil
		}
		return FrameMsg(f.buildFrame(context.Background()))
	}
}

// buildFrame gathers one Frame from the folded Status plus the store's
// world-state/agent_units, mirroring SnapshotFeed.buildFrame apart from the
// live-activity source. HasSnapshot is always true and Stale is always
// false: this process IS the live writer (Owner mode), so there is no
// separate-process staleness to detect the way SnapshotFeed must for a
// status.json it did not itself write. Live agents come from the folded
// Status unconditionally — unlike SnapshotFeed, which cannot have live
// agents without a store (status.json only exists once a store does),
// LiveFeed's fold is entirely in-memory, so it always has an answer even
// before Open() (or if Open ever failed) has a store handle; only the
// historical-merge half degrades to empty without one.
func (f *LiveFeed) buildFrame(ctx context.Context) Frame {
	var fr Frame

	fr.Snapshot = f.acc.Snapshot()
	fr.HasSnapshot = true
	fr.Stale = false

	// Hold stMu across the store reads themselves, not just the pointer
	// fetch: Close() takes the same lock to nil f.st and close the handle,
	// so this prevents a benign-but-avoidable use-after-close window where a
	// Next() cmd still mid-query races a concurrent Close() during quit.
	f.stMu.Lock()
	defer f.stMu.Unlock()
	st := f.st

	var hist []store.AgentUnit
	if st != nil {
		fr.World = gatherWorldState(ctx, st, f.cfg)
		hist = gatherHistoricalAgents(ctx, st, fr.World)
	}
	fr.Agents = mergeAgents(fr.Snapshot.ActiveAgents, hist, f.transcriptDir)

	// Snapshot the per-agent action rows (under actionMu, not stMu).
	f.actionMu.Lock()
	if len(f.actionState.perAgent) > 0 {
		fr.ActionRows = make(map[string][]ActionRow, len(f.actionState.perAgent))
		for k, ring := range f.actionState.perAgent {
			fr.ActionRows[k] = ring.Rows()
		}
	}
	f.actionMu.Unlock()

	return fr
}

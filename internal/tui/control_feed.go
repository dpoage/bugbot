package tui

import (
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/control"
	"github.com/dpoage/bugbot/internal/progress"
)

// controlFeedInterval is the idle-ticker fallback cadence, matching
// LiveFeed's and SnapshotFeed's so every mode feels identical even with no
// frames flowing (e.g. a connection hiccup).
const controlFeedInterval = time.Second

// ControlSocketFeed is the Attach-mode Feed (bugbot-2p8z.4): it connects to
// a separately-running daemon's control socket and drives Model from the
// wire event/status stream instead of an in-process progress.EventSink
// (LiveFeed) or status.json + store polling (SnapshotFeed).
//
// It folds the SAME event stream LiveFeed folds — just received over the
// wire instead of called directly — into its own progress.StatusAccumulator
// and ActionFeedState, so Attach mode's live-activity rendering (active
// agents, tool-call rows, spend) is identical to Owner mode's. WorldState
// (findings/leads/blackboard tallies, persisted store data) is NOT
// available over the socket in this slice — see bugbot-2p8z.4's --design
// notes — and degrades to its zero value exactly like SnapshotFeed does
// when it has no store handle.
type ControlSocketFeed struct {
	client *control.Client

	acc  *progress.StatusAccumulator
	wake chan struct{} // 1-buffered; coalescing wakeup signal

	closeOnce sync.Once
	closed    chan struct{}

	interval time.Duration // ticker fallback cadence, injectable for tests

	actionMu    sync.Mutex
	actionState ActionFeedState

	readDone chan struct{}
}

// Compile-time assertion that ControlSocketFeed satisfies Feed.
var _ Feed = (*ControlSocketFeed)(nil)

// NewControlSocketFeed wraps an already-dialed control.Client (see
// control.Dial) as a Feed. The caller owns dialing (so mode selection can
// confirm the socket is reachable before committing to Attach mode) but
// ControlSocketFeed owns the client's lifecycle from here — Close closes it.
func NewControlSocketFeed(client *control.Client) *ControlSocketFeed {
	f := &ControlSocketFeed{
		client:      client,
		acc:         progress.NewStatusAccumulator(),
		wake:        make(chan struct{}, 1),
		closed:      make(chan struct{}),
		interval:    controlFeedInterval,
		actionState: newActionFeedState(),
		readDone:    make(chan struct{}),
	}
	go f.readLoop()
	return f
}

// Mode implements Feed. Attach mode renders identically to Owner (dispatch
// -enabled UI); it is distinguished from Owner only by which Feed/dispatcher
// pair Run wires in, not by a different Mode constant here — Attach's
// distinguishing header text comes from mode reporting at the Run/model
// wiring layer (see run.go's selectFeed doc).
func (f *ControlSocketFeed) Mode() Mode { return Attach }

// Close implements Feed: unblocks any goroutine parked in Next() and closes
// the underlying control.Client connection.
func (f *ControlSocketFeed) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	err := f.client.Close()
	<-f.readDone
	return err
}

// Next implements Feed: blocks until a wire frame updates the fold, the
// idle ticker fires, or the feed is closed, then builds a fresh FrameMsg.
func (f *ControlSocketFeed) Next() tea.Cmd {
	return func() tea.Msg {
		timer := time.NewTimer(f.interval)
		defer timer.Stop()
		select {
		case <-f.wake:
		case <-timer.C:
		case <-f.closed:
			return nil
		}
		return FrameMsg(f.buildFrame())
	}
}

// readLoop drains the client's inbound frames, folding events into the
// accumulator/action state exactly like LiveFeed.Handle does, and firing
// the same non-blocking coalescing wakeup Next waits on. Exits when the
// feed closes OR the client's Frames channel closes (the connection died —
// daemon exited/crashed, or the peer otherwise dropped): in that case
// readLoop ALSO closes f.closed (via the same closeOnce Close() uses), so a
// naturally-dead connection unblocks any pending Next() exactly like an
// explicit Close() would, instead of leaving the cockpit spinning on its
// idle ticker forever with no indication the daemon is gone.
func (f *ControlSocketFeed) readLoop() {
	defer close(f.readDone)
	for {
		select {
		case fr, ok := <-f.client.Frames():
			if !ok {
				f.closeOnce.Do(func() { close(f.closed) })
				return
			}
			f.applyFrame(fr)
		case <-f.closed:
			return
		}
	}
}

func (f *ControlSocketFeed) applyFrame(fr control.Frame) {
	switch fr.Kind {
	case control.FrameKindEvent:
		if fr.Event == nil {
			return
		}
		ev := *fr.Event
		f.acc.Apply(ev)
		switch ev.Kind {
		case progress.KindToolCall:
			f.actionMu.Lock()
			f.actionState.ApplyToolCallEvent(ev)
			f.actionMu.Unlock()
		case progress.KindAgentFinished:
			f.actionMu.Lock()
			f.actionState.PruneAgent(feedKeyForEvent(ev))
			f.actionMu.Unlock()
		}
	case control.FrameKindStatus:
		// The status frame is a redundant, already-folded snapshot the
		// server sends alongside every event (and on its own idle ticker);
		// buildFrame always re-derives from f.acc rather than trusting a
		// server-computed Status directly, so there is nothing to apply
		// here beyond the wakeup below — this case exists so Next() also
		// wakes on a status-only frame (e.g. after reconnecting mid-cycle
		// with no new event yet).
	case control.FrameKindReply:
		// Dispatch replies are consumed by control.Client.Dispatch directly
		// and never reach Frames(); nothing to do here.
		return
	}
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// buildFrame gathers one Frame from the folded Status. World/Agents (the
// store-derived historical half) degrade to zero values — Attach mode has
// no store handle of its own, mirroring SnapshotFeed's no-store contract.
func (f *ControlSocketFeed) buildFrame() Frame {
	var fr Frame
	fr.Snapshot = f.acc.Snapshot()
	fr.HasSnapshot = true
	fr.Stale = false
	fr.Agents = mergeAgents(fr.Snapshot.ActiveAgents, nil, nil)

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

// dialControlSocket is a small indirection so tests can substitute a fake
// dialer; production always uses control.Dial.
var dialControlSocket = control.Dial

package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/progress"
)

// testLiveFeed builds a LiveFeed with no backing store (buildFrame degrades
// to the folded Status only, mirroring SnapshotFeed's no-store behavior) and
// a short idle-ticker interval so tests exercising the ticker path stay
// fast.
func testLiveFeed(interval time.Duration) *LiveFeed {
	f := NewLiveFeed(config.Default())
	f.interval = interval
	return f
}

// TestLiveFeed_HandleFoldsEventsIntoFrame drives Handle with a synthetic
// finder+verifier sequence and asserts the built Frame's live agents,
// activity, stage, and spend reflect the fold.
func TestLiveFeed_HandleFoldsEventsIntoFrame(t *testing.T) {
	f := testLiveFeed(time.Hour)
	now := time.Unix(1000, 0)

	f.Handle(progress.Event{Kind: progress.KindScanStarted, ScanKind: "sweep", Commit: "deadbeef", Time: now})
	f.Handle(progress.Event{Kind: progress.KindStageStarted, Stage: progress.StageHypothesize, Time: now})
	f.Handle(progress.Event{Kind: progress.KindAgentStarted, Role: progress.RoleFinder, Label: "lensA", Time: now})
	f.Handle(progress.Event{Kind: progress.KindAgentActivity, Role: progress.RoleFinder, Label: "lensA", Activity: "reading main.go", Time: now})
	f.Handle(progress.Event{Kind: progress.KindAgentStarted, Role: progress.RoleVerifier, Label: "candidate 1", Time: now})
	f.Handle(progress.Event{Kind: progress.KindAgentFinished, Role: progress.RoleFinder, Label: "lensA", Candidates: 2, Time: now})
	f.Handle(progress.Event{Kind: progress.KindStageFinished, Stage: progress.StageHypothesize, Counts: &progress.Counts{Hypothesized: 2}, Time: now})
	f.Handle(progress.Event{Kind: progress.KindStageStarted, Stage: progress.StageVerify, Time: now})
	f.Handle(progress.Event{Kind: progress.KindFindingVerified, File: "a.go", Title: "nil deref", Time: now})
	f.Handle(progress.Event{Kind: progress.KindFindingKilled, File: "b.go", Title: "false positive", Time: now})
	f.Handle(progress.Event{Kind: progress.KindSpendTick, InputTokens: 500, OutputTokens: 120, CacheReadTokens: 40, Time: now})

	fr := f.buildFrame(context.Background())

	if !fr.HasSnapshot || fr.Stale {
		t.Fatalf("frame HasSnapshot/Stale = %v/%v, want true/false (Owner mode is never stale)", fr.HasSnapshot, fr.Stale)
	}
	if fr.Snapshot.Stage != progress.StageVerify {
		t.Errorf("Stage = %q, want %q", fr.Snapshot.Stage, progress.StageVerify)
	}
	if fr.Snapshot.ScanKind != "sweep" || fr.Snapshot.Commit != "deadbeef" {
		t.Errorf("scan identity not folded: %+v", fr.Snapshot)
	}
	if fr.Snapshot.LiveVerified != 1 || fr.Snapshot.LiveKilled != 1 {
		t.Errorf("live verify/kill counters = %d/%d, want 1/1", fr.Snapshot.LiveVerified, fr.Snapshot.LiveKilled)
	}
	if fr.Snapshot.SpendInput != 500 || fr.Snapshot.SpendOutput != 120 || fr.Snapshot.SpendCacheRead != 40 {
		t.Errorf("spend not folded: %+v", fr.Snapshot)
	}
	// The finder finished (removed from the live map); only the verifier
	// remains active.
	if len(fr.Agents) != 1 {
		t.Fatalf("Agents = %+v, want exactly the still-running verifier", fr.Agents)
	}
	if fr.Agents[0].Role != progress.RoleVerifier || fr.Agents[0].Label != "candidate 1" || !fr.Agents[0].Live {
		t.Errorf("surviving agent = %+v, want live verifier/candidate 1", fr.Agents[0])
	}
}

// TestLiveFeed_HandleNeverBlocksAndCoalescesWakeups fires many Handle calls
// far faster than anything drains the wakeup channel, asserting each call
// returns promptly (never blocks) and that the final folded Status still
// reflects every started/finished event — dropping a wakeup notification is
// fine (Next always rebuilds from the latest fold), but dropping the FOLD
// itself would corrupt the live agent list.
func TestLiveFeed_HandleNeverBlocksAndCoalescesWakeups(t *testing.T) {
	f := testLiveFeed(time.Hour)
	now := time.Unix(2000, 0)

	const n = 50
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			label := "lens" + string(rune('A'+i%26))
			f.Handle(progress.Event{Kind: progress.KindAgentStarted, Role: progress.RoleFinder, Label: label, Time: now})
			f.Handle(progress.Event{Kind: progress.KindAgentActivity, Role: progress.RoleFinder, Label: label, Activity: "working", Time: now})
			f.Handle(progress.Event{Kind: progress.KindAgentFinished, Role: progress.RoleFinder, Label: label, Time: now})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Handle appears to have blocked: 150 calls did not complete in 5s")
	}

	// Every agent was started then finished, so none should remain live.
	snap := f.acc.Snapshot()
	if len(snap.ActiveAgents) != 0 {
		t.Fatalf("ActiveAgents = %+v, want empty (every agent finished)", snap.ActiveAgents)
	}
}

// TestLiveFeed_HandleConcurrentProducersNeverBlockOrLoseEvents is the
// multi-goroutine variant of the test above: the progress.EventSink
// contract explicitly requires safety against PARALLEL finder/verifier
// agents emitting from separate goroutines, not just a single fast
// producer. Each goroutine drives its own uniquely-labeled agent through
// start/activity/finish, so a correct implementation ends with an empty
// live-agent set regardless of how the accumulator's mutex and the wakeup
// channel interleave across goroutines. Run with -race to also catch any
// data race the mutex might have missed.
func TestLiveFeed_HandleConcurrentProducersNeverBlockOrLoseEvents(t *testing.T) {
	f := testLiveFeed(time.Hour)
	now := time.Unix(2500, 0)

	const goroutines = 8
	const perGoroutine = 25

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				label := fmt.Sprintf("g%d-lens%d", g, i)
				f.Handle(progress.Event{Kind: progress.KindAgentStarted, Role: progress.RoleFinder, Label: label, Time: now})
				f.Handle(progress.Event{Kind: progress.KindAgentActivity, Role: progress.RoleFinder, Label: label, Activity: "working", Time: now})
				f.Handle(progress.Event{Kind: progress.KindAgentFinished, Role: progress.RoleFinder, Label: label, Time: now})
			}
		}(g)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Handle appears to have blocked: %d concurrent producers did not finish in 5s", goroutines)
	}

	snap := f.acc.Snapshot()
	if len(snap.ActiveAgents) != 0 {
		t.Fatalf("ActiveAgents = %+v, want empty (every concurrently-started agent finished)", snap.ActiveAgents)
	}
}

// TestLiveFeed_NextResolvesOnWakeup verifies Next()'s cmd unblocks and
// returns a FrameMsg as soon as Handle folds an event, well before the idle
// ticker (set very long here) would fire.
func TestLiveFeed_NextResolvesOnWakeup(t *testing.T) {
	f := testLiveFeed(time.Hour)
	t.Cleanup(func() { _ = f.Close() })

	cmd := f.Next()
	type result struct {
		msg tea.Msg
	}
	resCh := make(chan result, 1)
	go func() { resCh <- result{msg: cmd()} }()

	// Give the goroutine a moment to reach the select before firing the
	// event that should wake it.
	time.Sleep(20 * time.Millisecond)
	f.Handle(progress.Event{Kind: progress.KindAgentStarted, Role: progress.RoleFinder, Label: "lensA", Time: time.Unix(3000, 0)})

	select {
	case r := <-resCh:
		fr, ok := r.msg.(FrameMsg)
		if !ok {
			t.Fatalf("Next() resolved to %T, want FrameMsg", r.msg)
		}
		if len(fr.Agents) != 1 || fr.Agents[0].Label != "lensA" {
			t.Fatalf("FrameMsg.Agents = %+v, want the just-started lensA", fr.Agents)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Next() did not resolve within 3s of a Handle wakeup")
	}
}

// TestLiveFeed_NextResolvesOnIdleTicker verifies Next()'s cmd resolves even
// with no events at all, once the idle ticker interval elapses.
func TestLiveFeed_NextResolvesOnIdleTicker(t *testing.T) {
	f := testLiveFeed(20 * time.Millisecond)
	t.Cleanup(func() { _ = f.Close() })

	cmd := f.Next()
	select {
	case msg := <-runAsync(cmd):
		if _, ok := msg.(FrameMsg); !ok {
			t.Fatalf("Next() resolved to %T, want FrameMsg", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next() did not resolve on the idle ticker within 2s")
	}
}

// TestLiveFeed_CloseUnblocksPendingNext verifies Close() unblocks a Next()
// cmd that is currently parked waiting for a wakeup or ticker, rather than
// leaking the goroutine for the full interval.
func TestLiveFeed_CloseUnblocksPendingNext(t *testing.T) {
	f := testLiveFeed(time.Hour)
	cmd := f.Next()
	ch := runAsync(cmd)

	time.Sleep(20 * time.Millisecond)
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	select {
	case msg := <-ch:
		if msg != nil {
			t.Fatalf("Next() resolved to %v after Close(), want nil", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not unblock a pending Next() within 2s")
	}
}

func runAsync(cmd tea.Cmd) <-chan tea.Msg {
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	return ch
}

// TestModel_RendersLiveFeedFrame drives NewModel with a real LiveFeed
// (folded via Handle, not a fakeFeed) and asserts the resulting Frame's live
// agent renders and drill-down works, exercising the same Model code path a
// real Owner-mode cockpit uses.
func TestModel_RendersLiveFeedFrame(t *testing.T) {
	f := testLiveFeed(time.Hour)
	t.Cleanup(func() { _ = f.Close() })

	now := time.Unix(4000, 0)
	f.Handle(progress.Event{Kind: progress.KindScanStarted, ScanKind: "sweep", Time: now})
	f.Handle(progress.Event{Kind: progress.KindAgentStarted, Role: progress.RoleVerifier, Label: "candidate A", Time: now})
	f.Handle(progress.Event{Kind: progress.KindAgentActivity, Role: progress.RoleVerifier, Label: "candidate A", Activity: "running sandbox", Time: now})

	fr := f.buildFrame(context.Background())

	m := NewModel(context.Background(), f, nil)
	m = sendFrame(m, fr)

	if len(m.frame.Agents) != 1 || !m.frame.Agents[0].Live {
		t.Fatalf("live agent not rendered into Model.frame: %+v", m.frame.Agents)
	}
	if m.frame.Agents[0].Activity != "running sandbox" {
		t.Fatalf("activity = %q, want %q", m.frame.Agents[0].Activity, "running sandbox")
	}

	m = sendKey(m, "tab") // -> Agents screen
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	_ = cmd
	if m.screen != screenAgentDetail {
		t.Fatalf("screen = %v after enter, want AgentDetail (drill-down failed)", m.screen)
	}

	view := stripANSI(m.View())
	if !strings.Contains(view, "candidate A") {
		t.Errorf("agent detail view missing drilled-in agent label, got:\n%s", view)
	}
}

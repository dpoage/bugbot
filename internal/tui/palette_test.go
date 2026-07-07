package tui

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/engine"
)

// fakeDispatcher is a test double for dispatcher: it records every call's
// Opts, returns canned results/errors, and — when block is non-nil — parks
// each call until either block is closed or ctx is cancelled, so tests can
// exercise the running/cancel states deterministically without a real
// funnel/LLM/sandbox stack.
type fakeDispatcher struct {
	mode engine.Mode

	mu          sync.Mutex
	scanCalls   []engine.ScanOpts
	verifyCalls []engine.VerifyOpts
	reproCalls  []engine.ReproOpts
	sweepCalls  []engine.SweepOpts

	scanResult   *engine.ScanResult
	verifyResult *engine.VerifyResult
	reproResult  *engine.ReproResult
	sweepResult  *engine.SweepResult
	err          error

	// block, when non-nil, makes every verb call park until it is closed or
	// ctx.Done() fires. sawCancel, when non-nil, is closed the moment a
	// parked call observes ctx.Done() — proof the dispatched verb's context
	// was actually cancelled, not just that the palette forgot about it.
	block     chan struct{}
	sawCancel chan struct{}
}

func (f *fakeDispatcher) Mode() engine.Mode { return f.mode }

func (f *fakeDispatcher) wait(ctx context.Context) error {
	if f.block == nil {
		return nil
	}
	select {
	case <-f.block:
		return nil
	case <-ctx.Done():
		if f.sawCancel != nil {
			close(f.sawCancel)
		}
		return ctx.Err()
	}
}

func (f *fakeDispatcher) Scan(ctx context.Context, opts engine.ScanOpts) (*engine.ScanResult, error) {
	f.mu.Lock()
	f.scanCalls = append(f.scanCalls, opts)
	f.mu.Unlock()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	return f.scanResult, f.err
}

func (f *fakeDispatcher) Verify(ctx context.Context, opts engine.VerifyOpts) (*engine.VerifyResult, error) {
	f.mu.Lock()
	f.verifyCalls = append(f.verifyCalls, opts)
	f.mu.Unlock()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	return f.verifyResult, f.err
}

func (f *fakeDispatcher) Repro(ctx context.Context, opts engine.ReproOpts) (*engine.ReproResult, error) {
	f.mu.Lock()
	f.reproCalls = append(f.reproCalls, opts)
	f.mu.Unlock()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	return f.reproResult, f.err
}

func (f *fakeDispatcher) Sweep(ctx context.Context, opts engine.SweepOpts) (*engine.SweepResult, error) {
	f.mu.Lock()
	f.sweepCalls = append(f.sweepCalls, opts)
	f.mu.Unlock()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	return f.sweepResult, f.err
}

func (f *fakeDispatcher) callCounts() (scan, verify, repro, sweep int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.scanCalls), len(f.verifyCalls), len(f.reproCalls), len(f.sweepCalls)
}

// typeString feeds each rune of s through sendKey, mirroring a user typing
// into whichever textinput currently has focus.
func typeString(m Model, s string) Model {
	for _, r := range s {
		// Update directly (not sendKey/runCmd): textinput.Update's returned
		// cmd is a cursor-blink tick that sendKey's synchronous runCmd would
		// otherwise actually sleep through per keystroke — irrelevant to
		// what these tests assert (the input's final Value()).
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(string(r))})
		m = next.(Model)
	}
	return m
}

func TestPalette_OpenAndClose(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, &fakeDispatcher{mode: engine.Owner})
	m = sendFrame(m, baseFrame())

	if m.palette.open {
		t.Fatal("palette should start closed")
	}
	m = sendKey(m, "d")
	if !m.palette.open {
		t.Fatal("expected palette open after 'd'")
	}
	m = sendKey(m, "esc")
	if m.palette.open {
		t.Fatal("expected palette closed after esc")
	}
}

func TestPalette_OpensFromEveryPane(t *testing.T) {
	for _, p := range []pane{paneRoster, paneDetail, paneContext} {
		m := NewModel(context.Background(), &fakeFeed{}, &fakeDispatcher{mode: engine.Owner})
		m = sendFrame(m, baseFrame())
		m.focus = p
		m = sendKey(m, "d")
		if !m.palette.open {
			t.Errorf("pane %v: expected palette to open on 'd'", p)
		}
	}
}

func TestPalette_GatingDisabledWhenDispatcherNil(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")

	m = sendKey(m, "enter") // confirm rowScanSweep (default cursor)
	if m.running {
		t.Fatal("confirm must be a no-op when Model.disp is nil")
	}
}

func TestPalette_ConfirmScanSweep(t *testing.T) {
	fd := &fakeDispatcher{mode: engine.Owner, scanResult: &engine.ScanResult{}}
	m := NewModel(context.Background(), &fakeFeed{}, fd)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if !m.running {
		t.Fatal("expected running=true after confirming Scan Sweep")
	}
	if m.palette.open {
		t.Fatal("expected palette to close once a dispatch starts")
	}
	if cmd == nil {
		t.Fatal("expected a dispatch tea.Cmd")
	}

	m = runCmd(m, cmd)
	if m.running {
		t.Fatal("expected running=false after dispatchDoneMsg")
	}
	if scan, verify, repro, sweep := fd.callCounts(); scan != 1 || verify != 0 || repro != 0 || sweep != 0 {
		t.Fatalf("call counts = (scan=%d verify=%d repro=%d sweep=%d), want (1,0,0,0)", scan, verify, repro, sweep)
	}
	if got := fd.scanCalls[0]; got.Since != "" {
		t.Errorf("Scan Sweep called with Since=%q, want empty (whole-snapshot)", got.Since)
	}
	if m.lastErr != nil {
		t.Errorf("lastErr = %v, want nil", m.lastErr)
	}
}

func TestPalette_ScanTargetedUsesSinceInput(t *testing.T) {
	fd := &fakeDispatcher{mode: engine.Owner, scanResult: &engine.ScanResult{}}
	m := NewModel(context.Background(), &fakeFeed{}, fd)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")
	m = sendKey(m, "j") // rowScanSweep -> rowScanTargeted

	m = sendKey(m, "enter") // focus the since input (editing)
	if !m.palette.editing {
		t.Fatal("expected editing=true after enter on Scan Targeted")
	}
	m = typeString(m, "HEAD~3")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // submit
	m = next.(Model)
	if !m.running {
		t.Fatal("expected running=true after submitting Scan Targeted")
	}
	m = runCmd(m, cmd)

	if len(fd.scanCalls) != 1 {
		t.Fatalf("Scan called %d times, want 1", len(fd.scanCalls))
	}
	if got := fd.scanCalls[0].Since; got != "HEAD~3" {
		t.Errorf("Scan Since = %q, want %q", got, "HEAD~3")
	}
}

func TestPalette_VerifySuspectedToggle(t *testing.T) {
	fd := &fakeDispatcher{mode: engine.Owner, verifyResult: &engine.VerifyResult{}}
	m := NewModel(context.Background(), &fakeFeed{}, fd)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")
	m = sendKey(m, "j") // -> rowScanTargeted
	m = sendKey(m, "j") // -> rowVerify

	m = sendKey(m, "s") // toggle suspected on
	if !m.palette.suspected {
		t.Fatal("expected suspected=true after 's'")
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	m = runCmd(m, cmd)

	if len(fd.verifyCalls) != 1 {
		t.Fatalf("Verify called %d times, want 1", len(fd.verifyCalls))
	}
	if !fd.verifyCalls[0].Suspected {
		t.Error("Verify called with Suspected=false, want true")
	}
}

func TestPalette_ReproUsesMaxInput(t *testing.T) {
	fd := &fakeDispatcher{mode: engine.Owner, reproResult: &engine.ReproResult{}}
	m := NewModel(context.Background(), &fakeFeed{}, fd)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")
	m = sendKey(m, "j") // -> rowScanTargeted
	m = sendKey(m, "j") // -> rowVerify
	m = sendKey(m, "j") // -> rowRepro

	m = sendKey(m, "enter") // focus maxN input
	if !m.palette.editing {
		t.Fatal("expected editing=true after enter on Repro")
	}
	m = typeString(m, "5")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	m = runCmd(m, cmd)

	if len(fd.reproCalls) != 1 {
		t.Fatalf("Repro called %d times, want 1", len(fd.reproCalls))
	}
	if got := fd.reproCalls[0].MaxN; got != 5 {
		t.Errorf("Repro MaxN = %d, want 5", got)
	}
}

func TestPalette_ConfirmSweep(t *testing.T) {
	fd := &fakeDispatcher{mode: engine.Owner, sweepResult: &engine.SweepResult{}}
	m := NewModel(context.Background(), &fakeFeed{}, fd)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")
	m = sendKey(m, "j") // rowScanTargeted
	m = sendKey(m, "j") // rowVerify
	m = sendKey(m, "j") // rowRepro
	m = sendKey(m, "j") // rowSweep

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	runCmd(next.(Model), cmd)

	if len(fd.sweepCalls) != 1 {
		t.Fatalf("Sweep called %d times, want 1", len(fd.sweepCalls))
	}
}

func TestPalette_OneAtATimeRefusesSecondDispatch(t *testing.T) {
	fd := &fakeDispatcher{mode: engine.Owner, block: make(chan struct{})}
	m := NewModel(context.Background(), &fakeFeed{}, fd)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // starts Scan Sweep, parks on fd.block
	m = next.(Model)
	if !m.running {
		t.Fatal("expected running=true")
	}
	if cmd == nil {
		t.Fatal("expected a dispatch cmd")
	}
	go func() { cmd() }() // let it park; result is drained by the deferred unblock below
	defer close(fd.block)

	// Re-open the palette and try to confirm another verb while the first
	// is still running: must be refused (no second call recorded).
	m = sendKey(m, "d")
	m = sendKey(m, "j") // rowScanTargeted
	m = sendKey(m, "enter")
	time.Sleep(20 * time.Millisecond) // let any (incorrect) second dispatch start

	if scan, verify, repro, sweep := fd.callCounts(); scan != 1 || verify != 0 || repro != 0 || sweep != 0 {
		t.Fatalf("call counts after refused second dispatch = (scan=%d verify=%d repro=%d sweep=%d), want (1,0,0,0)", scan, verify, repro, sweep)
	}
}

func TestPalette_CancelStopsContextAndClearsRunningOnDone(t *testing.T) {
	fd := &fakeDispatcher{mode: engine.Owner, block: make(chan struct{}), sawCancel: make(chan struct{})}
	m := NewModel(context.Background(), &fakeFeed{}, fd)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // starts Scan Sweep
	m = next.(Model)
	if !m.running {
		t.Fatal("expected running=true")
	}

	msgCh := make(chan tea.Msg, 1)
	go func() { msgCh <- cmd() }()

	// Give the goroutine a moment to reach fd.wait's select before cancelling.
	time.Sleep(10 * time.Millisecond)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	m = next.(Model)

	select {
	case <-fd.sawCancel:
	case <-time.After(2 * time.Second):
		t.Fatal("fake dispatcher never observed context cancellation after ctrl+x")
	}

	var msg tea.Msg
	select {
	case msg = <-msgCh:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch cmd never resolved after cancellation")
	}

	next, _ = m.Update(msg)
	m = next.(Model)
	if m.running {
		t.Fatal("expected running=false after dispatchDoneMsg following cancel")
	}
	if !isCancelled(m.lastErr) {
		t.Fatalf("lastErr = %v, want context.Canceled", m.lastErr)
	}
	if got := dispatchResultLine(m.lastVerb, m.lastErr, m.lastResult); got != m.lastVerb+": cancelled" {
		t.Errorf("dispatchResultLine = %q, want %q", got, m.lastVerb+": cancelled")
	}
}

func TestPalette_EscCancelsActiveRunInsteadOfClosingPalette(t *testing.T) {
	fd := &fakeDispatcher{mode: engine.Owner, block: make(chan struct{}), sawCancel: make(chan struct{})}
	m := NewModel(context.Background(), &fakeFeed{}, fd)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	go func() { cmd() }()
	defer close(fd.block)

	time.Sleep(10 * time.Millisecond)
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	select {
	case <-fd.sawCancel:
	case <-time.After(2 * time.Second):
		t.Fatal("esc while running did not cancel the dispatch context")
	}
}

func TestUpdate_DispatchDoneMsg_UpdatesResultLine(t *testing.T) {
	cases := []struct {
		name string
		msg  dispatchDoneMsg
		want string
	}{
		{"success", dispatchDoneMsg{verb: "sweep", summary: "sweep complete: 2 finding(s)"}, "sweep: sweep complete: 2 finding(s)"},
		{"error", dispatchDoneMsg{verb: "verify", err: errors.New("boom")}, "verify: error: boom"},
		{"cancelled", dispatchDoneMsg{verb: "scan (sweep)", err: context.Canceled}, "scan (sweep): cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewModel(context.Background(), &fakeFeed{}, &fakeDispatcher{mode: engine.Owner})
			m = sendFrame(m, baseFrame())
			m.running = true // dispatchDoneMsg must clear this regardless of prior state

			next, _ := m.Update(tc.msg)
			m = next.(Model)

			if m.running {
				t.Fatal("expected running=false after dispatchDoneMsg")
			}
			if got := dispatchResultLine(m.lastVerb, m.lastErr, m.lastResult); got != tc.want {
				t.Errorf("dispatchResultLine = %q, want %q", got, tc.want)
			}
		})
	}
}

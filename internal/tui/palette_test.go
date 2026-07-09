package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/engine"
	"github.com/dpoage/bugbot/internal/funnel"
)

// fakeDispatcher is a test double for dispatcher: it records every call's
// Opts, returns canned results/errors, and — when block is non-nil — parks
// each call until either block is closed or ctx is cancelled, so tests can
// exercise the running/cancel states deterministically without a real
// funnel/LLM/sandbox stack.
type fakeDispatcher struct {
	mode engine.Mode

	mu             sync.Mutex
	scanCalls      []engine.ScanOpts
	verifyCalls    []engine.VerifyOpts
	reproCalls     []engine.ReproOpts
	sweepCalls     []engine.SweepOpts
	reconcileCalls []engine.ReconcileOpts
	reviewCalls    []engine.ReviewPROpts

	scanResult      *engine.ScanResult
	verifyResult    *engine.VerifyResult
	reproResult     *engine.ReproResult
	sweepResult     *engine.SweepResult
	reconcileResult *engine.ReconcileResult
	reviewResult    *engine.ReviewPRResult
	err             error

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

func (f *fakeDispatcher) Reconcile(ctx context.Context, opts engine.ReconcileOpts) (*engine.ReconcileResult, error) {
	f.mu.Lock()
	f.reconcileCalls = append(f.reconcileCalls, opts)
	f.mu.Unlock()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	return f.reconcileResult, f.err
}

func (f *fakeDispatcher) ReviewPR(ctx context.Context, opts engine.ReviewPROpts) (*engine.ReviewPRResult, error) {
	f.mu.Lock()
	f.reviewCalls = append(f.reviewCalls, opts)
	f.mu.Unlock()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	return f.reviewResult, f.err
}

func (f *fakeDispatcher) callCounts() (scan, verify, repro, sweep, reconcile int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.scanCalls), len(f.verifyCalls), len(f.reproCalls), len(f.sweepCalls), len(f.reconcileCalls)
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
	if scan, verify, repro, sweep, reconcile := fd.callCounts(); scan != 1 || verify != 0 || repro != 0 || sweep != 0 || reconcile != 0 {
		t.Fatalf("call counts = (scan=%d verify=%d repro=%d sweep=%d reconcile=%d), want (1,0,0,0,0)", scan, verify, repro, sweep, reconcile)
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

// TestPalette_ConfirmReconcile confirms the reconcile row (bugbot-7bjl)
// dispatches through the Reconcile method and the resulting summary line
// renders reconcileSummary's text, mirroring TestPalette_ConfirmSweep.
func TestPalette_ConfirmReconcile(t *testing.T) {
	fd := &fakeDispatcher{mode: engine.Owner, reconcileResult: &engine.ReconcileResult{
		Result: &funnel.Result{Stats: funnel.Stats{
			ReconcileNominated:  3,
			ReconcileArbitrated: 3,
			ReconcileMerged:     1,
			ReconcileSkippedCap: 0,
		}},
	}}
	m := NewModel(context.Background(), &fakeFeed{}, fd)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")
	m = sendKey(m, "j") // rowScanTargeted
	m = sendKey(m, "j") // rowVerify
	m = sendKey(m, "j") // rowRepro
	m = sendKey(m, "j") // rowSweep
	m = sendKey(m, "j") // rowReconcile

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(next.(Model), cmd)

	if len(fd.reconcileCalls) != 1 {
		t.Fatalf("Reconcile called %d times, want 1", len(fd.reconcileCalls))
	}
	wantSummary := "reconcile complete: 3 nominated, 3 arbitrated, 1 merged, 0 skipped (cap)"
	if got := dispatchResultLine(m.lastVerb, m.lastErr, m.lastResult); got != "reconcile: "+wantSummary {
		t.Errorf("dispatchResultLine = %q, want %q", got, "reconcile: "+wantSummary)
	}
}

// TestPalette_ReviewUsesPRInput confirms the review row's PR-number textinput
// feeds ReviewPROpts.PRNumber through to the dispatcher, mirroring Repro's
// --max input row.
func TestPalette_ReviewUsesPRInput(t *testing.T) {
	fd := &fakeDispatcher{mode: engine.Owner, reviewResult: &engine.ReviewPRResult{}}
	m := NewModel(context.Background(), &fakeFeed{}, fd)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")
	m = sendKey(m, "j") // -> rowScanTargeted
	m = sendKey(m, "j") // -> rowVerify
	m = sendKey(m, "j") // -> rowRepro
	m = sendKey(m, "j") // -> rowSweep
	m = sendKey(m, "j") // -> rowReconcile
	m = sendKey(m, "j") // -> rowReview

	m = sendKey(m, "enter") // focus prNumber input
	if !m.palette.editing {
		t.Fatal("expected editing=true after enter on Review")
	}
	m = typeString(m, "42")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	m = runCmd(m, cmd)

	if len(fd.reviewCalls) != 1 {
		t.Fatalf("ReviewPR called %d times, want 1", len(fd.reviewCalls))
	}
	if got := fd.reviewCalls[0].PRNumber; got != 42 {
		t.Errorf("ReviewPR PRNumber = %d, want 42", got)
	}
}

// TestPalette_ReviewRejectsInvalidPRNumber confirms an empty/non-positive PR
// number never reaches the dispatcher and instead surfaces a clear
// dispatchDoneMsg error — the HEAD-mismatch-adjacent "never crash, always a
// status-line error" contract applies to malformed input too.
func TestPalette_ReviewRejectsInvalidPRNumber(t *testing.T) {
	fd := &fakeDispatcher{mode: engine.Owner, reviewResult: &engine.ReviewPRResult{}}
	m := NewModel(context.Background(), &fakeFeed{}, fd)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")
	m = sendKey(m, "j") // -> rowScanTargeted
	m = sendKey(m, "j") // -> rowVerify
	m = sendKey(m, "j") // -> rowRepro
	m = sendKey(m, "j") // -> rowSweep
	m = sendKey(m, "j") // -> rowReconcile
	m = sendKey(m, "j") // -> rowReview

	m = sendKey(m, "enter") // focus prNumber input, leave it empty
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	m = runCmd(m, cmd)

	if len(fd.reviewCalls) != 0 {
		t.Fatalf("ReviewPR must not be called with an empty PR number, got %d calls", len(fd.reviewCalls))
	}
	if m.lastErr == nil {
		t.Fatal("expected a status-line error for an empty PR number")
	}
}

// TestPalette_ReviewGatedInObserverMode confirms the review row is disabled
// exactly like the other four verbs when dispatch is unavailable (m.disp ==
// nil, e.g. Observer mode) — confirmPaletteRow's silent no-op gate.
func TestPalette_ReviewGatedInObserverMode(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")
	m = sendKey(m, "j") // -> rowScanTargeted
	m = sendKey(m, "j") // -> rowVerify
	m = sendKey(m, "j") // -> rowRepro
	m = sendKey(m, "j") // -> rowSweep
	m = sendKey(m, "j") // -> rowReconcile
	m = sendKey(m, "j") // -> rowReview

	if m.palette.cursor != rowReview {
		t.Fatalf("cursor = %v, want rowReview", m.palette.cursor)
	}

	// rowReview is an editableField row: the first Enter only focuses the
	// prNumber input (handlePaletteKey's editableField branch runs before
	// any m.disp check). The gate under test is confirmPaletteRow's
	// m.disp==nil check, reached only by a second Enter after typing a
	// value — reproducing exactly what an operator would do.
	m = sendKey(m, "enter")
	if !m.palette.editing {
		t.Fatal("expected editing=true after enter on Review")
	}
	m = typeString(m, "42")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	m = runCmd(m, cmd)

	if m.running {
		t.Error("confirming a row with a nil dispatcher must be a silent no-op, not start a run")
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

	if scan, verify, repro, sweep, reconcile := fd.callCounts(); scan != 1 || verify != 0 || repro != 0 || sweep != 0 || reconcile != 0 {
		t.Fatalf("call counts after refused second dispatch = (scan=%d verify=%d repro=%d sweep=%d reconcile=%d), want (1,0,0,0,0)", scan, verify, repro, sweep, reconcile)
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

// TestPalette_EscClosesPaletteRunUntouched verifies the NEW contract:
// esc while a run is active closes the palette but leaves the dispatch
// context alive. The old behaviour (esc cancelled the run) is the bug.
func TestPalette_EscClosesPaletteRunUntouched(t *testing.T) {
	fd := &fakeDispatcher{mode: engine.Owner, block: make(chan struct{}), sawCancel: make(chan struct{})}
	m := NewModel(context.Background(), &fakeFeed{}, fd)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d") // open palette

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // start Scan Sweep, parks on fd.block
	m = next.(Model)
	if !m.running {
		t.Fatal("expected running=true after dispatch")
	}
	// palette closes when dispatch starts (confirmPaletteRow); re-open it.
	if m.palette.open {
		t.Fatal("expected palette closed once dispatch starts")
	}
	go func() { cmd() }() // let it park; unblocked by defer
	defer close(fd.block)

	m = sendKey(m, "d") // re-open palette while running
	if !m.palette.open {
		t.Fatal("expected palette open after 'd' while running")
	}

	time.Sleep(10 * time.Millisecond)

	// esc must close the palette, not cancel the run.
	m = sendKey(m, "esc")
	if m.palette.open {
		t.Fatal("expected palette closed after esc")
	}
	if !m.running {
		t.Fatal("expected run still active after esc (esc must not cancel)")
	}

	// Confirm the dispatch context was NOT cancelled within a short window.
	select {
	case <-fd.sawCancel:
		t.Fatal("esc incorrectly cancelled the dispatch context")
	case <-time.After(50 * time.Millisecond):
		// good — context was not cancelled
	}
}

// TestPalette_DToggleClosesPalette verifies that pressing 'd' while the
// palette is open closes it (toggle), and pressing 'd' again re-opens it.
func TestPalette_DToggleClosesPalette(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, &fakeDispatcher{mode: engine.Owner})
	m = sendFrame(m, baseFrame())

	m = sendKey(m, "d") // open
	if !m.palette.open {
		t.Fatal("expected palette open after first 'd'")
	}
	m = sendKey(m, "d") // close via toggle
	if m.palette.open {
		t.Fatal("expected palette closed after second 'd' (toggle)")
	}
	m = sendKey(m, "d") // open again
	if !m.palette.open {
		t.Fatal("expected palette open after third 'd'")
	}
}

// TestPalette_CtrlXCancelsWithPaletteOpen verifies ctrl+x cancels the run
// whether the palette is open or closed.
func TestPalette_CtrlXCancelsWithPaletteOpen(t *testing.T) {
	for _, paletteOpen := range []bool{false, true} {
		name := "palette_closed"
		if paletteOpen {
			name = "palette_open"
		}
		t.Run(name, func(t *testing.T) {
			fd := &fakeDispatcher{mode: engine.Owner, block: make(chan struct{}), sawCancel: make(chan struct{})}
			m := NewModel(context.Background(), &fakeFeed{}, fd)
			m = sendFrame(m, baseFrame())
			m = sendKey(m, "d")

			next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // start Scan Sweep
			m = next.(Model)
			if !m.running {
				t.Fatal("expected running=true")
			}
			msgCh := make(chan tea.Msg, 1)
			go func() { msgCh <- cmd() }()
			defer close(fd.block)

			if paletteOpen {
				m = sendKey(m, "d") // re-open palette
				if !m.palette.open {
					t.Fatal("expected palette open")
				}
			}

			time.Sleep(10 * time.Millisecond)
			m = sendKey(m, "ctrl+x")

			select {
			case <-fd.sawCancel:
			case <-time.After(2 * time.Second):
				t.Fatal("ctrl+x did not cancel the dispatch context")
			}

			var msg tea.Msg
			select {
			case msg = <-msgCh:
			case <-time.After(2 * time.Second):
				t.Fatal("dispatch cmd never resolved after ctrl+x cancellation")
			}
			next, _ = m.Update(msg)
			m = next.(Model)
			if m.running {
				t.Fatal("expected running=false after dispatchDoneMsg")
			}
			if !isCancelled(m.lastErr) {
				t.Fatalf("lastErr = %v, want context.Canceled", m.lastErr)
			}
		})
	}
}

// TestPalette_PaletteXCancels verifies the in-palette 'x' key cancels the
// active run (the labeled affordance shown in the footer while running).
func TestPalette_PaletteXCancels(t *testing.T) {
	fd := &fakeDispatcher{mode: engine.Owner, block: make(chan struct{}), sawCancel: make(chan struct{})}
	m := NewModel(context.Background(), &fakeFeed{}, fd)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // start Scan Sweep
	m = next.(Model)
	if !m.running {
		t.Fatal("expected running=true")
	}
	msgCh := make(chan tea.Msg, 1)
	go func() { msgCh <- cmd() }()
	defer close(fd.block)

	// Re-open palette (dispatch closed it); press 'x' to cancel.
	m = sendKey(m, "d")
	if !m.palette.open {
		t.Fatal("expected palette open after 'd'")
	}

	time.Sleep(10 * time.Millisecond)
	m = sendKey(m, "x")

	select {
	case <-fd.sawCancel:
	case <-time.After(2 * time.Second):
		t.Fatal("palette 'x' did not cancel the dispatch context")
	}

	var msg tea.Msg
	select {
	case msg = <-msgCh:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch cmd never resolved after 'x' cancellation")
	}
	next, _ = m.Update(msg)
	m = next.(Model)
	if m.running {
		t.Fatal("expected running=false after dispatchDoneMsg")
	}
	if !isCancelled(m.lastErr) {
		t.Fatalf("lastErr = %v, want context.Canceled", m.lastErr)
	}
}

// TestPalette_DoubleDispatchRefusedWithVisibleReason verifies the
// one-at-a-time policy: trying to confirm a row while a run is active is
// refused, and the rendered palette shows a per-row "run in progress" reason.
func TestPalette_DoubleDispatchRefusedWithVisibleReason(t *testing.T) {
	fd := &fakeDispatcher{mode: engine.Owner, block: make(chan struct{})}
	m := NewModel(context.Background(), &fakeFeed{}, fd)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // starts Scan Sweep, parks
	m = next.(Model)
	if !m.running {
		t.Fatal("expected running=true")
	}
	go func() { cmd() }()
	defer close(fd.block)

	// Re-open palette while running; try confirm — must be refused.
	m = sendKey(m, "d")
	m = sendKey(m, "j") // rowScanTargeted
	m = sendKey(m, "enter")
	time.Sleep(20 * time.Millisecond)

	if scan, verify, repro, sweep, reconcile := fd.callCounts(); scan != 1 || verify != 0 || repro != 0 || sweep != 0 || reconcile != 0 {
		t.Fatalf("call counts after refused second dispatch = (scan=%d verify=%d repro=%d sweep=%d reconcile=%d), want (1,0,0,0,0)",
			scan, verify, repro, sweep, reconcile)
	}

	// Palette render must include the "run in progress" reason on each row.
	rendered := m.viewPalette()
	if !strings.Contains(rendered, "run in progress") {
		t.Errorf("palette render while running must show 'run in progress' reason; got:\n%s", rendered)
	}
}

// TestPalette_FinishedRunDoesNotTrapKeys verifies that when a dispatch
// finishes while the palette is open, result state renders and normal keys
// continue to work (palette does not enter a stuck state).
func TestPalette_FinishedRunDoesNotTrapKeys(t *testing.T) {
	fd := &fakeDispatcher{mode: engine.Owner, scanResult: &engine.ScanResult{}}
	m := NewModel(context.Background(), &fakeFeed{}, fd)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "d")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // starts + finishes Scan Sweep (no block)
	m = next.(Model)
	if !m.running {
		t.Fatal("expected running=true")
	}

	// Deliver the completion message (palette is closed because dispatch started).
	m = runCmd(m, cmd)
	if m.running {
		t.Fatal("expected running=false after dispatch completes")
	}

	// Open the palette after completion; navigation and esc should work normally.
	m = sendKey(m, "d")
	if !m.palette.open {
		t.Fatal("expected palette open after 'd' post-completion")
	}
	m = sendKey(m, "j") // navigate — must not crash or freeze
	if m.palette.cursor == 0 {
		t.Fatal("expected cursor to move after j")
	}
	m = sendKey(m, "esc")
	if m.palette.open {
		t.Fatal("expected palette closed after esc post-completion")
	}
	// Normal key outside palette must work.
	prev := m.focus
	m = sendKey(m, "tab")
	if m.focus == prev {
		t.Fatal("tab should cycle focus after palette close")
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
		{
			"reconcile populated",
			dispatchDoneMsg{verb: "reconcile", summary: reconcileSummary(&engine.ReconcileResult{Result: &funnel.Result{Stats: funnel.Stats{
				ReconcileNominated: 5, ReconcileArbitrated: 4, ReconcileMerged: 2, ReconcileSkippedCap: 1,
			}}})},
			"reconcile: reconcile complete: 5 nominated, 4 arbitrated, 2 merged, 1 skipped (cap)",
		},
		{
			"reconcile nothing nominated",
			dispatchDoneMsg{verb: "reconcile", summary: reconcileSummary(&engine.ReconcileResult{Result: &funnel.Result{}})},
			"reconcile: reconcile: no duplicate candidates nominated",
		},
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

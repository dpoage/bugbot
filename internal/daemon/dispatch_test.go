package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/control"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/store"
)

// fakeVerbDispatcher records the verb/opts it was called with, blocks until
// released (so tests can prove serialization against the cycle loop), and
// returns a scripted summary/error.
type fakeVerbDispatcher struct {
	mu      sync.Mutex
	calls   []control.Verb
	optsLog []control.DispatchOpts

	release chan struct{} // closed to let a blocked call return; nil = never blocks

	summary control.DispatchSummary
	err     error
}

func (f *fakeVerbDispatcher) Dispatch(ctx context.Context, verb control.Verb, opts control.DispatchOpts) (control.DispatchSummary, error) {
	f.mu.Lock()
	f.calls = append(f.calls, verb)
	f.optsLog = append(f.optsLog, opts)
	f.mu.Unlock()

	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return control.DispatchSummary{}, ctx.Err()
		}
	}
	return f.summary, f.err
}

func (f *fakeVerbDispatcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// buildDispatchDaemon is buildDaemon plus a wired Dispatcher, for tests that
// only care about the control-socket dispatch queue (not a real funnel
// cycle). Pre-seeds a prior sweep scan_run so New/Run's startup sweep is
// skipped and the loop parks in its select immediately.
func buildDispatchDaemon(t *testing.T, disp Dispatcher) (*Daemon, *fakeClock) {
	t.Helper()
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	base := fr.commit("init")

	st := openStore(t)
	if _, err := st.BeginScanRun(context.Background(), store.ScanSweep, base); err != nil {
		t.Fatal(err)
	}

	llmc := newFakeLLM(candidateJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))

	d, err := New(Deps{
		Repo:       fr.open(),
		Store:      st,
		Clients:    funnel.RoleClients{Finder: llmc, Verifier: llmc},
		FunnelOpts: funnel.Options{Limits: funnel.StageLimits{Refuters: 1, MaxParallel: 2}},
		Logger:     discardLogger(),
		Dispatch:   disp,
	}, DaemonConfig{
		PollInterval:   time.Hour,
		SweepInterval:  time.Hour,
		PerCycleTokens: 1_000_000,
		PerDayTokens:   10_000_000,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.clock = clk
	return d, clk
}

// TestDaemon_SubmitDispatch_ExecutesAtCycleBoundaryAndReturnsSummary drives a
// verb through SubmitDispatch against a live Run loop and asserts the fake
// Dispatcher received the exact verb/opts and the summary/nil-error round
// -trips back to the caller.
func TestDaemon_SubmitDispatch_ExecutesAtCycleBoundaryAndReturnsSummary(t *testing.T) {
	fake := &fakeVerbDispatcher{summary: control.DispatchSummary{FindingCount: 5, HasResult: true}}
	d, _ := buildDispatchDaemon(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	reqCtx, reqCancel := context.WithTimeout(ctx, 2*time.Second)
	defer reqCancel()
	sum, err := d.SubmitDispatch(reqCtx, control.VerbScan, control.DispatchOpts{Target: "./x", Since: "HEAD~1"})
	if err != nil {
		t.Fatalf("SubmitDispatch() error: %v", err)
	}
	if sum.FindingCount != 5 || !sum.HasResult {
		t.Errorf("SubmitDispatch() summary = %+v, want FindingCount=5 HasResult=true", sum)
	}

	fake.mu.Lock()
	if len(fake.calls) != 1 || fake.calls[0] != control.VerbScan {
		t.Errorf("dispatch calls = %v, want [scan]", fake.calls)
	}
	if len(fake.optsLog) != 1 || fake.optsLog[0].Target != "./x" || fake.optsLog[0].Since != "HEAD~1" {
		t.Errorf("dispatch opts = %+v, want Target=./x Since=HEAD~1", fake.optsLog)
	}
	fake.mu.Unlock()

	cancel()
	<-done
}

// TestDaemon_SubmitDispatch_NeverRunsConcurrentlyWithACycle starts a dispatch
// call that blocks mid-execution (via fakeVerbDispatcher.release) and proves
// the scheduler loop is unavailable to start a NEW cycle/dispatch while it is
// in flight: a second SubmitDispatch queued behind it only reaches the fake
// after the first is released, never overlapping.
func TestDaemon_SubmitDispatch_NeverRunsConcurrentlyWithACycle(t *testing.T) {
	fake := &fakeVerbDispatcher{release: make(chan struct{})}
	d, _ := buildDispatchDaemon(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	first := make(chan error, 1)
	go func() {
		_, err := d.SubmitDispatch(ctx, control.VerbSweep, control.DispatchOpts{})
		first <- err
	}()

	// Wait until the fake has been entered (blocked on release) before firing
	// the second request, so we know the first is genuinely in flight on the
	// scheduler goroutine.
	waitFor(t, func() bool { return fake.callCount() == 1 })

	second := make(chan error, 1)
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		_, err := d.SubmitDispatch(ctx, control.VerbVerify, control.DispatchOpts{})
		second <- err
	}()
	<-secondStarted

	// Give the second request a moment to reach the queue; it must NOT have
	// been picked up by the scheduler yet (only one goroutine drives Run's
	// select loop, and it is still blocked inside the first Dispatch call).
	time.Sleep(50 * time.Millisecond)
	if fake.callCount() != 1 {
		t.Fatalf("second dispatch started concurrently with the first: callCount = %d, want 1", fake.callCount())
	}

	close(fake.release)

	if err := <-first; err != nil {
		t.Errorf("first SubmitDispatch() error: %v", err)
	}
	waitFor(t, func() bool { return fake.callCount() == 2 })
	if err := <-second; err != nil {
		t.Errorf("second SubmitDispatch() error: %v", err)
	}

	fake.mu.Lock()
	if len(fake.calls) != 2 || fake.calls[0] != control.VerbSweep || fake.calls[1] != control.VerbVerify {
		t.Errorf("dispatch calls = %v, want [sweep verify] in order", fake.calls)
	}
	fake.mu.Unlock()

	cancel()
	<-done
}

// TestDaemon_SubmitDispatch_DisabledReturnsError verifies a Daemon built
// with no Dispatcher (nil) refuses dispatch without ever touching the
// scheduler loop.
func TestDaemon_SubmitDispatch_DisabledReturnsError(t *testing.T) {
	d, _ := buildDispatchDaemon(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	_, err := d.SubmitDispatch(ctx, control.VerbScan, control.DispatchOpts{})
	if err == nil {
		t.Fatal("SubmitDispatch() error = nil, want dispatch-disabled error")
	}

	cancel()
	<-done
}

// TestDaemon_SubmitDispatch_ContextCancelledBeforeReply verifies a cancelled
// ctx unblocks SubmitDispatch's caller even though the underlying verb (once
// accepted) keeps running to completion rather than being interrupted
// mid-write.
func TestDaemon_SubmitDispatch_ContextCancelledBeforeReply(t *testing.T) {
	fake := &fakeVerbDispatcher{release: make(chan struct{})}
	d, _ := buildDispatchDaemon(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	reqCtx, reqCancel := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() {
		_, err := d.SubmitDispatch(reqCtx, control.VerbRepro, control.DispatchOpts{})
		result <- err
	}()

	waitFor(t, func() bool { return fake.callCount() == 1 })
	reqCancel()

	select {
	case err := <-result:
		if err == nil {
			t.Error("SubmitDispatch() error = nil after ctx cancel, want context.Canceled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SubmitDispatch() did not return promptly after ctx cancel")
	}

	close(fake.release) // let the in-flight verb finish so Run can shut down cleanly
	cancel()
	<-done
}

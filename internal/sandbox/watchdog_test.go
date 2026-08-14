package sandbox

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestProgressSnapshotComparable(t *testing.T) {
	a := progressSnapshot{outputBytes: 1, fsSize: 2, fsCount: 3, fsMaxModNano: 4}
	b := a
	if a != b {
		t.Error("identical snapshots must compare equal")
	}
	b.fsSize++
	if a == b {
		t.Error("snapshots differing in one field must compare unequal")
	}
}

func TestIdlePollInterval(t *testing.T) {
	cases := []struct {
		idle time.Duration
		want time.Duration
	}{
		{0, time.Second},               // /4 = 0 -> floor 1s
		{2 * time.Second, time.Second}, // /4 = 500ms -> floor 1s
		{8 * time.Second, 2 * time.Second},
		{40 * time.Second, 10 * time.Second},
		{120 * time.Second, 30 * time.Second}, // /4 = 30s -> cap 30s
		{10 * time.Minute, 30 * time.Second},  // capped
	}
	for _, tc := range cases {
		if got := idlePollInterval(tc.idle); got != tc.want {
			t.Errorf("idlePollInterval(%v) = %v, want %v", tc.idle, got, tc.want)
		}
	}
}

// TestEffectivePollInterval pins the decoupling fix from the oracle round
// (bugbot-bdqf review B1a/B2): the growth ceiling's sampling cadence must
// never be DERIVED from idlePollInterval(idleTimeout) — that let a breach
// overshoot the default 2 GiB ceiling by tens of GB under the shipped
// idle_timeout_seconds:120 default (a 30s poll against multi-GB/s disk
// throughput), and idlePollInterval(0)'s 1s floor for a DISABLED idle
// window was an accident of its own clamp, not a deliberate growth
// cadence.
func TestEffectivePollInterval(t *testing.T) {
	cases := []struct {
		name               string
		idleTimeout        time.Duration
		growthCeilingBytes int64
		want               time.Duration
	}{
		{
			name:               "ceiling disabled: exactly idlePollInterval, unchanged behavior",
			idleTimeout:        120 * time.Second,
			growthCeilingBytes: 0,
			want:               idlePollInterval(120 * time.Second), // 30s
		},
		{
			name:               "ceiling disabled, idle also disabled: exactly idlePollInterval(0)",
			idleTimeout:        0,
			growthCeilingBytes: 0,
			want:               idlePollInterval(0), // 1s, unchanged legacy behavior
		},
		{
			name:               "ceiling-only (idle disabled): growthPollInterval, not idlePollInterval(0)'s accidental floor",
			idleTimeout:        0,
			growthCeilingBytes: 1000,
			want:               growthPollInterval,
		},
		{
			name:               "shipped default shape: idle=120s (30s poll) + ceiling active -> tighter growthPollInterval wins",
			idleTimeout:        120 * time.Second,
			growthCeilingBytes: 2 * 1024 * 1024 * 1024,
			want:               growthPollInterval,
		},
		{
			name:               "idle poll already tighter than growthPollInterval -> idle poll wins (no widening)",
			idleTimeout:        2 * time.Second, // idlePollInterval -> 1s floor == growthPollInterval here
			growthCeilingBytes: 1000,
			want:               idlePollInterval(2 * time.Second),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectivePollInterval(tc.idleTimeout, tc.growthCeilingBytes); got != tc.want {
				t.Errorf("effectivePollInterval(%v, %d) = %v, want %v", tc.idleTimeout, tc.growthCeilingBytes, got, tc.want)
			}
		})
	}
}

// TestEffectivePollIntervalNeverExceedsGrowthPollIntervalWhenCeilingActive is
// the general form of the shipped-default regression: for ANY idleTimeout,
// once the growth ceiling is active the effective cadence must never be
// looser than growthPollInterval — otherwise an operator's long (or
// disabled) idle-stall window silently reintroduces the overshoot bug.
func TestEffectivePollIntervalNeverExceedsGrowthPollIntervalWhenCeilingActive(t *testing.T) {
	for _, idle := range []time.Duration{0, 1 * time.Second, 30 * time.Second, 120 * time.Second, 10 * time.Minute, time.Hour} {
		got := effectivePollInterval(idle, 2*1024*1024*1024)
		if got > growthPollInterval {
			t.Errorf("effectivePollInterval(%v, ceiling-active) = %v, must never exceed growthPollInterval (%v)", idle, got, growthPollInterval)
		}
	}
}

func TestWorkspaceProgressDetectsWrites(t *testing.T) {
	dir := t.TempDir()
	s0, c0, m0 := workspaceProgress(dir)

	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	s1, c1, m1 := workspaceProgress(dir)
	if s1 <= s0 {
		t.Errorf("size did not grow after adding a file: %d -> %d", s0, s1)
	}
	if c1 <= c0 {
		t.Errorf("count did not grow after adding a file: %d -> %d", c0, c1)
	}
	if m1 < m0 {
		t.Errorf("max mtime went backwards: %d -> %d", m0, m1)
	}

	// A newer mtime anywhere in the tree counts as progress even with no size
	// change (e.g. a tool rewriting a file in place).
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "a.txt"), future, future); err != nil {
		t.Fatal(err)
	}
	_, _, m2 := workspaceProgress(dir)
	if m2 <= m1 {
		t.Errorf("max mtime did not advance after Chtimes: %d -> %d", m1, m2)
	}
}

// TestWatchIdleFiresOnStall: a run with no progress is cancelled after the idle
// window, and the killed flag is visible before cancel runs.
func TestWatchIdleFiresOnStall(t *testing.T) {
	var killed, quotaExceeded atomic.Bool
	cancelled := make(chan struct{})
	done := make(chan struct{})
	defer close(done)

	noProgress := func() progressSnapshot { return progressSnapshot{} }
	go watchIdle(watchdogArgs{
		done:          done,
		fingerprint:   noProgress,
		limits:        watchdogLimits{idleTimeout: 40 * time.Millisecond},
		pollEvery:     5 * time.Millisecond,
		killed:        &killed,
		quotaExceeded: &quotaExceeded,
		cancel:        func() { close(cancelled) },
	})

	select {
	case <-cancelled:
		if !killed.Load() {
			t.Error("killed must be set before cancel is invoked")
		}
		if quotaExceeded.Load() {
			t.Error("a plain idle-stall kill must not set quotaExceeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchIdle did not fire on a stalled run")
	}
}

// TestWatchIdleCPUKeepsAlive: a run with no output/fs change but a busy CPU
// (activeFallback returns true) is treated as making progress and never killed.
func TestWatchIdleCPUKeepsAlive(t *testing.T) {
	var killed, quotaExceeded atomic.Bool
	done := make(chan struct{})

	flat := func() progressSnapshot { return progressSnapshot{} } // no output, no fs change
	cpuBusy := func() bool { return true }                        // but the container is churning
	go watchIdle(watchdogArgs{
		done:           done,
		fingerprint:    flat,
		activeFallback: cpuBusy,
		limits:         watchdogLimits{idleTimeout: 40 * time.Millisecond},
		pollEvery:      5 * time.Millisecond,
		killed:         &killed,
		quotaExceeded:  &quotaExceeded,
		cancel:         func() { killed.Store(true) },
	})

	time.Sleep(250 * time.Millisecond)
	close(done)
	if killed.Load() {
		t.Error("watchIdle fired despite the container being CPU-busy")
	}
}

// TestWatchIdleNoFireWhenProgressing: continuous progress keeps resetting the
// clock, so the watchdog never cancels.
func TestWatchIdleNoFireWhenProgressing(t *testing.T) {
	var killed, quotaExceeded atomic.Bool
	var n atomic.Int64
	done := make(chan struct{})

	progressing := func() progressSnapshot { return progressSnapshot{outputBytes: n.Add(1)} }
	go watchIdle(watchdogArgs{
		done:          done,
		fingerprint:   progressing,
		limits:        watchdogLimits{idleTimeout: 40 * time.Millisecond},
		pollEvery:     5 * time.Millisecond,
		killed:        &killed,
		quotaExceeded: &quotaExceeded,
		cancel:        func() { killed.Store(true) },
	})

	time.Sleep(250 * time.Millisecond) // ~50 polls, each shows fresh progress
	close(done)
	if killed.Load() {
		t.Error("watchIdle fired despite continuous progress")
	}
}

// TestWatchIdleDisabled: both limits <= 0 returns immediately and never fires.
func TestWatchIdleDisabled(t *testing.T) {
	var killed, quotaExceeded atomic.Bool
	done := make(chan struct{})
	defer close(done)
	returned := make(chan struct{})

	go func() {
		watchIdle(watchdogArgs{
			done:          done,
			fingerprint:   func() progressSnapshot { return progressSnapshot{} },
			limits:        watchdogLimits{},
			pollEvery:     time.Millisecond,
			killed:        &killed,
			quotaExceeded: &quotaExceeded,
			cancel:        func() { killed.Store(true) },
		})
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("watchIdle with both limits disabled must return immediately")
	}
	if killed.Load() {
		t.Error("disabled watchdog must never fire")
	}
}

// TestWatchIdleStopsOnDone: closing done returns the watchdog without firing,
// even when the idle window has not elapsed.
func TestWatchIdleStopsOnDone(t *testing.T) {
	var killed, quotaExceeded atomic.Bool
	done := make(chan struct{})
	returned := make(chan struct{})

	go func() {
		watchIdle(watchdogArgs{
			done:          done,
			fingerprint:   func() progressSnapshot { return progressSnapshot{} },
			limits:        watchdogLimits{idleTimeout: time.Hour},
			pollEvery:     5 * time.Millisecond,
			killed:        &killed,
			quotaExceeded: &quotaExceeded,
			cancel:        func() { killed.Store(true) },
		})
		close(returned)
	}()

	time.Sleep(20 * time.Millisecond)
	close(done)

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("watchIdle did not return after done was closed")
	}
	if killed.Load() {
		t.Error("watchdog fired though the run finished on its own")
	}
}

// TestCappedBufferWrittenCountsBeyondCap: the output-activity signal must keep
// growing even after the buffer is full, so a chatty-then-stalled run is still
// seen as having made progress.
func TestCappedBufferWrittenCountsBeyondCap(t *testing.T) {
	b := newCappedBuffer(4)
	if _, err := b.Write([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	if got := b.written(); got != 2 {
		t.Errorf("written = %d, want 2", got)
	}
	if _, err := b.Write([]byte("cdef")); err != nil { // exceeds the 4-byte cap
		t.Fatal(err)
	}
	if got := b.written(); got != 6 {
		t.Errorf("written = %d, want 6 (must count bytes discarded over cap)", got)
	}
	if _, trunc := b.result(); !trunc {
		t.Error("buffer should report truncation after exceeding cap")
	}
}

// TestWatchIdleFiresOnGrowthCeiling pins bugbot-bdqf's acceptance: a run
// whose ONLY activity is file growth (a disk-filler) is killed by the
// growth ceiling — not left to run until Timeout, and not misreported as a
// plain idle stall. fsSize climbing every tick is exactly the pathological
// case the bug report described: it constantly "changes" the fingerprint,
// so a naive idle-only watchdog would treat it as perpetual progress and
// never fire. idleTimeout here is deliberately generous (1 hour) to prove
// the growth ceiling fires independently of idle-stall detection, well
// before any idle-based kill ever could. killed must stay FALSE — that
// flag is reserved for a plain idle-stall kill (see watchdogArgs); a
// growth-ceiling kill is signaled by quotaExceeded alone.
func TestWatchIdleFiresOnGrowthCeiling(t *testing.T) {
	var killed, quotaExceeded atomic.Bool
	cancelled := make(chan struct{})
	done := make(chan struct{})
	defer close(done)

	const ceiling = 1000 // bytes
	var grown atomic.Int64
	fingerprint := func() progressSnapshot {
		// Simulate a process that only appends to a file: fsSize keeps
		// climbing every poll, which would reset a plain idle clock forever.
		return progressSnapshot{fsSize: grown.Add(200)}
	}
	go watchIdle(watchdogArgs{
		done:          done,
		fingerprint:   fingerprint,
		base:          fingerprint(),
		limits:        watchdogLimits{idleTimeout: time.Hour, growthCeilingBytes: ceiling},
		pollEvery:     5 * time.Millisecond,
		killed:        &killed,
		quotaExceeded: &quotaExceeded,
		cancel:        func() { close(cancelled) },
	})

	select {
	case <-cancelled:
		if killed.Load() {
			t.Error("a growth-ceiling kill must NOT set killed (that flag is idle-stall-only)")
		}
		if !quotaExceeded.Load() {
			t.Error("a growth-ceiling kill must set quotaExceeded (the distinct reason)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchIdle did not fire on workspace growth past the ceiling")
	}
}

// TestWatchIdleGrowthCeilingMeasuresFromBaseline: the ceiling bounds GROWTH
// since watchIdle started sampling (a.base), not absolute fsSize — a
// workspace that already holds more than the ceiling's worth of bytes at
// watch start (e.g. a large pre-existing repo copy) must not immediately
// trip the ceiling; only bytes written AFTER that baseline count.
func TestWatchIdleGrowthCeilingMeasuresFromBaseline(t *testing.T) {
	var killed, quotaExceeded atomic.Bool
	done := make(chan struct{})

	const baseline = 10_000_000 // pre-existing workspace content, far over the ceiling
	const ceiling = 1000
	fingerprint := func() progressSnapshot { return progressSnapshot{fsSize: baseline} } // never grows past baseline
	go watchIdle(watchdogArgs{
		done:          done,
		fingerprint:   fingerprint,
		base:          fingerprint(),
		limits:        watchdogLimits{growthCeilingBytes: ceiling},
		pollEvery:     5 * time.Millisecond,
		killed:        &killed,
		quotaExceeded: &quotaExceeded,
		cancel:        func() { killed.Store(true) },
	})

	time.Sleep(100 * time.Millisecond)
	close(done)
	if killed.Load() || quotaExceeded.Load() {
		t.Error("growth ceiling must be measured from the watch-start baseline, not absolute fsSize")
	}
}

// TestWatchIdleGrowthCeilingDisabled: growthCeilingBytes <= 0 disables the
// ceiling even while unbounded growth continues — idle-stall detection (or
// its own absence) is unaffected.
func TestWatchIdleGrowthCeilingDisabled(t *testing.T) {
	var killed, quotaExceeded atomic.Bool
	done := make(chan struct{})

	var grown atomic.Int64
	fingerprint := func() progressSnapshot { return progressSnapshot{fsSize: grown.Add(10_000)} }
	go watchIdle(watchdogArgs{
		done:          done,
		fingerprint:   fingerprint,
		base:          fingerprint(),
		limits:        watchdogLimits{idleTimeout: time.Hour, growthCeilingBytes: 0},
		pollEvery:     5 * time.Millisecond,
		killed:        &killed,
		quotaExceeded: &quotaExceeded,
		cancel:        func() { killed.Store(true) },
	})

	time.Sleep(100 * time.Millisecond)
	close(done)
	if killed.Load() || quotaExceeded.Load() {
		t.Error("growthCeilingBytes<=0 must disable the ceiling")
	}
}

// TestWatchIdleRunsWithGrowthCeilingOnlyNoIdleTimeout: watchIdle must still
// sample (and enforce the growth ceiling) even when idleTimeout is disabled
// — a disk-filler must be caught regardless of the operator's idle-timeout
// setting, since disk growth is independent of idle-stall semantics.
func TestWatchIdleRunsWithGrowthCeilingOnlyNoIdleTimeout(t *testing.T) {
	var killed, quotaExceeded atomic.Bool
	cancelled := make(chan struct{})
	done := make(chan struct{})
	defer close(done)

	var grown atomic.Int64
	fingerprint := func() progressSnapshot { return progressSnapshot{fsSize: grown.Add(200)} }
	go watchIdle(watchdogArgs{
		done:          done,
		fingerprint:   fingerprint,
		base:          fingerprint(),
		limits:        watchdogLimits{idleTimeout: 0, growthCeilingBytes: 1000},
		pollEvery:     5 * time.Millisecond,
		killed:        &killed,
		quotaExceeded: &quotaExceeded,
		cancel:        func() { close(cancelled) },
	})

	select {
	case <-cancelled:
		if !quotaExceeded.Load() {
			t.Error("expected a growth-ceiling kill with idleTimeout disabled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchIdle did not run with idleTimeout<=0 and growthCeilingBytes>0")
	}
}

// TestCheckGrowthCeiling_DetectsBreach: the post-run check (bugbot-bdqf
// oracle review B1b) catches a breach that no tick observed — e.g. a burst
// write that completed inside a single poll window.
func TestCheckGrowthCeiling_DetectsBreach(t *testing.T) {
	var quotaExceeded atomic.Bool
	base := progressSnapshot{fsSize: 100}
	fingerprint := func() progressSnapshot { return progressSnapshot{fsSize: 100 + 5000} }

	checkGrowthCeiling(fingerprint, base, 1000, &quotaExceeded)
	if !quotaExceeded.Load() {
		t.Error("a final growth of 5000 bytes past a 1000-byte ceiling must set quotaExceeded")
	}
}

// TestCheckGrowthCeiling_NoBreach: growth within the ceiling leaves
// quotaExceeded false.
func TestCheckGrowthCeiling_NoBreach(t *testing.T) {
	var quotaExceeded atomic.Bool
	base := progressSnapshot{fsSize: 100}
	fingerprint := func() progressSnapshot { return progressSnapshot{fsSize: 100 + 500} }

	checkGrowthCeiling(fingerprint, base, 1000, &quotaExceeded)
	if quotaExceeded.Load() {
		t.Error("growth within the ceiling must not set quotaExceeded")
	}
}

// TestCheckGrowthCeiling_DisabledNeverCallsFingerprint: growthCeilingBytes
// <= 0 must return WITHOUT calling fingerprint — this is what makes it safe
// for Exec to pass a nil fingerprint when neither idleTimeout nor the
// ceiling was ever configured (fingerprint is never allocated in that
// case).
func TestCheckGrowthCeiling_DisabledNeverCallsFingerprint(t *testing.T) {
	var quotaExceeded atomic.Bool
	called := false
	fingerprint := func() progressSnapshot { called = true; return progressSnapshot{} }

	checkGrowthCeiling(fingerprint, progressSnapshot{}, 0, &quotaExceeded)
	if called {
		t.Error("checkGrowthCeiling must not call fingerprint when the ceiling is disabled")
	}
	if quotaExceeded.Load() {
		t.Error("a disabled ceiling must never set quotaExceeded")
	}

	// The nil-fingerprint safety claim itself: must not panic.
	checkGrowthCeiling(nil, progressSnapshot{}, 0, &quotaExceeded)
}

// TestCheckGrowthCeiling_AlreadyExceededSkipsResample: once a tick has
// already caught the breach (quotaExceeded already true), the post-run
// check must be a no-op — no second filesystem walk.
func TestCheckGrowthCeiling_AlreadyExceededSkipsResample(t *testing.T) {
	var quotaExceeded atomic.Bool
	quotaExceeded.Store(true)
	called := false
	fingerprint := func() progressSnapshot { called = true; return progressSnapshot{fsSize: 999999} }

	checkGrowthCeiling(fingerprint, progressSnapshot{}, 1000, &quotaExceeded)
	if called {
		t.Error("checkGrowthCeiling must not re-sample when quotaExceeded is already true")
	}
}

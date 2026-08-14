package sandbox

import (
	"os"
	"path/filepath"
	"strconv"
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
// cadence. bugbot-gb3o extends the same guarantee to the file-count
// ceiling: it must drive the same tight cadence as the byte-size ceiling,
// independently or together.
func TestEffectivePollInterval(t *testing.T) {
	cases := []struct {
		name               string
		idleTimeout        time.Duration
		growthCeilingBytes int64
		fileCountCeiling   int64
		want               time.Duration
	}{
		{
			name:               "both ceilings disabled: exactly idlePollInterval, unchanged behavior",
			idleTimeout:        120 * time.Second,
			growthCeilingBytes: 0,
			fileCountCeiling:   0,
			want:               idlePollInterval(120 * time.Second), // 30s
		},
		{
			name:               "both ceilings disabled, idle also disabled: exactly idlePollInterval(0)",
			idleTimeout:        0,
			growthCeilingBytes: 0,
			fileCountCeiling:   0,
			want:               idlePollInterval(0), // 1s, unchanged legacy behavior
		},
		{
			name:               "byte ceiling only (idle disabled): growthPollInterval, not idlePollInterval(0)'s accidental floor",
			idleTimeout:        0,
			growthCeilingBytes: 1000,
			fileCountCeiling:   0,
			want:               growthPollInterval,
		},
		{
			name:               "file-count ceiling only (idle disabled): growthPollInterval",
			idleTimeout:        0,
			growthCeilingBytes: 0,
			fileCountCeiling:   500,
			want:               growthPollInterval,
		},
		{
			name:               "both growth ceilings active, idle disabled: growthPollInterval",
			idleTimeout:        0,
			growthCeilingBytes: 1000,
			fileCountCeiling:   500,
			want:               growthPollInterval,
		},
		{
			name:               "shipped default shape: idle=120s (30s poll) + byte ceiling active -> tighter growthPollInterval wins",
			idleTimeout:        120 * time.Second,
			growthCeilingBytes: 2 * 1024 * 1024 * 1024,
			fileCountCeiling:   0,
			want:               growthPollInterval,
		},
		{
			name:               "shipped default shape: idle=120s (30s poll) + file-count ceiling active -> tighter growthPollInterval wins",
			idleTimeout:        120 * time.Second,
			growthCeilingBytes: 0,
			fileCountCeiling:   200_000,
			want:               growthPollInterval,
		},
		{
			name:               "idle poll already tighter than growthPollInterval -> idle poll wins (no widening)",
			idleTimeout:        2 * time.Second, // idlePollInterval -> 1s floor == growthPollInterval here
			growthCeilingBytes: 1000,
			fileCountCeiling:   500,
			want:               idlePollInterval(2 * time.Second),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectivePollInterval(tc.idleTimeout, tc.growthCeilingBytes, tc.fileCountCeiling); got != tc.want {
				t.Errorf("effectivePollInterval(%v, %d, %d) = %v, want %v", tc.idleTimeout, tc.growthCeilingBytes, tc.fileCountCeiling, got, tc.want)
			}
		})
	}
}

// TestEffectivePollIntervalNeverExceedsGrowthPollIntervalWhenCeilingActive is
// the general form of the shipped-default regression: for ANY idleTimeout,
// once EITHER growth ceiling (byte-size or file-count) is active the
// effective cadence must never be looser than growthPollInterval —
// otherwise an operator's long (or disabled) idle-stall window silently
// reintroduces the overshoot bug.
func TestEffectivePollIntervalNeverExceedsGrowthPollIntervalWhenCeilingActive(t *testing.T) {
	idles := []time.Duration{0, 1 * time.Second, 30 * time.Second, 120 * time.Second, 10 * time.Minute, time.Hour}
	for _, idle := range idles {
		if got := effectivePollInterval(idle, 2*1024*1024*1024, 0); got > growthPollInterval {
			t.Errorf("effectivePollInterval(%v, byte-ceiling-active, 0) = %v, must never exceed growthPollInterval (%v)", idle, got, growthPollInterval)
		}
		if got := effectivePollInterval(idle, 0, 200_000); got > growthPollInterval {
			t.Errorf("effectivePollInterval(%v, 0, file-count-ceiling-active) = %v, must never exceed growthPollInterval (%v)", idle, got, growthPollInterval)
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

// TestWorkspaceProgressCountsManyTinyFiles pins bugbot-gb3o's motivating
// measurement: many tiny files grow fsCount substantially while fsSize
// barely moves — the exact shape that lets a workload evade a byte-size
// ceiling while still exhausting host inodes.
func TestWorkspaceProgressCountsManyTinyFiles(t *testing.T) {
	dir := t.TempDir()
	s0, c0, _ := workspaceProgress(dir)
	const n = 500
	for i := range n {
		writeFile(t, filepath.Join(dir, "f"+strconv.Itoa(i)), "x") // 1 byte each
	}
	s1, c1, _ := workspaceProgress(dir)
	if got := c1 - c0; got != n {
		t.Errorf("fsCount grew by %d, want %d", got, n)
	}
	if grew := s1 - s0; grew > 10*n {
		t.Errorf("fsSize grew by %d bytes for %d one-byte files — sanity check failed (bound too loose)", grew, n)
	}
}

// TestWatchIdleFiresOnStall: a run with no progress is cancelled after the idle
// window, and the killed flag is visible before cancel runs.
func TestWatchIdleFiresOnStall(t *testing.T) {
	var killed, quotaExceeded, fileCountExceeded atomic.Bool
	cancelled := make(chan struct{})
	done := make(chan struct{})
	defer close(done)

	noProgress := func() progressSnapshot { return progressSnapshot{} }
	go watchIdle(watchdogArgs{
		done:              done,
		fingerprint:       noProgress,
		limits:            watchdogLimits{idleTimeout: 40 * time.Millisecond},
		pollEvery:         5 * time.Millisecond,
		killed:            &killed,
		quotaExceeded:     &quotaExceeded,
		fileCountExceeded: &fileCountExceeded,
		cancel:            func() { close(cancelled) },
	})

	select {
	case <-cancelled:
		if !killed.Load() {
			t.Error("killed must be set before cancel is invoked")
		}
		if quotaExceeded.Load() {
			t.Error("a plain idle-stall kill must not set quotaExceeded")
		}
		if fileCountExceeded.Load() {
			t.Error("a plain idle-stall kill must not set fileCountExceeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchIdle did not fire on a stalled run")
	}
}

// TestWatchIdleCPUKeepsAlive: a run with no output/fs change but a busy CPU
// (activeFallback returns true) is treated as making progress and never killed.
func TestWatchIdleCPUKeepsAlive(t *testing.T) {
	var killed, quotaExceeded, fileCountExceeded atomic.Bool
	done := make(chan struct{})

	flat := func() progressSnapshot { return progressSnapshot{} } // no output, no fs change
	cpuBusy := func() bool { return true }                        // but the container is churning
	go watchIdle(watchdogArgs{
		done:              done,
		fingerprint:       flat,
		activeFallback:    cpuBusy,
		limits:            watchdogLimits{idleTimeout: 40 * time.Millisecond},
		pollEvery:         5 * time.Millisecond,
		killed:            &killed,
		quotaExceeded:     &quotaExceeded,
		fileCountExceeded: &fileCountExceeded,
		cancel:            func() { killed.Store(true) },
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
	var killed, quotaExceeded, fileCountExceeded atomic.Bool
	var n atomic.Int64
	done := make(chan struct{})

	progressing := func() progressSnapshot { return progressSnapshot{outputBytes: n.Add(1)} }
	go watchIdle(watchdogArgs{
		done:              done,
		fingerprint:       progressing,
		limits:            watchdogLimits{idleTimeout: 40 * time.Millisecond},
		pollEvery:         5 * time.Millisecond,
		killed:            &killed,
		quotaExceeded:     &quotaExceeded,
		fileCountExceeded: &fileCountExceeded,
		cancel:            func() { killed.Store(true) },
	})

	time.Sleep(250 * time.Millisecond) // ~50 polls, each shows fresh progress
	close(done)
	if killed.Load() {
		t.Error("watchIdle fired despite continuous progress")
	}
}

// TestWatchIdleDisabled: all three limits <= 0 returns immediately and never fires.
func TestWatchIdleDisabled(t *testing.T) {
	var killed, quotaExceeded, fileCountExceeded atomic.Bool
	done := make(chan struct{})
	defer close(done)
	returned := make(chan struct{})

	go func() {
		watchIdle(watchdogArgs{
			done:              done,
			fingerprint:       func() progressSnapshot { return progressSnapshot{} },
			limits:            watchdogLimits{},
			pollEvery:         time.Millisecond,
			killed:            &killed,
			quotaExceeded:     &quotaExceeded,
			fileCountExceeded: &fileCountExceeded,
			cancel:            func() { killed.Store(true) },
		})
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("watchIdle with all limits disabled must return immediately")
	}
	if killed.Load() {
		t.Error("disabled watchdog must never fire")
	}
}

// TestWatchIdleStopsOnDone: closing done returns the watchdog without firing,
// even when the idle window has not elapsed.
func TestWatchIdleStopsOnDone(t *testing.T) {
	var killed, quotaExceeded, fileCountExceeded atomic.Bool
	done := make(chan struct{})
	returned := make(chan struct{})

	go func() {
		watchIdle(watchdogArgs{
			done:              done,
			fingerprint:       func() progressSnapshot { return progressSnapshot{} },
			limits:            watchdogLimits{idleTimeout: time.Hour},
			pollEvery:         5 * time.Millisecond,
			killed:            &killed,
			quotaExceeded:     &quotaExceeded,
			fileCountExceeded: &fileCountExceeded,
			cancel:            func() { killed.Store(true) },
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
// growth-ceiling kill is signaled by quotaExceeded alone, and
// fileCountExceeded must stay false too (bugbot-gb3o: the two growth
// reasons must never collapse into each other).
func TestWatchIdleFiresOnGrowthCeiling(t *testing.T) {
	var killed, quotaExceeded, fileCountExceeded atomic.Bool
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
		done:              done,
		fingerprint:       fingerprint,
		base:              fingerprint(),
		limits:            watchdogLimits{idleTimeout: time.Hour, growthCeilingBytes: ceiling},
		pollEvery:         5 * time.Millisecond,
		killed:            &killed,
		quotaExceeded:     &quotaExceeded,
		fileCountExceeded: &fileCountExceeded,
		cancel:            func() { close(cancelled) },
	})

	select {
	case <-cancelled:
		if killed.Load() {
			t.Error("a growth-ceiling kill must NOT set killed (that flag is idle-stall-only)")
		}
		if !quotaExceeded.Load() {
			t.Error("a growth-ceiling kill must set quotaExceeded (the distinct reason)")
		}
		if fileCountExceeded.Load() {
			t.Error("a byte-size growth-ceiling kill must NOT set fileCountExceeded")
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
	var killed, quotaExceeded, fileCountExceeded atomic.Bool
	done := make(chan struct{})

	const baseline = 10_000_000 // pre-existing workspace content, far over the ceiling
	const ceiling = 1000
	fingerprint := func() progressSnapshot { return progressSnapshot{fsSize: baseline} } // never grows past baseline
	go watchIdle(watchdogArgs{
		done:              done,
		fingerprint:       fingerprint,
		base:              fingerprint(),
		limits:            watchdogLimits{growthCeilingBytes: ceiling},
		pollEvery:         5 * time.Millisecond,
		killed:            &killed,
		quotaExceeded:     &quotaExceeded,
		fileCountExceeded: &fileCountExceeded,
		cancel:            func() { killed.Store(true) },
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
	var killed, quotaExceeded, fileCountExceeded atomic.Bool
	done := make(chan struct{})

	var grown atomic.Int64
	fingerprint := func() progressSnapshot { return progressSnapshot{fsSize: grown.Add(10_000)} }
	go watchIdle(watchdogArgs{
		done:              done,
		fingerprint:       fingerprint,
		base:              fingerprint(),
		limits:            watchdogLimits{idleTimeout: time.Hour, growthCeilingBytes: 0},
		pollEvery:         5 * time.Millisecond,
		killed:            &killed,
		quotaExceeded:     &quotaExceeded,
		fileCountExceeded: &fileCountExceeded,
		cancel:            func() { killed.Store(true) },
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
	var killed, quotaExceeded, fileCountExceeded atomic.Bool
	cancelled := make(chan struct{})
	done := make(chan struct{})
	defer close(done)

	var grown atomic.Int64
	fingerprint := func() progressSnapshot { return progressSnapshot{fsSize: grown.Add(200)} }
	go watchIdle(watchdogArgs{
		done:              done,
		fingerprint:       fingerprint,
		base:              fingerprint(),
		limits:            watchdogLimits{idleTimeout: 0, growthCeilingBytes: 1000},
		pollEvery:         5 * time.Millisecond,
		killed:            &killed,
		quotaExceeded:     &quotaExceeded,
		fileCountExceeded: &fileCountExceeded,
		cancel:            func() { close(cancelled) },
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

// TestWatchIdleFiresOnFileCountCeiling is TestWatchIdleFiresOnGrowthCeiling's
// file-count analogue (bugbot-gb3o): a run whose ONLY activity is creating
// many tiny files (fsCount climbing every tick, fsSize essentially flat) is
// killed by the file-count ceiling — not left to run until Timeout, not
// misreported as a plain idle stall, and NOT collapsed into the byte-size
// quotaExceeded reason. This is the exact pathological case the bug report
// measured: 10,000 files totaling 20 KB, which a byte-size ceiling never
// catches.
func TestWatchIdleFiresOnFileCountCeiling(t *testing.T) {
	var killed, quotaExceeded, fileCountExceeded atomic.Bool
	cancelled := make(chan struct{})
	done := make(chan struct{})
	defer close(done)

	const ceiling = 100 // entries
	var grown atomic.Int64
	fingerprint := func() progressSnapshot {
		// One-byte files: fsSize barely moves, fsCount climbs every poll.
		return progressSnapshot{fsSize: grown.Load(), fsCount: grown.Add(20)}
	}
	go watchIdle(watchdogArgs{
		done:              done,
		fingerprint:       fingerprint,
		base:              fingerprint(),
		limits:            watchdogLimits{idleTimeout: time.Hour, fileCountCeiling: ceiling},
		pollEvery:         5 * time.Millisecond,
		killed:            &killed,
		quotaExceeded:     &quotaExceeded,
		fileCountExceeded: &fileCountExceeded,
		cancel:            func() { close(cancelled) },
	})

	select {
	case <-cancelled:
		if killed.Load() {
			t.Error("a file-count-ceiling kill must NOT set killed (that flag is idle-stall-only)")
		}
		if quotaExceeded.Load() {
			t.Error("a file-count-ceiling kill must NOT set quotaExceeded (the byte-size reason) — the two must stay distinct")
		}
		if !fileCountExceeded.Load() {
			t.Error("a file-count-ceiling kill must set fileCountExceeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchIdle did not fire on workspace file-count growth past the ceiling")
	}
}

// TestWatchIdleFileCountCeilingMeasuresFromBaseline is
// TestWatchIdleGrowthCeilingMeasuresFromBaseline's file-count analogue: a
// pre-existing workspace holding more entries than the ceiling (e.g. a
// large repo copy or a populated node_modules) must NOT immediately trip
// the ceiling — only entries created AFTER the baseline count.
func TestWatchIdleFileCountCeilingMeasuresFromBaseline(t *testing.T) {
	var killed, quotaExceeded, fileCountExceeded atomic.Bool
	done := make(chan struct{})

	const baseline = 50_000 // pre-existing workspace entries, far over the ceiling
	const ceiling = 100
	fingerprint := func() progressSnapshot { return progressSnapshot{fsCount: baseline} } // never grows past baseline
	go watchIdle(watchdogArgs{
		done:              done,
		fingerprint:       fingerprint,
		base:              fingerprint(),
		limits:            watchdogLimits{fileCountCeiling: ceiling},
		pollEvery:         5 * time.Millisecond,
		killed:            &killed,
		quotaExceeded:     &quotaExceeded,
		fileCountExceeded: &fileCountExceeded,
		cancel:            func() { killed.Store(true) },
	})

	time.Sleep(100 * time.Millisecond)
	close(done)
	if killed.Load() || fileCountExceeded.Load() {
		t.Error("the file-count ceiling must be measured from the watch-start baseline, not absolute fsCount — a normal repo copy with many pre-existing files must not trip it")
	}
}

// TestWatchIdleFileCountCeilingDisabled: fileCountCeiling <= 0 disables the
// ceiling even while unbounded entry-count growth continues.
func TestWatchIdleFileCountCeilingDisabled(t *testing.T) {
	var killed, quotaExceeded, fileCountExceeded atomic.Bool
	done := make(chan struct{})

	var grown atomic.Int64
	fingerprint := func() progressSnapshot { return progressSnapshot{fsCount: grown.Add(1000)} }
	go watchIdle(watchdogArgs{
		done:              done,
		fingerprint:       fingerprint,
		base:              fingerprint(),
		limits:            watchdogLimits{idleTimeout: time.Hour, fileCountCeiling: 0},
		pollEvery:         5 * time.Millisecond,
		killed:            &killed,
		quotaExceeded:     &quotaExceeded,
		fileCountExceeded: &fileCountExceeded,
		cancel:            func() { killed.Store(true) },
	})

	time.Sleep(100 * time.Millisecond)
	close(done)
	if killed.Load() || fileCountExceeded.Load() {
		t.Error("fileCountCeiling<=0 must disable the ceiling")
	}
}

// TestWatchIdleRunsWithFileCountCeilingOnlyNoIdleTimeout: watchIdle must
// still sample (and enforce the file-count ceiling) even when idleTimeout
// AND the byte-size ceiling are both disabled — a many-tiny-files run must
// be caught regardless of the operator's other watchdog settings.
func TestWatchIdleRunsWithFileCountCeilingOnlyNoIdleTimeout(t *testing.T) {
	var killed, quotaExceeded, fileCountExceeded atomic.Bool
	cancelled := make(chan struct{})
	done := make(chan struct{})
	defer close(done)

	var grown atomic.Int64
	fingerprint := func() progressSnapshot { return progressSnapshot{fsCount: grown.Add(20)} }
	go watchIdle(watchdogArgs{
		done:              done,
		fingerprint:       fingerprint,
		base:              fingerprint(),
		limits:            watchdogLimits{idleTimeout: 0, growthCeilingBytes: 0, fileCountCeiling: 100},
		pollEvery:         5 * time.Millisecond,
		killed:            &killed,
		quotaExceeded:     &quotaExceeded,
		fileCountExceeded: &fileCountExceeded,
		cancel:            func() { close(cancelled) },
	})

	select {
	case <-cancelled:
		if !fileCountExceeded.Load() {
			t.Error("expected a file-count-ceiling kill with idleTimeout and the byte-size ceiling both disabled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchIdle did not run with idleTimeout<=0, growthCeilingBytes<=0, and fileCountCeiling>0")
	}
}

// TestCheckGrowthCeiling_DetectsBreach: the post-run check (bugbot-bdqf
// oracle review B1b) catches a byte-size breach that no tick observed —
// e.g. a burst write that completed inside a single poll window.
func TestCheckGrowthCeiling_DetectsBreach(t *testing.T) {
	var quotaExceeded, fileCountExceeded atomic.Bool
	base := progressSnapshot{fsSize: 100}
	fingerprint := func() progressSnapshot { return progressSnapshot{fsSize: 100 + 5000} }

	checkGrowthCeiling(fingerprint, base, 1000, 0, &quotaExceeded, &fileCountExceeded)
	if !quotaExceeded.Load() {
		t.Error("a final growth of 5000 bytes past a 1000-byte ceiling must set quotaExceeded")
	}
	if fileCountExceeded.Load() {
		t.Error("a byte-size-only breach must not set fileCountExceeded")
	}
}

// TestCheckGrowthCeiling_NoBreach: growth within the ceiling leaves
// quotaExceeded false.
func TestCheckGrowthCeiling_NoBreach(t *testing.T) {
	var quotaExceeded, fileCountExceeded atomic.Bool
	base := progressSnapshot{fsSize: 100}
	fingerprint := func() progressSnapshot { return progressSnapshot{fsSize: 100 + 500} }

	checkGrowthCeiling(fingerprint, base, 1000, 0, &quotaExceeded, &fileCountExceeded)
	if quotaExceeded.Load() {
		t.Error("growth within the ceiling must not set quotaExceeded")
	}
}

// TestCheckGrowthCeiling_DisabledNeverCallsFingerprint: both ceilings <= 0
// must return WITHOUT calling fingerprint — this is what makes it safe for
// Exec to pass a nil fingerprint when neither idleTimeout nor either
// ceiling was ever configured (fingerprint is never allocated in that
// case).
func TestCheckGrowthCeiling_DisabledNeverCallsFingerprint(t *testing.T) {
	var quotaExceeded, fileCountExceeded atomic.Bool
	called := false
	fingerprint := func() progressSnapshot { called = true; return progressSnapshot{} }

	checkGrowthCeiling(fingerprint, progressSnapshot{}, 0, 0, &quotaExceeded, &fileCountExceeded)
	if called {
		t.Error("checkGrowthCeiling must not call fingerprint when both ceilings are disabled")
	}
	if quotaExceeded.Load() || fileCountExceeded.Load() {
		t.Error("disabled ceilings must never set either exceeded flag")
	}

	// The nil-fingerprint safety claim itself: must not panic.
	checkGrowthCeiling(nil, progressSnapshot{}, 0, 0, &quotaExceeded, &fileCountExceeded)
}

// TestCheckGrowthCeiling_AlreadyExceededSkipsResample: once a tick has
// already caught the byte-size breach (quotaExceeded already true) and the
// file-count ceiling is disabled, the post-run check must be a no-op — no
// second filesystem walk.
func TestCheckGrowthCeiling_AlreadyExceededSkipsResample(t *testing.T) {
	var quotaExceeded, fileCountExceeded atomic.Bool
	quotaExceeded.Store(true)
	called := false
	fingerprint := func() progressSnapshot { called = true; return progressSnapshot{fsSize: 999999} }

	checkGrowthCeiling(fingerprint, progressSnapshot{}, 1000, 0, &quotaExceeded, &fileCountExceeded)
	if called {
		t.Error("checkGrowthCeiling must not re-sample when quotaExceeded is already true and the file-count ceiling is disabled")
	}
}

// TestCheckGrowthCeiling_FileCount_DetectsBreach is
// TestCheckGrowthCeiling_DetectsBreach's file-count analogue (bugbot-gb3o):
// the post-run check catches a file-count breach that no tick observed —
// e.g. a burst of file creation that completed inside a single poll
// window. This is what makes the acceptance criterion "a burst that
// creates files and exits inside one poll window is still classified"
// true: dropping this check (or this call) would let such a burst report
// a false clean exit.
func TestCheckGrowthCeiling_FileCount_DetectsBreach(t *testing.T) {
	var quotaExceeded, fileCountExceeded atomic.Bool
	base := progressSnapshot{fsCount: 10}
	fingerprint := func() progressSnapshot { return progressSnapshot{fsCount: 10 + 5000} }

	checkGrowthCeiling(fingerprint, base, 0, 1000, &quotaExceeded, &fileCountExceeded)
	if !fileCountExceeded.Load() {
		t.Error("a final growth of 5000 entries past a 1000-entry ceiling must set fileCountExceeded")
	}
	if quotaExceeded.Load() {
		t.Error("a file-count-only breach must not set quotaExceeded — the two reasons must stay distinct")
	}
}

// TestCheckGrowthCeiling_FileCount_NoBreach: entry-count growth within the
// ceiling leaves fileCountExceeded false.
func TestCheckGrowthCeiling_FileCount_NoBreach(t *testing.T) {
	var quotaExceeded, fileCountExceeded atomic.Bool
	base := progressSnapshot{fsCount: 10}
	fingerprint := func() progressSnapshot { return progressSnapshot{fsCount: 10 + 50} }

	checkGrowthCeiling(fingerprint, base, 0, 1000, &quotaExceeded, &fileCountExceeded)
	if fileCountExceeded.Load() {
		t.Error("entry-count growth within the ceiling must not set fileCountExceeded")
	}
}

// TestCheckGrowthCeiling_FileCountAlreadyExceededSkipsResample: once a tick
// has already caught the file-count breach (fileCountExceeded already
// true) and the byte-size ceiling is disabled, the post-run check must be
// a no-op — no second filesystem walk.
func TestCheckGrowthCeiling_FileCountAlreadyExceededSkipsResample(t *testing.T) {
	var quotaExceeded, fileCountExceeded atomic.Bool
	fileCountExceeded.Store(true)
	called := false
	fingerprint := func() progressSnapshot { called = true; return progressSnapshot{fsCount: 999999} }

	checkGrowthCeiling(fingerprint, progressSnapshot{}, 0, 1000, &quotaExceeded, &fileCountExceeded)
	if called {
		t.Error("checkGrowthCeiling must not re-sample when fileCountExceeded is already true and the byte-size ceiling is disabled")
	}
}

// TestCheckGrowthCeiling_BothCeilingsIndependentBreaches: when BOTH
// ceilings are active and only ONE has actually breached, exactly that
// one's flag is set — the two must never be conflated. This is the direct
// mutation-check for "collapsing the count reason into the size reason":
// a mutant that ORs the two conditions together (or shares one flag)
// fails this test.
func TestCheckGrowthCeiling_BothCeilingsIndependentBreaches(t *testing.T) {
	t.Run("only file count breaches", func(t *testing.T) {
		var quotaExceeded, fileCountExceeded atomic.Bool
		base := progressSnapshot{fsSize: 100, fsCount: 10}
		fingerprint := func() progressSnapshot { return progressSnapshot{fsSize: 150, fsCount: 10 + 5000} }

		checkGrowthCeiling(fingerprint, base, 1_000_000, 1000, &quotaExceeded, &fileCountExceeded)
		if quotaExceeded.Load() {
			t.Error("quotaExceeded must stay false when only the file-count ceiling breached")
		}
		if !fileCountExceeded.Load() {
			t.Error("fileCountExceeded must be set")
		}
	})
	t.Run("only byte size breaches", func(t *testing.T) {
		var quotaExceeded, fileCountExceeded atomic.Bool
		base := progressSnapshot{fsSize: 100, fsCount: 10}
		fingerprint := func() progressSnapshot { return progressSnapshot{fsSize: 100 + 5_000_000, fsCount: 15} }

		checkGrowthCeiling(fingerprint, base, 1000, 1_000_000, &quotaExceeded, &fileCountExceeded)
		if !quotaExceeded.Load() {
			t.Error("quotaExceeded must be set")
		}
		if fileCountExceeded.Load() {
			t.Error("fileCountExceeded must stay false when only the byte-size ceiling breached")
		}
	})
}

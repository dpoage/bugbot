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
	var killed atomic.Bool
	cancelled := make(chan struct{})
	done := make(chan struct{})
	defer close(done)

	noProgress := func() progressSnapshot { return progressSnapshot{} }
	go watchIdle(done, noProgress, nil, 40*time.Millisecond, 5*time.Millisecond, &killed, func() { close(cancelled) })

	select {
	case <-cancelled:
		if !killed.Load() {
			t.Error("killed must be set before cancel is invoked")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchIdle did not fire on a stalled run")
	}
}

// TestWatchIdleCPUKeepsAlive: a run with no output/fs change but a busy CPU
// (activeFallback returns true) is treated as making progress and never killed.
func TestWatchIdleCPUKeepsAlive(t *testing.T) {
	var killed atomic.Bool
	done := make(chan struct{})

	flat := func() progressSnapshot { return progressSnapshot{} } // no output, no fs change
	cpuBusy := func() bool { return true }                        // but the container is churning
	go watchIdle(done, flat, cpuBusy, 40*time.Millisecond, 5*time.Millisecond, &killed, func() { killed.Store(true) })

	time.Sleep(250 * time.Millisecond)
	close(done)
	if killed.Load() {
		t.Error("watchIdle fired despite the container being CPU-busy")
	}
}

// TestWatchIdleNoFireWhenProgressing: continuous progress keeps resetting the
// clock, so the watchdog never cancels.
func TestWatchIdleNoFireWhenProgressing(t *testing.T) {
	var killed atomic.Bool
	var n atomic.Int64
	done := make(chan struct{})

	progressing := func() progressSnapshot { return progressSnapshot{outputBytes: n.Add(1)} }
	go watchIdle(done, progressing, nil, 40*time.Millisecond, 5*time.Millisecond, &killed, func() { killed.Store(true) })

	time.Sleep(250 * time.Millisecond) // ~50 polls, each shows fresh progress
	close(done)
	if killed.Load() {
		t.Error("watchIdle fired despite continuous progress")
	}
}

// TestWatchIdleDisabled: idleTimeout <= 0 returns immediately and never fires.
func TestWatchIdleDisabled(t *testing.T) {
	var killed atomic.Bool
	done := make(chan struct{})
	defer close(done)
	returned := make(chan struct{})

	go func() {
		watchIdle(done, func() progressSnapshot { return progressSnapshot{} }, nil, 0, time.Millisecond, &killed, func() { killed.Store(true) })
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("watchIdle with idleTimeout<=0 must return immediately")
	}
	if killed.Load() {
		t.Error("disabled watchdog must never fire")
	}
}

// TestWatchIdleStopsOnDone: closing done returns the watchdog without firing,
// even when the idle window has not elapsed.
func TestWatchIdleStopsOnDone(t *testing.T) {
	var killed atomic.Bool
	done := make(chan struct{})
	returned := make(chan struct{})

	go func() {
		watchIdle(done, func() progressSnapshot { return progressSnapshot{} }, nil, time.Hour, 5*time.Millisecond, &killed, func() { killed.Store(true) })
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

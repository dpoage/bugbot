package progress

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestLogRenderer_SignificantLines(t *testing.T) {
	var buf bytes.Buffer
	r := NewLogRenderer(&buf)

	r.Handle(Event{Kind: KindScanStarted, ScanKind: "sweep", Commit: "abcdef1234567890"})
	r.Handle(Event{Kind: KindStageFinished, Stage: StageVerify, Counts: &Counts{Hypothesized: 4, Triaged: 3, Verified: 2, Killed: 1}})
	r.Handle(Event{Kind: KindAgentFinished, Role: RoleFinder, Label: "lensA", Tokens: 900, Duration: 1500 * time.Millisecond})
	r.Handle(Event{Kind: KindFindingVerified, Title: "real bug", File: "a.go", Line: 7})
	r.Handle(Event{Kind: KindScanFinished, ScanKind: "sweep", Commit: "abcdef1234567890", Counts: &Counts{Verified: 2}, InputTokens: 100, OutputTokens: 50, CacheReadTokens: 40})

	out := buf.String()
	for _, want := range []string{
		"scan started: kind=sweep commit=abcdef123456",
		"stage done: verify hypothesized=4 triaged=3 verified=2 killed=1",
		"agent done: finder [lensA] tokens=900 dur=1.5s",
		"verified: real bug (a.go:7)",
		"scan finished: kind=sweep",
		"spend in=100 out=50 cached=40",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q\n---\n%s", want, out)
		}
	}
}

func TestLogRenderer_SuppressesNoise(t *testing.T) {
	var buf bytes.Buffer
	r := NewLogRenderer(&buf)

	// These events are deliberately not logged as lines (too high-frequency /
	// low-information for a plain log tail).
	r.Handle(Event{Kind: KindAgentStarted, Role: RoleFinder, Label: "lensA"})
	r.Handle(Event{Kind: KindSpendTick, InputTokens: 10, OutputTokens: 5})
	r.Handle(Event{Kind: KindReverify, Count: 0})
	r.Handle(Event{Kind: KindPromote, Count: 0})

	if buf.Len() != 0 {
		t.Errorf("expected no output for noise events, got:\n%s", buf.String())
	}
}

func TestLogRenderer_DaemonEventLines(t *testing.T) {
	var buf bytes.Buffer
	r := NewLogRenderer(&buf)

	r.Handle(Event{Kind: KindCycleStarted, ScanKind: "targeted"})
	r.Handle(Event{Kind: KindReverify, Count: 3})
	r.Handle(Event{Kind: KindPromote, Count: 1})
	r.Handle(Event{Kind: KindCycleFinished, ScanKind: "targeted", Count: 2, InputTokens: 7, OutputTokens: 3})

	out := buf.String()
	for _, want := range []string{
		"cycle started: kind=targeted",
		"re-verify: 3 finding(s) auto-closed",
		"promote: 1 finding(s) promoted to T1",
		"cycle finished: kind=targeted new=2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

func TestLogRenderer_CycleScheduledWithBacklog(t *testing.T) {
	t0 := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 6, 10, 15, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 6, 10, 16, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	r := NewLogRenderer(&buf)

	// With NextBacklog set (repro enabled): all three times appear.
	r.Handle(Event{Kind: KindCycleScheduled, NextPoll: t0, NextSweep: t1, NextBacklog: t2})
	out := buf.String()
	for _, want := range []string{"next_poll=14:00:00", "next_sweep=15:00:00", "next_backlog=16:00:00"} {
		if !strings.Contains(out, want) {
			t.Errorf("schedule line with backlog missing %q:\n%s", want, out)
		}
	}

	buf.Reset()
	// Without NextBacklog (repro disabled / zero time): field is omitted.
	r.Handle(Event{Kind: KindCycleScheduled, NextPoll: t0, NextSweep: t1})
	out = buf.String()
	if strings.Contains(out, "next_backlog") {
		t.Errorf("schedule line without backlog must not include next_backlog:\n%s", out)
	}
	if !strings.Contains(out, "next_poll=14:00:00") {
		t.Errorf("schedule line missing next_poll:\n%s", out)
	}
}

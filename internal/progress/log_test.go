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
	r.Handle(Event{Kind: KindScanFinished, ScanKind: "sweep", Commit: "abcdef1234567890", Counts: &Counts{Verified: 2}, InputTokens: 100, OutputTokens: 50})

	out := buf.String()
	for _, want := range []string{
		"scan started: kind=sweep commit=abcdef123456",
		"stage done: verify hypothesized=4 triaged=3 verified=2 killed=1",
		"agent done: finder [lensA] tokens=900 dur=1.5s",
		"verified: real bug (a.go:7)",
		"scan finished: kind=sweep",
		"spend in=100 out=50",
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

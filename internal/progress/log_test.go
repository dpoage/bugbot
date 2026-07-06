package progress

import (
	"bytes"
	"log/slog"
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

// TestLogRenderer_AgentActivity_ReproducerOnly verifies KindAgentActivity
// renders a line for the low-volume reproducer/patch-prover roles but stays
// suppressed for the finder role, which runs in large parallel batches and
// would flood a plain log tail with one line per tool-call turn.
func TestLogRenderer_AgentActivity_ReproducerOnly(t *testing.T) {
	var buf bytes.Buffer
	r := NewLogRenderer(&buf)

	r.Handle(Event{Kind: KindAgentActivity, Role: RoleFinder, Label: "lensA", Activity: "reading main.go"})
	if buf.Len() != 0 {
		t.Fatalf("finder activity must be suppressed, got:\n%s", buf.String())
	}

	r.Handle(Event{Kind: KindAgentActivity, Role: RoleReproducer, Label: "nil deref", Activity: "reading handler.go"})
	out := buf.String()
	if !strings.Contains(out, "agent: reproducer [nil deref] reading handler.go") {
		t.Errorf("expected reproducer activity line, got:\n%s", out)
	}

	buf.Reset()
	r.Handle(Event{Kind: KindAgentActivity, Role: RolePatchProver, Label: "nil deref", Activity: "running suite"})
	out = buf.String()
	if !strings.Contains(out, "agent: patch-prover [nil deref] running suite") {
		t.Errorf("expected patch-prover activity line, got:\n%s", out)
	}
}

// TestLogRenderer_ReproAttempt renders a KindReproAttempt as one line
// carrying the round number, cap, verdict, duration, and finding label.
func TestLogRenderer_ReproAttempt(t *testing.T) {
	var buf bytes.Buffer
	r := NewLogRenderer(&buf)

	r.Handle(Event{
		Kind: KindReproAttempt, Role: RoleReproducer, Label: "nil deref",
		Attempt: 1, MaxAttempts: 2, Verdict: "exit_zero", Duration: 3500 * time.Millisecond,
	})

	out := buf.String()
	if !strings.Contains(out, "repro attempt 1/2: exit_zero") {
		t.Errorf("expected attempt/verdict in line, got:\n%s", out)
	}
	if !strings.Contains(out, "dur=3.5s") {
		t.Errorf("expected duration in line, got:\n%s", out)
	}
	if !strings.Contains(out, "nil deref") {
		t.Errorf("expected finding label in line, got:\n%s", out)
	}
}

// TestLogRenderer_HandleSlog_AgentActivityAndReproAttempt verifies the slog
// bridge (NewSlogRenderer) mirrors the plain-line renderer's role filter for
// KindAgentActivity (reproducer/patch-prover only, finder suppressed) and
// emits a record for KindReproAttempt.
func TestLogRenderer_HandleSlog_AgentActivityAndReproAttempt(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	r := NewSlogRenderer(log)

	r.Handle(Event{Kind: KindAgentActivity, Role: RoleFinder, Label: "lensA", Activity: "reading main.go"})
	if buf.Len() != 0 {
		t.Fatalf("finder activity must be suppressed in slog, got:\n%s", buf.String())
	}

	r.Handle(Event{Kind: KindAgentActivity, Role: RoleReproducer, Label: "nil deref", Activity: "reading handler.go"})
	out := buf.String()
	if !strings.Contains(out, "progress: agent activity") || !strings.Contains(out, "reading handler.go") {
		t.Errorf("expected reproducer activity slog record, got:\n%s", out)
	}

	buf.Reset()
	r.Handle(Event{
		Kind: KindReproAttempt, Role: RoleReproducer, Label: "nil deref",
		Attempt: 1, MaxAttempts: 2, Verdict: "exit_zero", Duration: 3500 * time.Millisecond,
	})
	out = buf.String()
	if !strings.Contains(out, "progress: repro attempt") {
		t.Errorf("expected repro attempt slog record, got:\n%s", out)
	}
	for _, want := range []string{"attempt=1", "max_attempts=2", "verdict=exit_zero", "\"nil deref\""} {
		if !strings.Contains(out, want) {
			t.Errorf("slog record missing %q, got:\n%s", want, out)
		}
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

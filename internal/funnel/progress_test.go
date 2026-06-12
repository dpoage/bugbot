package funnel

import (
	"context"
	"sync"
	"testing"

	"github.com/dpoage/bugbot/internal/progress"
)

// captureSink records every progress event in arrival order. Safe for the
// parallel emission the funnel does from finder/verifier goroutines.
type captureSink struct {
	mu     sync.Mutex
	events []progress.Event
}

func (c *captureSink) Handle(ev progress.Event) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

func (c *captureSink) kinds() []progress.Kind {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]progress.Kind, len(c.events))
	for i, e := range c.events {
		out[i] = e.Kind
	}
	return out
}

func (c *captureSink) byKind(k progress.Kind) []progress.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []progress.Event
	for _, e := range c.events {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

// TestSweep_EmitsProgressInStageOrder runs the same one-real/one-bogus fixture
// as the E2E test but with a progress sink attached, and asserts the event
// stream brackets the run, visits the four stages in pipeline order with correct
// counts, and reports the surviving finding.
func TestSweep_EmitsProgressInStageOrder(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := finderOnNilLens(newScriptedClient())
	verifier := verifierRouting(newScriptedClient())

	sink := &captureSink{}
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{Progress: sink})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Sweep(ctx); err != nil {
		t.Fatal(err)
	}

	kinds := sink.kinds()
	if len(kinds) == 0 {
		t.Fatal("no progress events emitted")
	}
	// KindSweepSummary and KindHeatOrdered may precede KindScanStarted;
	// find the first event that is neither of those and assert it is scan_started.
	firstSignificant := kinds[0]
	for _, k := range kinds {
		if k != progress.KindSweepSummary && k != progress.KindHeatOrdered {
			firstSignificant = k
			break
		}
	}
	if firstSignificant != progress.KindScanStarted {
		t.Errorf("first significant event = %q, want scan_started", firstSignificant)
	}
	if last := kinds[len(kinds)-1]; last != progress.KindScanFinished {
		t.Errorf("last event = %q, want scan_finished", last)
	}

	// Stage-finished events must arrive in pipeline order.
	wantStages := []string{
		progress.StageHypothesize, progress.StageTriage,
		progress.StageVerify, progress.StagePersist,
	}
	var gotStages []string
	for _, e := range sink.byKind(progress.KindStageFinished) {
		gotStages = append(gotStages, e.Stage)
	}
	if len(gotStages) != len(wantStages) {
		t.Fatalf("stage_finished count = %d (%v), want %d", len(gotStages), gotStages, len(wantStages))
	}
	for i, want := range wantStages {
		if gotStages[i] != want {
			t.Errorf("stage_finished[%d] = %q, want %q", i, gotStages[i], want)
		}
	}

	// Counts: hypothesize sees 2 candidates; verify reports 1 verified, 1 killed.
	hyp := sink.byKind(progress.KindStageFinished)[0]
	if hyp.Counts == nil || hyp.Counts.Hypothesized != 2 {
		t.Errorf("hypothesize counts = %+v, want hypothesized=2", hyp.Counts)
	}
	verifyEv := lastStageFinished(sink, progress.StageVerify)
	if verifyEv.Counts == nil || verifyEv.Counts.Verified != 1 || verifyEv.Counts.Killed != 1 {
		t.Errorf("verify counts = %+v, want verified=1 killed=1", verifyEv.Counts)
	}

	// Agents: at least one finder run and one verifier run, each bracketed.
	if got := len(sink.byKind(progress.KindAgentStarted)); got == 0 {
		t.Error("no agent_started events")
	}
	if got := len(sink.byKind(progress.KindAgentFinished)); got == 0 {
		t.Error("no agent_finished events")
	}

	// The surviving real finding is reported.
	verified := sink.byKind(progress.KindFindingVerified)
	if len(verified) != 1 {
		t.Fatalf("finding_verified count = %d, want 1", len(verified))
	}
	if verified[0].File != "bug.go" || verified[0].Line != 10 {
		t.Errorf("verified finding anchor = %s:%d, want bug.go:10", verified[0].File, verified[0].Line)
	}

	// Spend ticks carry a monotonic-ish cumulative total that ends non-zero.
	ticks := sink.byKind(progress.KindSpendTick)
	if len(ticks) == 0 {
		t.Fatal("no spend_tick events")
	}
	last := ticks[len(ticks)-1]
	if last.InputTokens == 0 || last.OutputTokens == 0 {
		t.Errorf("final spend tick = in:%d out:%d, want both non-zero", last.InputTokens, last.OutputTokens)
	}
	if last.CacheReadTokens == 0 {
		t.Errorf("final spend tick carries no cache reads; cumulative cache total not threaded through")
	}
}

// lastStageFinished returns the last stage_finished event for the named stage.
func lastStageFinished(c *captureSink, stage string) progress.Event {
	var found progress.Event
	for _, e := range c.byKind(progress.KindStageFinished) {
		if e.Stage == stage {
			found = e
		}
	}
	return found
}

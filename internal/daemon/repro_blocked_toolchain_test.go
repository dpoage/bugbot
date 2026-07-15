package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/progress"
)

// capturingSink records every progress.Event it receives. Safe for
// concurrent use.
type capturingSink struct {
	mu     sync.Mutex
	events []progress.Event
}

func (s *capturingSink) Handle(ev progress.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func (s *capturingSink) reproBlockedEvents() []progress.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []progress.Event
	for _, ev := range s.events {
		if ev.Kind == progress.KindReproBlocked {
			out = append(out, ev)
		}
	}
	return out
}

// blockedToolchainTestConfig is a minimal DaemonConfig for the tests in this
// file: long intervals so the (unused) scheduler loop never fires, generous
// token budgets so nothing gates the direct method calls under test.
func blockedToolchainTestConfig() DaemonConfig {
	return DaemonConfig{
		PollInterval:   time.Hour,
		SweepInterval:  time.Hour,
		PerCycleTokens: 1_000_000,
		PerDayTokens:   10_000_000,
	}
}

// TestPromoteNewFindings_EmitsBlockedToolchainAggregate is the daemon-side
// half of bugbot-14g0 acceptance 2's producer requirement: the post-cycle
// promotion step (the unattended daemon's primary repro path) must emit the
// blocked-toolchain aggregate into the daemon's progress sink, not just leave
// it sitting unreported in the store. Uses a fakePromoter (no real sandbox)
// and seeds the queue row directly, mirroring what a real claim-time gate
// would have left behind on a prior cycle.
func TestPromoteNewFindings_EmitsBlockedToolchainAggregate(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	sink := &capturingSink{}

	// Seed a blocked_toolchain row directly — this is what a real claim-time
	// gate (repro.promoteOne) would have left on a prior cycle; the daemon's
	// aggregate emission reads persisted state, not this cycle's own summary.
	if _, err := st.EnqueueRepro(ctx, "fp-blocked-1"); err != nil {
		t.Fatalf("EnqueueRepro: %v", err)
	}
	if _, err := st.BlockReproAttemptOnToolchain(ctx, "fp-blocked-1", "js"); err != nil {
		t.Fatalf("BlockReproAttemptOnToolchain: %v", err)
	}

	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	fr.commit("init")

	llmc := newFakeLLM(candidateJSON, notRefutedJSON)
	d, err := New(Deps{
		Repo:       fr.open(),
		Store:      st,
		Clients:    funnel.RoleClients{Finder: llmc, Verifier: llmc},
		Reproducer: &fakePromoter{},
		FunnelOpts: funnel.Options{Limits: funnel.StageLimits{Refuters: 1, MaxParallel: 2}},
		Logger:     discardLogger(),
		Progress:   sink,
	}, blockedToolchainTestConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.cfg.EnableRepro = true

	promoted := d.promoteNewFindings(ctx, &funnel.Result{
		ScanRunID: "run-1",
		Findings: []domain.Finding{
			{ID: "a", Fingerprint: "fp-new-1", Tier: domain.TierVerified, Status: domain.StatusOpen},
		},
	})
	if promoted != 0 {
		t.Fatalf("fakePromoter promotes nothing; want 0, got %d", promoted)
	}

	events := sink.reproBlockedEvents()
	if len(events) != 1 {
		t.Fatalf("want 1 repro_blocked event, got %d: %+v", len(events), events)
	}
	if events[0].Label != "js" || events[0].Count != 1 {
		t.Errorf("event = %+v, want Label=js Count=1", events[0])
	}
	if events[0].Message == "" {
		t.Error("event Message should be a human-readable summary")
	}
}

// TestRunReproBacklog_EmitsBlockedToolchainAggregate is the same check for
// the daemon's OTHER PromoteAll call site: the periodic backlog drain.
func TestRunReproBacklog_EmitsBlockedToolchainAggregate(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	sink := &capturingSink{}

	if _, err := st.EnqueueRepro(ctx, "fp-blocked-2"); err != nil {
		t.Fatalf("EnqueueRepro: %v", err)
	}
	if _, err := st.BlockReproAttemptOnToolchain(ctx, "fp-blocked-2", "python"); err != nil {
		t.Fatalf("BlockReproAttemptOnToolchain: %v", err)
	}
	// runReproBacklog only fires when OpenBacklog returns at least one
	// eligible finding (an empty backlog returns early, before PromoteAll —
	// see backlog.go), so seed one open T2 finding with no repro attempt.
	if _, err := st.UpsertFinding(ctx, domain.Finding{
		Fingerprint: "fp-eligible-1",
		Title:       "seed finding",
		Tier:        domain.TierVerified,
		Status:      domain.StatusOpen,
		File:        "main.go",
		Line:        1,
	}); err != nil {
		t.Fatalf("UpsertFinding: %v", err)
	}

	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	fr.commit("init")

	llmc := newFakeLLM(candidateJSON, notRefutedJSON)
	d, err := New(Deps{
		Repo:       fr.open(),
		Store:      st,
		Clients:    funnel.RoleClients{Finder: llmc, Verifier: llmc},
		Reproducer: &fakePromoter{},
		FunnelOpts: funnel.Options{Limits: funnel.StageLimits{Refuters: 1, MaxParallel: 2}},
		Logger:     discardLogger(),
		Progress:   sink,
	}, blockedToolchainTestConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.cfg.EnableRepro = true

	d.runReproBacklog(ctx)

	events := sink.reproBlockedEvents()
	if len(events) != 1 {
		t.Fatalf("want 1 repro_blocked event, got %d: %+v", len(events), events)
	}
	if events[0].Label != "python" || events[0].Count != 1 {
		t.Errorf("event = %+v, want Label=python Count=1", events[0])
	}
}

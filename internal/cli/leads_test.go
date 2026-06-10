package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/store"
)

// seedLead writes a lead directly into the store backing cfgPath's config.
func seedLead(t *testing.T, st *store.Store, target, file string, line int, note string, consume bool) {
	t.Helper()
	ctx := context.Background()
	l := store.Lead{PosterLens: "nil-safety/error-handling", TargetLens: target, File: file, Line: line, Note: note}
	if err := st.AddLead(ctx, l); err != nil {
		t.Fatal(err)
	}
	if consume {
		got, err := st.ListLeads(ctx, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range got {
			if g.File == file && g.Line == line {
				if err := st.ConsumeLeads(ctx, []string{g.ID}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// TestLeadsCommand covers the full listing, the --pending filter, and the
// friendly empty output, through the real root command.
func TestLeadsCommand(t *testing.T) {
	cfgPath, _, _ := setup(t)

	// Empty board first.
	out, err := run(t, cfgPath, "leads")
	if err != nil {
		t.Fatalf("leads errored: %v", err)
	}
	if !strings.Contains(out, "blackboard: empty") {
		t.Errorf("empty board output wrong:\n%s", out)
	}

	// Seed one pending + one consumed lead. setup() closed its store handle, so
	// reopen the same path.
	ctx := context.Background()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, cfg.Storage.Path)
	if err != nil {
		t.Fatal(err)
	}
	seedLead(t, st, "concurrency", "ingest.go", 31, "unsynchronized map write under concurrent ingest", false)
	seedLead(t, st, "resource-leaks", "ingest.go", 26, "scanner error path skips Close", true)
	_ = st.Close()

	out, err = run(t, cfgPath, "leads")
	if err != nil {
		t.Fatalf("leads errored: %v", err)
	}
	for _, want := range []string{
		"2 lead(s), 1 pending",
		"[PENDING] nil-safety/error-handling -> concurrency",
		"ingest.go:31",
		"[consumed] nil-safety/error-handling -> resource-leaks",
		", consumed ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("leads output missing %q\n---\n%s", want, out)
		}
	}

	out, err = run(t, cfgPath, "leads", "--pending")
	if err != nil {
		t.Fatalf("leads --pending errored: %v", err)
	}
	if strings.Contains(out, "resource-leaks") {
		t.Errorf("--pending must hide consumed leads:\n%s", out)
	}
	if !strings.Contains(out, "concurrency") {
		t.Errorf("--pending must keep pending leads:\n%s", out)
	}
}

// TestRenderWorldState covers the pure renderer over a fully-populated state.
func TestRenderWorldState(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	ws := worldState{
		HasTallies: true,
		Tallies: store.FindingTallies{
			OpenByTier: map[int]int{0: 1, 2: 3},
			NeedsHuman: 2, Fixed: 4, Dismissed: 1,
		},
		PendingLeadsTotal: 5,
		PendingLeads: []store.Lead{
			{TargetLens: "concurrency", File: "a.go", Line: 7, Note: "shared map"},
		},
		Published:      map[string]int{"open": 2, "closed": 4},
		HasDaySpend:    true,
		DaySpend:       store.SpendTotals{InputTokens: 250_000, OutputTokens: 50_000},
		DayBudgetLimit: 1_000_000,
		HasLastRun:     true,
		LastRun: store.ScanRun{
			Kind: store.ScanTargeted, CommitSHA: "abcdef1234567890",
			StartedAt:  now.Add(-time.Hour),
			FinishedAt: now.Add(-50 * time.Minute),
		},
	}

	var b strings.Builder
	renderWorldState(&b, ws, now)
	out := b.String()
	for _, want := range []string{
		"open: T0=1 T2=3 | fixed=4 dismissed=1",
		"needs human:  2 finding(s)",
		"5 pending lead(s)",
		"-> concurrency: a.go:7 — shared map",
		"github sync:  issues open=2 closed=4",
		"spend today:  in=250000 out=50000 total=300000 tokens (30.0% of day budget)",
		"last run:     targeted commit=abcdef123456 finished 50m ago",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("world state missing %q\n---\n%s", want, out)
		}
	}
}

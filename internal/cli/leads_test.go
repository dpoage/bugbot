package cli

import (
	"context"
	"strings"
	"testing"
	"time"


	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/store"
)

// seedLead writes a pending lead directly into the store. It is a test
// helper, not a fixture for a particular run.
func seedLead(t *testing.T, st *store.Store, target, file string, line int, note string) {
	t.Helper()
	ctx := context.Background()
	l := store.Lead{PosterLens: "nil-safety/error-handling", TargetLens: target, File: file, Line: line, Note: note}
	if err := st.AddLead(ctx, l); err != nil {
		t.Fatal(err)
	}
}

// TestLeadsCommand covers the empty-output path, the pending-only listing
// (every row is pending now — consumed leads are deleted at claim time), and
// the consume-then-vanish path that is the user-visible behavior of
// delete-on-consume.
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

	// Reopen the store to seed rows. setup() closed its handle.
	ctx := context.Background()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, cfg.Storage.Path)
	if err != nil {
		t.Fatal(err)
	}

	// Two pending leads. Both must show up.
	seedLead(t, st, "concurrency", "ingest.go", 31, "unsynchronized map write under concurrent ingest")
	seedLead(t, st, "resource-leaks", "ingest.go", 26, "scanner error path skips Close")

	out, err = run(t, cfgPath, "leads")
	if err != nil {
		t.Fatalf("leads errored: %v", err)
	}
	for _, want := range []string{
		"2 pending lead(s)",
		"[PENDING] nil-safety/error-handling -> concurrency",
		"ingest.go:31",
		"[PENDING] nil-safety/error-handling -> resource-leaks",
		"ingest.go:26",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("leads output missing %q\n---\n%s", want, out)
		}
	}
	// RenderLeads no longer produces a 'consumed' history section.
	if strings.Contains(out, "[consumed]") {
		t.Errorf("leads output should not render a consumed history:\n%s", out)
	}
	if strings.Contains(out, ", consumed ") {
		t.Errorf("leads output should not print a 'consumed …' timestamp:\n%s", out)
	}

	// Claim the concurrency lead (delete-on-consume): the listing must no
	// longer surface it, and the resource-leaks lead must still be there.
	all, err := st.ListLeads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var concurrencyID string
	for _, l := range all {
		if l.TargetLens == "concurrency" {
			concurrencyID = l.ID
			break
		}
	}
	if concurrencyID == "" {
		t.Fatal("seeded concurrency lead missing before consume")
	}
	if err := st.ConsumeLeads(ctx, []string{concurrencyID}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	out, err = run(t, cfgPath, "leads")
	if err != nil {
		t.Fatalf("leads errored: %v", err)
	}
	if strings.Contains(out, "-> concurrency") {
		t.Errorf("consumed lead must be gone from `bugbot leads`:\n%s", out)
	}
	if !strings.Contains(out, "-> resource-leaks") {
		t.Errorf("resource-leaks lead (different lens) must still be listed:\n%s", out)
	}
	if !strings.Contains(out, "1 pending lead(s)") {
		t.Errorf("leads output should report 1 surviving pending lead:\n%s", out)
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

// TestTruncateNote_UTF8Safe moved to internal/util/util_test.go's
// TestTruncateRunes/multibyte-rune-boundary after the helper was lifted
// out of cli. The pinning coverage (50 runes truncated to 10 must remain
// valid UTF-8 with 11 runes / ellipsis) is preserved there.

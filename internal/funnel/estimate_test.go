package funnel

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/store"
)

// TestEstimateScan_PredictsRunFinderUnits is the anti-drift test: the estimate's
// exact finder-unit count must equal the number of finder agents an actual
// unlimited-budget Sweep launches (Stats.FinderRuns). With no prior runs the
// projection falls back to labeled priors.
func TestEstimateScan_PredictsRunFinderUnits(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	finder := newScriptedClient()
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	est, err := f.EstimateScan(ctx, store.ScanOneshot, nil)
	if err != nil {
		t.Fatalf("EstimateScan: %v", err)
	}
	if est.Files == 0 {
		t.Fatalf("estimate Files = 0, want > 0 (fixture has tracked files)")
	}
	if est.FinderUnits == 0 {
		t.Fatalf("estimate FinderUnits = 0, want > 0")
	}

	// No finished runs yet → priors, with provenance flagged and no duration.
	if est.Calibrated {
		t.Errorf("Calibrated = true, want false (no history)")
	}
	if est.TokensPerUnit != defaultEstTokensPerUnit {
		t.Errorf("TokensPerUnit = %v, want prior %v", est.TokensPerUnit, defaultEstTokensPerUnit)
	}
	wantPrior := int64(math.Round(float64(est.FinderUnits) * defaultEstTokensPerUnit))
	if est.EstTokens != wantPrior {
		t.Errorf("EstTokens = %d, want %d (units × prior)", est.EstTokens, wantPrior)
	}
	if est.EstDuration != 0 {
		t.Errorf("EstDuration = %v, want 0 (no throughput history)", est.EstDuration)
	}

	// The actual run must launch exactly FinderUnits agents (unlimited budget =
	// no degradation), proving the estimate matches reality.
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Stats.FinderRuns != est.FinderUnits {
		t.Errorf("estimate FinderUnits = %d, but Sweep launched FinderRuns = %d",
			est.FinderUnits, res.Stats.FinderRuns)
	}
}

// TestEstimateScan_CalibratesFromHistory pins the projection math: a single
// finished run of known per-unit cost calibrates tokens/unit and throughput, so
// the projected tokens equal FinderUnits × per-unit and the duration is finite.
func TestEstimateScan_CalibratesFromHistory(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	// Seed one finished run: 10 finder units, 1,000,000 total tokens, a
	// measurable wall (sleep) so throughput is positive.
	id, err := st.BeginScanRun(ctx, store.ScanOneshot, "seed")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)
	blob, _ := json.Marshal(Stats{FinderRuns: 10, InputTokens: 900_000, OutputTokens: 100_000})
	if err := st.FinishScanRun(ctx, id, string(blob)); err != nil {
		t.Fatal(err)
	}

	est, err := f.EstimateScan(ctx, store.ScanOneshot, nil)
	if err != nil {
		t.Fatalf("EstimateScan: %v", err)
	}
	if !est.Calibrated {
		t.Fatalf("Calibrated = false, want true (one finished run seeded)")
	}
	if est.SampleRuns != 1 {
		t.Errorf("SampleRuns = %d, want 1", est.SampleRuns)
	}
	const wantPerUnit = 1_000_000.0 / 10.0
	if est.TokensPerUnit != wantPerUnit {
		t.Errorf("TokensPerUnit = %v, want %v", est.TokensPerUnit, wantPerUnit)
	}
	wantTokens := int64(math.Round(float64(est.FinderUnits) * wantPerUnit))
	if est.EstTokens != wantTokens {
		t.Errorf("EstTokens = %d, want %d", est.EstTokens, wantTokens)
	}
	if est.ThroughputTokPerSec <= 0 {
		t.Errorf("ThroughputTokPerSec = %v, want > 0", est.ThroughputTokPerSec)
	}
	if est.EstDuration <= 0 {
		t.Errorf("EstDuration = %v, want > 0 (throughput is known)", est.EstDuration)
	}
}

// TestEstimateScan_DiffIntentOnlyWhenTargetedWithContext pins the diff-intent
// unit accounting: it is counted on a Targeted run with a ChangeContext and
// never on a Sweep.
func TestEstimateScan_DiffIntentOnlyWhenTargetedWithContext(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{
		ChangeContext: &ChangeContext{
			FromCommit:   "a",
			ToCommit:     "b",
			Message:      "tweak greeting",
			ChangedFiles: []string{"bug.go"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	targeted, err := f.EstimateScan(ctx, store.ScanTargeted, []string{"bug.go"})
	if err != nil {
		t.Fatalf("EstimateScan(targeted): %v", err)
	}
	if !targeted.DiffIntent {
		t.Errorf("targeted run with ChangeContext: DiffIntent = false, want true")
	}
	if targeted.FinderUnits < 1 {
		t.Errorf("targeted FinderUnits = %d, want >= 1", targeted.FinderUnits)
	}

	sweep, err := f.EstimateScan(ctx, store.ScanOneshot, nil)
	if err != nil {
		t.Fatalf("EstimateScan(sweep): %v", err)
	}
	if sweep.DiffIntent {
		t.Errorf("sweep run: DiffIntent = true, want false")
	}
}

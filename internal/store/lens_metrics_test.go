package store

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
)

// TestLensMetrics verifies acceptance criterion 2: per-lens survival rates are
// computable from stored data. The test seeds findings, dead hypotheses, and
// repro_attempts rows, then asserts the LensMetrics query returns correct stats.
func TestLensMetrics_SurvivalAndReproRates(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// --- seed "race" lens: 2 survived findings, 3 killed hypotheses,
	//     1 finding reproduced (T1), 0 contradicted ---
	race1 := sampleFinding()
	race1.Tier = 2
	race1.Status = domain.StatusOpen
	r1, _ := st.UpsertFinding(ctx, race1)

	race2 := sampleFinding()
	race2.Fingerprint = domain.Fingerprint("race", "internal/x/y.go", "99|race on map")
	race2.Line = 99
	race2.Tier = 1
	race2.ReproPath = "/repro/race2"
	race2.Status = domain.StatusOpen
	_, _ = st.UpsertFinding(ctx, race2)

	scanID, _ := st.BeginScanRun(ctx, ScanOneshot, "abc")
	for i := 0; i < 3; i++ {
		_ = st.AddDeadHypothesis(ctx, DeadHypothesis{
			ScanRunID:    scanID,
			Fingerprint:  domain.Fingerprint("race", "internal/x/y.go", "k"+string(rune('A'+i))),
			Lens:         "race",
			File:         "internal/x/y.go",
			Title:        "killed race candidate",
			RefutedCount: 2, TotalSeats: 2,
		})
	}

	// --- seed "nil-deref" lens: 1 survived finding, 1 killed, 1 contradicted ---
	nilF := sampleFinding()
	nilF.Fingerprint = domain.Fingerprint("nil-deref", "pkg/foo.go", "10|nil ptr")
	nilF.Lens = "nil-deref"
	nilF.File = "pkg/foo.go"
	nilF.Tier = 2
	nilF.Status = domain.StatusOpen
	nilStored, _ := st.UpsertFinding(ctx, nilF)
	_, _ = st.EnqueueRepro(ctx, nilStored.Fingerprint)
	_ = st.RecordExitZeroAttempt(ctx, nilStored.Fingerprint)
	_ = st.RecordExitZeroAttempt(ctx, nilStored.Fingerprint) // now contradicted

	_ = st.AddDeadHypothesis(ctx, DeadHypothesis{
		ScanRunID:    scanID,
		Fingerprint:  domain.Fingerprint("nil-deref", "pkg/foo.go", "killed-nil"),
		Lens:         "nil-deref",
		File:         "pkg/foo.go",
		Title:        "killed nil candidate",
		RefutedCount: 1, TotalSeats: 1,
	})

	// One exit-zero on race1 (NOT contradicted yet — only 1 attempt).
	_, _ = st.EnqueueRepro(ctx, r1.Fingerprint)
	_ = st.RecordExitZeroAttempt(ctx, r1.Fingerprint)

	lensStats, err := st.LensMetrics(ctx)
	if err != nil {
		t.Fatalf("LensMetrics: %v", err)
	}

	// Build a map for easy lookup.
	byLens := make(map[string]LensStat, len(lensStats))
	for _, ls := range lensStats {
		byLens[ls.Lens] = ls
	}

	t.Run("race_lens", func(t *testing.T) {
		ls, ok := byLens["race"]
		if !ok {
			t.Fatal("race lens missing from LensMetrics")
		}
		if ls.Survived != 2 {
			t.Errorf("Survived = %d, want 2", ls.Survived)
		}
		if ls.Killed != 3 {
			t.Errorf("Killed = %d, want 3", ls.Killed)
		}
		// Reprod = 1 (race2 is T1).
		if ls.Reprod != 1 {
			t.Errorf("Reprod = %d, want 1", ls.Reprod)
		}
		// ContradictedCount = 0 (race1 only had 1 exit-zero, below threshold).
		if ls.ContradictedCount != 0 {
			t.Errorf("ContradictedCount = %d, want 0", ls.ContradictedCount)
		}
		// SurvivalRate = 2/(2+3) = 0.4
		if got := ls.SurvivalRate(); got < 0.39 || got > 0.41 {
			t.Errorf("SurvivalRate = %.3f, want ~0.400", got)
		}
		// ReproRate = 1/2 = 0.5
		if got := ls.ReproRate(); got < 0.49 || got > 0.51 {
			t.Errorf("ReproRate = %.3f, want ~0.500", got)
		}
	})

	t.Run("nil-deref_lens", func(t *testing.T) {
		ls, ok := byLens["nil-deref"]
		if !ok {
			t.Fatal("nil-deref lens missing from LensMetrics")
		}
		if ls.Survived != 1 {
			t.Errorf("Survived = %d, want 1", ls.Survived)
		}
		if ls.Killed != 1 {
			t.Errorf("Killed = %d, want 1", ls.Killed)
		}
		if ls.ContradictedCount != 1 {
			t.Errorf("ContradictedCount = %d, want 1", ls.ContradictedCount)
		}
		// SurvivalRate = 1/(1+1) = 0.5
		if got := ls.SurvivalRate(); got < 0.49 || got > 0.51 {
			t.Errorf("SurvivalRate = %.3f, want ~0.500", got)
		}
	})
}

// TestLensMetrics_LensOnlyInDeadHypotheses verifies that a lens appearing only
// in dead_hypotheses (never survived) is still included in the output with
// Survived=0 and a correct survival rate of 0.
func TestLensMetrics_LensOnlyInDeadHypotheses(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	scanID, _ := st.BeginScanRun(ctx, ScanOneshot, "sha1")
	_ = st.AddDeadHypothesis(ctx, DeadHypothesis{
		ScanRunID:    scanID,
		Fingerprint:  domain.Fingerprint("ghost-lens", "foo.go", "ghost"),
		Lens:         "ghost-lens",
		File:         "foo.go",
		Title:        "always killed",
		RefutedCount: 2, TotalSeats: 2,
	})
	_ = st.AddDeadHypothesis(ctx, DeadHypothesis{
		ScanRunID:    scanID,
		Fingerprint:  domain.Fingerprint("ghost-lens", "foo.go", "ghost2"),
		Lens:         "ghost-lens",
		File:         "foo.go",
		Title:        "also killed",
		RefutedCount: 2, TotalSeats: 2,
	})

	lensStats, err := st.LensMetrics(ctx)
	if err != nil {
		t.Fatalf("LensMetrics: %v", err)
	}
	var found bool
	for _, ls := range lensStats {
		if ls.Lens == "ghost-lens" {
			found = true
			if ls.Survived != 0 {
				t.Errorf("Survived = %d, want 0", ls.Survived)
			}
			if ls.Killed != 2 {
				t.Errorf("Killed = %d, want 2", ls.Killed)
			}
			if ls.SurvivalRate() != 0 {
				t.Errorf("SurvivalRate = %.3f, want 0", ls.SurvivalRate())
			}
		}
	}
	if !found {
		t.Error("ghost-lens (only in dead_hypotheses) missing from LensMetrics")
	}
}

// TestLensMetrics_EmptyStore verifies LensMetrics returns empty slice (not error)
// when no data exists.
func TestLensMetrics_EmptyStore(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	stats, err := st.LensMetrics(ctx)
	if err != nil {
		t.Fatalf("LensMetrics on empty store: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected empty slice, got %d rows", len(stats))
	}
}

// TestLensMetrics_NonOpenFindingsExcluded pins that dismissed and fixed findings
// do not count toward Survived (they are resolved signal, not unresolved).
func TestLensMetrics_NonOpenFindingsExcluded(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	open := sampleFinding()
	open.Status = domain.StatusOpen
	open.Lens = "race"
	_, _ = st.UpsertFinding(ctx, open)

	dismissed := sampleFinding()
	dismissed.Fingerprint = domain.Fingerprint("race", "internal/x/y.go", "d1|dismissed")
	dismissed.Lens = "race"
	dismissed.Status = domain.StatusOpen // start open, dismiss below
	ds, _ := st.UpsertFinding(ctx, dismissed)
	_ = st.UpdateStatus(ctx, ds.Fingerprint, domain.StatusDismissed, "false positive")

	stats, err := st.LensMetrics(ctx)
	if err != nil {
		t.Fatalf("LensMetrics: %v", err)
	}
	for _, ls := range stats {
		if ls.Lens == "race" && ls.Survived != 1 {
			t.Errorf("Survived = %d, want 1 (dismissed finding must be excluded)", ls.Survived)
		}
	}
}

// TestLensMetrics_FixedFindingExcluded is a companion to the dismissed test for
// the domain.StatusFixed path.
func TestLensMetrics_FixedFindingExcluded(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	open := sampleFinding()
	open.Lens = "race"
	open.Tier = 1
	open.ReproPath = "/tmp/repro"
	stored, _ := st.UpsertFinding(ctx, open)
	_ = st.MarkFixed(ctx, stored.Fingerprint)

	// Another finding, still open: should be Survived=1.
	f2 := sampleFinding()
	f2.Fingerprint = domain.Fingerprint("race", "internal/x/y.go", "f2|race")
	f2.Lens = "race"
	f2.Status = domain.StatusOpen
	_, _ = st.UpsertFinding(ctx, f2)

	stats, err := st.LensMetrics(ctx)
	if err != nil {
		t.Fatalf("LensMetrics: %v", err)
	}
	for _, ls := range stats {
		if ls.Lens == "race" {
			if ls.Survived != 1 {
				t.Errorf("Survived = %d, want 1 (fixed finding excluded)", ls.Survived)
			}
		}
	}
}

// TestLensMetrics_ReproducedFindingNotContradicted pins that a finding which
// was successfully reproduced (ZeroExitZeroCount called) does not appear as
// contradicted in LensMetrics, even if it previously accumulated exit-zero counts.
func TestLensMetrics_ReproducedFindingNotContradicted(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	f.Lens = "race"
	f.Tier = 1
	f.ReproPath = "/repro/race"
	f.Status = domain.StatusOpen
	stored, _ := st.UpsertFinding(ctx, f)

	// Accumulate 2 exit-zero attempts (contradiction threshold reached).
	_, _ = st.EnqueueRepro(ctx, stored.Fingerprint)
	_ = st.RecordExitZeroAttempt(ctx, stored.Fingerprint)
	_ = st.RecordExitZeroAttempt(ctx, stored.Fingerprint)

	// Successful repro: zero the contradiction counter.
	_ = st.ZeroExitZeroCount(ctx, stored.Fingerprint)

	stats, err := st.LensMetrics(ctx)
	if err != nil {
		t.Fatalf("LensMetrics: %v", err)
	}
	for _, ls := range stats {
		if ls.Lens == "race" {
			if ls.ContradictedCount != 0 {
				t.Errorf("ContradictedCount = %d after successful repro, want 0 (reproduced findings must not be contradicted)", ls.ContradictedCount)
			}
			if ls.Reprod != 1 {
				t.Errorf("Reprod = %d, want 1 (T1 finding should count as reproduced)", ls.Reprod)
			}
		}
	}
}

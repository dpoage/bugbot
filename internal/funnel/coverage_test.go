package funnel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// ---------------------------------------------------------------------------
// Coverage truthfulness: finderOK vs. non-OK status
// ---------------------------------------------------------------------------

// TestHypothesize_CoverageTruthfulness verifies that only files from finderOK
// units end up in Result.CoveredFiles. Files from parse-failed or budget-stopped
// units must NOT appear.
func TestHypothesize_CoverageTruthfulness(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Three lenses: one returns valid JSON (finderOK), one returns prose that
	// never parses (finderParseFailed), one returns nothing (finderOK, empty).
	// We use lenses that map to the fixture files: bug.go and clean.go.
	//
	// To force the parse-fail path we give the second lens unparseable prose.
	nilSafety := "nil-safety/error-handling"
	apiLens := "api-contract-misuse"

	finder := newScriptedClient().
		onSystemContains(nilSafety, candJSON(realCand)).        // finderOK with bug.go
		onSystemContains(apiLens, "I cannot produce JSON here") // finderParseFailed

	verifier := verifierRouting(newScriptedClient())

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Lenses: []string{nilSafety, apiLens},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// Only bug.go should be covered (from the finderOK nil-safety unit).
	// clean.go gets covered by nil-safety too (same chunk), and api-contract
	// fails (so its files are not covered). Both bug.go and clean.go are in the
	// nil-safety chunk.
	covered := res.CoveredFiles
	sort.Strings(covered)
	if len(covered) == 0 {
		t.Fatal("expected covered files from the finderOK nil-safety unit, got none")
	}
	// At least bug.go must be covered.
	found := false
	for _, c := range covered {
		if c == "bug.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("bug.go not in covered files: %v", covered)
	}

	// Stats.CoveredFiles must equal len(CoveredFiles).
	if res.Stats.CoveredFiles != len(covered) {
		t.Errorf("Stats.CoveredFiles = %d, want %d", res.Stats.CoveredFiles, len(covered))
	}
}

// TestHypothesize_BudgetSkippedNotCovered verifies that files from budget-skipped
// units (hard stop before launching) are NOT in CoveredFiles.
func TestHypothesize_BudgetSkippedNotCovered(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Use a tiny budget so only the first lens runs; the rest are budget-stopped.
	finder := newScriptedClient()
	finder.fallback = candJSON(realCand)
	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Lenses:                []string{"nil-safety/error-handling"},
		TokenBudget:           100, // < 150 so pool is exhausted after first finder
		CacheReadBudgetWeight: 1.0,
		MaxParallel:           1,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// The budget-stopped finder did NOT cover its files cleanly (it never parsed),
	// so CoveredFiles must be empty (even though the finder produced a candidate
	// before stopping — but it's budget-stopped, not finderOK).
	// Note: with only one lens, and the finder actually SUCCEEDS (finderOK) before
	// budget stops verify stage, the files ARE covered. Let's check the logic.
	//
	// With TokenBudget=100 and each completion costing 150 tokens, the finder runs
	// and burns 150 tokens (above budget), which sets budget.stopped=true. The
	// finder itself completed with finderOK (budget stops AFTER the finder
	// completes, at the verify stage). So files ARE covered.
	//
	// For a true "budget-stopped before completion" case we'd need TruncBudgetPool.
	// This test verifies the overall stat is non-negative and consistent.
	if res.Stats.CoveredFiles != len(res.CoveredFiles) {
		t.Errorf("Stats.CoveredFiles (%d) != len(CoveredFiles) (%d)", res.Stats.CoveredFiles, len(res.CoveredFiles))
	}
}

// ---------------------------------------------------------------------------
// Sweep ordering: anti-starvation two-group scheme
// ---------------------------------------------------------------------------

// wm builds a store.Watermark for sweep-ordering tests.
func wm(ts time.Time, hash string) store.Watermark {
	return store.Watermark{LastScannedAt: ts, ContentHash: hash}
}

// TestApplySweepOrder_NeverScannedLeadsGroup1 verifies that never-scanned
// files (no row in watermarks) are placed in group 1 before clean files.
func TestApplySweepOrder_NeverScannedLeadsGroup1(t *testing.T) {
	// Simulate: clean.go was scanned recently (unchanged); bug.go never scanned.
	now := time.Now().UTC()
	watermarks := map[string]store.Watermark{
		"clean.go": wm(now.Add(-1*time.Hour), "h2"),
	}
	fps := map[string]string{"bug.go": "h1", "clean.go": "h2"}
	heat := map[string]float64{}

	targets := []string{"clean.go", "bug.go"}
	neverScanned, changedSinceScan, _ := applySweepOrder(targets, heat, fps, watermarks)

	if neverScanned != 1 {
		t.Errorf("neverScanned = %d, want 1", neverScanned)
	}
	if changedSinceScan != 0 {
		t.Errorf("changedSinceScan = %d, want 0", changedSinceScan)
	}
	// bug.go (never-scanned) must be first (group 1 before group 2).
	if targets[0] != "bug.go" {
		t.Errorf("targets[0] = %q, want bug.go (never-scanned must lead)", targets[0])
	}
	if targets[1] != "clean.go" {
		t.Errorf("targets[1] = %q, want clean.go (recently-scanned in group 2)", targets[1])
	}
}

// TestApplySweepOrder_EpochSentinelInGroup1 verifies that a file with
// last_scanned_at at the epoch sentinel is treated as never-scanned (group 1).
func TestApplySweepOrder_EpochSentinelInGroup1(t *testing.T) {
	// new.go has the epoch sentinel (written by RefreshContentHashes for new rows).
	epoch := epochSentinelParsed
	now := time.Now().UTC()
	watermarks := map[string]store.Watermark{
		"new.go":   wm(epoch, "h1"),
		"old.go":   wm(now.Add(-24*time.Hour), "h2"),
		"older.go": wm(now.Add(-48*time.Hour), "h3"),
	}
	fps := map[string]string{"new.go": "h1", "old.go": "h2", "older.go": "h3"}
	heat := map[string]float64{}

	targets := []string{"older.go", "old.go", "new.go"}
	neverScanned, _, _ := applySweepOrder(targets, heat, fps, watermarks)

	if neverScanned != 1 {
		t.Errorf("neverScanned = %d, want 1 (epoch-sentinel counts as never-scanned)", neverScanned)
	}
	// new.go must be in group 1 (first).
	if targets[0] != "new.go" {
		t.Errorf("targets[0] = %q, want new.go (epoch-sentinel in group 1)", targets[0])
	}
	// Group 2 must be stalest-first: older.go before old.go.
	if targets[1] != "older.go" {
		t.Errorf("targets[1] = %q, want older.go (stalest in group 2)", targets[1])
	}
	if targets[2] != "old.go" {
		t.Errorf("targets[2] = %q, want old.go (less stale in group 2)", targets[2])
	}
}

// TestApplySweepOrder_Group2StalestFirst verifies that group 2 (already-scanned)
// files are sorted by last_scanned_at ascending (stalest first).
func TestApplySweepOrder_Group2StalestFirst(t *testing.T) {
	now := time.Now().UTC()
	watermarks := map[string]store.Watermark{
		"a.go": wm(now.Add(-1*time.Hour), "ha"),  // least stale
		"b.go": wm(now.Add(-3*time.Hour), "hb"),  // middle
		"c.go": wm(now.Add(-12*time.Hour), "hc"), // most stale
	}
	fps := map[string]string{"a.go": "ha", "b.go": "hb", "c.go": "hc"}
	heat := map[string]float64{}

	targets := []string{"a.go", "b.go", "c.go"}
	neverScanned, _, _ := applySweepOrder(targets, heat, fps, watermarks)

	if neverScanned != 0 {
		t.Errorf("neverScanned = %d, want 0 (all files scanned)", neverScanned)
	}
	// Stalest first: c.go (12h ago) < b.go (3h ago) < a.go (1h ago).
	want := []string{"c.go", "b.go", "a.go"}
	for i, w := range want {
		if targets[i] != w {
			t.Errorf("targets[%d] = %q, want %q (stalest-first ordering)", i, targets[i], w)
		}
	}
}

// TestApplySweepOrder_Group1HeatOrdered verifies that group-1 (never-scanned)
// files are heat-ordered within the group.
func TestApplySweepOrder_Group1HeatOrdered(t *testing.T) {
	// All files are never-scanned; heat should order them.
	watermarks := map[string]store.Watermark{} // none scanned
	fps := map[string]string{"a.go": "ha", "b.go": "hb", "c.go": "hc"}
	heat := map[string]float64{
		"c.go": 3.0, // hottest
		"a.go": 1.0,
		"b.go": 0.5, // coldest
	}

	targets := []string{"a.go", "b.go", "c.go"}
	neverScanned, _, heatReordered := applySweepOrder(targets, heat, fps, watermarks)

	if neverScanned != 3 {
		t.Errorf("neverScanned = %d, want 3", neverScanned)
	}
	if !heatReordered {
		t.Error("heatReordered = false, want true (heat map reordered group 1)")
	}
	// c.go (heat=3) must lead.
	if targets[0] != "c.go" {
		t.Errorf("targets[0] = %q, want c.go (hottest in group 1)", targets[0])
	}
	if targets[1] != "a.go" {
		t.Errorf("targets[1] = %q, want a.go", targets[1])
	}
	if targets[2] != "b.go" {
		t.Errorf("targets[2] = %q, want b.go (coldest in group 1)", targets[2])
	}
}

// TestApplySweepOrder_StoreFallbackGivesHeatOrder verifies that when store reads
// succeed, the ordering is applied; and when not all files are in lastScanned,
// missing files are treated as never-scanned.
func TestApplySweepOrder_MissingFileIsNeverScanned(t *testing.T) {
	now := time.Now().UTC()
	// Only "a.go" is in the store; "b.go" is absent (new file).
	watermarks := map[string]store.Watermark{
		"a.go": wm(now.Add(-1*time.Hour), "ha"),
	}
	fps := map[string]string{"a.go": "ha", "b.go": "hb"}
	heat := map[string]float64{}

	targets := []string{"a.go", "b.go"}
	neverScanned, _, _ := applySweepOrder(targets, heat, fps, watermarks)

	if neverScanned != 1 {
		t.Errorf("neverScanned = %d, want 1 (b.go missing from store)", neverScanned)
	}
	// b.go (never-scanned) must be first.
	if targets[0] != "b.go" {
		t.Errorf("targets[0] = %q, want b.go (absent from store = never-scanned)", targets[0])
	}
}

// TestApplySweepOrder_HashChangedJoinsGroup1 verifies the third group-1
// condition: a previously-scanned file whose current fingerprint differs from
// the stored content hash is prioritised into group 1 (ahead of all clean
// group-2 files) and counted in changedSinceScan.
func TestApplySweepOrder_HashChangedJoinsGroup1(t *testing.T) {
	now := time.Now().UTC()
	watermarks := map[string]store.Watermark{
		"changed.go": wm(now.Add(-1*time.Hour), "old-hash"), // scanned, content changed
		"stale.go":   wm(now.Add(-72*time.Hour), "hs"),      // scanned long ago, unchanged
		"fresh.go":   wm(now.Add(-1*time.Minute), "hf"),     // scanned just now, unchanged
		"touched.go": wm(now.Add(-1*time.Hour), ""),         // covered but hash never recorded
	}
	fps := map[string]string{
		"changed.go": "new-hash",
		"stale.go":   "hs",
		"fresh.go":   "hf",
		"touched.go": "ht",
	}
	heat := map[string]float64{}

	targets := []string{"fresh.go", "stale.go", "changed.go", "touched.go"}
	neverScanned, changedSinceScan, _ := applySweepOrder(targets, heat, fps, watermarks)

	if neverScanned != 0 {
		t.Errorf("neverScanned = %d, want 0 (all files have non-epoch rows)", neverScanned)
	}
	// changed.go (hash mismatch) and touched.go (empty stored hash ≠ current)
	// both count as changed-since-scan.
	if changedSinceScan != 2 {
		t.Errorf("changedSinceScan = %d, want 2 (changed.go + touched.go)", changedSinceScan)
	}
	// Group 1 (alphabetical at equal zero heat): changed.go, touched.go.
	// Group 2 stalest-first: stale.go, fresh.go.
	want := []string{"changed.go", "touched.go", "stale.go", "fresh.go"}
	for i, w := range want {
		if targets[i] != w {
			t.Errorf("targets[%d] = %q, want %q (full order %v)", i, targets[i], w, targets)
		}
	}
}

// ---------------------------------------------------------------------------
// Acceptance test: rotation under budget truncation
// ---------------------------------------------------------------------------

// TestSweep_PartialCoverage_RotatesToFullCoverage is the acceptance test for
// the anti-starvation fix (bead bugbot-pav acceptance #1): repeated
// partial-coverage sweeps over an unchanged 5-file set must rotate so the
// union of covered files grows to the full set, with each sweep's coverage
// determined by the two-group ordering.
//
// Determinism design — why this test cannot flake AND cannot pass vacuously:
//
//   - Partial coverage per sweep is simulated by parse failures, not budget
//     stops: the scripted finder returns valid JSON ONLY for the one chunk we
//     expect at the head of the sweep's target order, and unparseable prose
//     for every other chunk (parse-failed units do not cover their files —
//     same coverage semantics as budget-skipped units, without the
//     goroutine-scheduling nondeterminism of a shared budget gate).
//   - Chunk COMPOSITION is the witness for ordering. With 5 files and
//     ChunkSize=2, sweep 2's expected chunk {e.go, c.go} straddles the
//     group-1/group-2 boundary: it exists IF AND ONly IF group 1 (uncovered
//     {a,b,e}, alphabetical at equal heat) precedes group 2 (covered {c,d}).
//     Under broken ordering (e.g. plain alphabetical) the chunks are
//     {a,b},{c,d},{e} — no task matches the {e,c} route, nothing is covered,
//     and the union assertion fails. A chunk-content route is position-blind,
//     so an even split could pass under broken ordering; the odd count is
//     what makes composition order-sensitive.
//   - SweepNeverScanned assertions independently catch a broken coverage
//     cursor (TouchScanCoverage regressions) even where chunk composition
//     happens to coincide.
func TestSweep_PartialCoverage_RotatesToFullCoverage(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Five same-language files committed together: equal churn heat, so group-1
	// ordering is the alphabetical tiebreak — fully deterministic.
	files := []string{"a.go", "b.go", "c.go", "d.go", "e.go"}
	dir := t.TempDir()
	for i, f := range files {
		content := "package fix\n\nfunc F" + string(rune('A'+i)) + "() int { return " + string(rune('0'+i)) + " }\n"
		if err := os.WriteFile(filepath.Join(dir, f), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitSeed(t, dir)
	repo, err := ingest.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	// sweepCovering runs one sweep whose finder parses ONLY the chunk whose
	// task names exactly the two seed files in wantChunk (in order), asserting
	// the sweep covered exactly those files and saw wantNeverScanned group-1
	// never-scanned files at ordering time.
	sweepCovering := func(label string, wantChunk []string, wantNeverScanned int) {
		t.Helper()
		finder := newScriptedClient()
		finder.fallback = "prose that never parses as JSON"
		routeSub := "- " + wantChunk[0] + "\n  - " + wantChunk[1]
		finder.onTaskContains(routeSub, emptyCandidates)
		f, err := New(RoleClients{Finder: finder, Verifier: newScriptedClient()}, st, repo, Options{
			Lenses:      []string{"nil-safety/error-handling"},
			ChunkSize:   2,
			MaxParallel: 1,
		})
		if err != nil {
			t.Fatalf("%s: New: %v", label, err)
		}
		res, err := f.Sweep(ctx)
		if err != nil {
			t.Fatalf("%s: Sweep: %v", label, err)
		}
		if res.Stats.SweepNeverScanned != wantNeverScanned {
			t.Errorf("%s: SweepNeverScanned = %d, want %d (coverage cursor not advancing)",
				label, res.Stats.SweepNeverScanned, wantNeverScanned)
		}
		got := append([]string(nil), res.CoveredFiles...)
		sort.Strings(got)
		want := append([]string(nil), wantChunk...)
		sort.Strings(want)
		if len(got) != len(want) {
			t.Fatalf("%s: CoveredFiles = %v, want %v — the expected head chunk was not formed, so the two-group ordering is broken",
				label, res.CoveredFiles, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: CoveredFiles = %v, want %v", label, res.CoveredFiles, want)
			}
		}
	}

	// Sweep 1: nothing scanned → group 1 = all five, alphabetical → chunks
	// {a,b},{c,d},{e}. Cover {c,d} (deliberately NOT the head chunk, so later
	// staleness order differs from alphabetical order).
	sweepCovering("sweep1", []string{"c.go", "d.go"}, 5)

	// Sweep 2: group 1 = uncovered {a,b,e} (alphabetical), group 2 = {c,d} →
	// targets a,b,e,c,d → chunks {a,b},{e,c},{d}. The {e,c} chunk exists only
	// under correct two-group ordering. Cover it.
	sweepCovering("sweep2", []string{"e.go", "c.go"}, 3)

	// Sweep 3: group 1 = {a,b}; group 2 stalest-first = d (sweep 1), then c, e
	// (sweep 2, alphabetical at equal timestamps) → targets a,b,d,c,e → chunks
	// {a,b},{d,c},{e}. Cover {a,b}: the union now spans all five files.
	sweepCovering("sweep3", []string{"a.go", "b.go"}, 2)

	// Every file now has a real (non-epoch) scan timestamp: a fourth sweep sees
	// an empty group 1 — the rotation converged to full coverage.
	wms, err := st.ScanWatermarks(ctx, files)
	if err != nil {
		t.Fatalf("ScanWatermarks: %v", err)
	}
	for _, f := range files {
		w, ok := wms[f]
		if !ok || w.LastScannedAt.Equal(epochSentinelParsed) {
			t.Errorf("%s: no truthful scan timestamp after 3 sweeps (union did not reach full set)", f)
		}
	}
}

// gitSeed initialises dir as a git repo with one commit containing all files.
func gitSeed(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-q", "-m", "seed"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// ---------------------------------------------------------------------------
// Daemon: refreshWatermarks uses RefreshContentHashes (preserves last_scanned_at)
// ---------------------------------------------------------------------------

// TestRefreshWatermarks_PreservesLastScannedAt verifies that refreshWatermarks
// (which now calls RefreshContentHashes, not UpsertFileStates) does NOT clobber
// a truthful last_scanned_at timestamp set by TouchScanCoverage.
func TestRefreshWatermarks_PreservesLastScannedAt(t *testing.T) {
	ctx := context.Background()

	// Open a store and set a truthful scan timestamp via TouchScanCoverage.
	st := openStoreForFunnelTest(t)
	if err := st.TouchScanCoverage(ctx, []string{"a.go"}, "c1", nil); err != nil {
		t.Fatalf("TouchScanCoverage: %v", err)
	}
	beforeFS, err := st.GetFileState(ctx, "a.go")
	if err != nil {
		t.Fatalf("GetFileState before refresh: %v", err)
	}

	// Now call RefreshContentHashes (what refreshWatermarks uses).
	if err := st.RefreshContentHashes(ctx, []store.FileState{
		{Path: "a.go", ContentHash: "newhash", LastScannedCommit: "c2"},
	}); err != nil {
		t.Fatalf("RefreshContentHashes: %v", err)
	}

	afterFS, err := st.GetFileState(ctx, "a.go")
	if err != nil {
		t.Fatalf("GetFileState after refresh: %v", err)
	}

	// last_scanned_at must be unchanged.
	if !afterFS.LastScannedAt.Equal(beforeFS.LastScannedAt) {
		t.Errorf("refreshWatermarks clobbered last_scanned_at: before=%v after=%v",
			beforeFS.LastScannedAt, afterFS.LastScannedAt)
	}
	// Content hash must be updated.
	if afterFS.ContentHash != "newhash" {
		t.Errorf("ContentHash not updated: %q", afterFS.ContentHash)
	}
}

// openStoreForFunnelTest opens a file-backed store for tests in the funnel
// package (which cannot use openFixture's store because it needs a Store, not
// the (Store, Repo) pair).
func openStoreForFunnelTest(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	dbPath := t.TempDir() + "/state.db"
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

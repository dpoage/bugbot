package funnel

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// leakSrc is a syntactically-parseable Go file whose single function spans
// several lines, so tree-sitter resolves lines 5 and 9 to the SAME enclosing
// symbol locus (S:function\x00Leak). Identifiers are undefined — tree-sitter
// only needs to parse, not compile.
const leakSrc = `package x

func Leak() error {
	a := acquire()
	if a == nil {
		return errInit
	}
	use(a)
	return release(a)
}
`

// seedPriorRunFinding writes a source file under a fresh temp root and persists
// an OPEN verified finding (lens A) at seedLine, exactly as a prior, since-exited
// run would have left it. It returns the root, the seeded fingerprint, and the
// locus the two lines share. The store carries the new locus_key column, so the
// seeded row is discoverable by OpenFindingsByLocusKey.
func seedPriorRunFinding(t *testing.T, st *store.Store, file, lensA, desc string, seedLine, candLine int) (root, seedFP, locus string) {
	t.Helper()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, file), []byte(leakSrc), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	resolver := NewLocusResolver(root)
	locus = resolver.Resolve(file, seedLine)
	// Precondition: the durable fold only fires when both lines map to ONE locus
	// key. If tree-sitter did not group them this test is not exercising the
	// same-symbol path and must fail loudly rather than pass vacuously.
	if got := resolver.Resolve(file, candLine); got != locus {
		t.Fatalf("precondition: lines %d and %d resolve to different loci (%q vs %q)", seedLine, candLine, locus, got)
	}
	seedFP = domain.Fingerprint(lensA, file, locus)
	if _, err := st.UpsertFinding(context.Background(), domain.Finding{
		Fingerprint: seedFP,
		LocusKey:    domain.LocusKey(file, locus),
		Title:       "Leak leaks handle on error path",
		Description: desc,
		Severity:    "high",
		Tier:        domain.TierVerified,
		Status:      domain.StatusOpen,
		Lens:        lensA,
		File:        file,
		Line:        seedLine,
		Sites:       []domain.Site{{File: file, Line: seedLine}},
	}); err != nil {
		t.Fatalf("seed prior-run finding: %v", err)
	}
	return root, seedFP, locus
}

// TestTriageState_DurableCrossLensFold proves the resume fix: a finding persisted
// by a prior (interrupted) run is folded into — not duplicated by — a later
// same-locus, different-lens candidate replayed through a FRESH triageState whose
// in-memory cluster state is empty. The candidate must NOT be forwarded as a
// second primary; the surviving single finding carries the second lens as
// corroboration and gains the candidate's distinct site.
func TestTriageState_DurableCrossLensFold(t *testing.T) {
	ctx := context.Background()
	st, _ := openFixture(t)

	const file, lensA, lensB = "x.go", "resource-leaks", "exception-safety"
	const seedLine, candLine = 5, 9
	desc := "the acquired handle a is leaked when use a fails because release is never called on the error path"

	root, seedFP, _ := seedPriorRunFinding(t, st, file, lensA, desc, seedLine, candLine)

	// Fresh triageState over the same snapshot: no in-memory cluster knows about
	// the prior-run primary — only the durable store does.
	snap := &ingest.Snapshot{Root: root, Files: []ingest.File{{Path: file}}}
	ts, _ := newTriageState(snap)
	var stats Stats
	cand := Candidate{
		Lens: lensB, File: file, Line: candLine,
		Title:       "handle not released on the exception path",
		Description: desc,
		Severity:    "high", Confidence: "high",
	}
	if err := ts.process(ctx, st, &stats, cand); err != nil {
		t.Fatalf("process: %v", err)
	}

	if ready := ts.popReady(); len(ready) != 0 {
		t.Fatalf("popReady = %d, want 0 (candidate folded, not forwarded): %+v", len(ready), ready)
	}
	if stats.MergedCrossLens != 1 {
		t.Errorf("MergedCrossLens = %d, want 1", stats.MergedCrossLens)
	}

	all, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("open findings = %d, want 1 (no duplicate row): %+v", len(all), all)
	}
	got := all[0]
	if got.Fingerprint != seedFP {
		t.Errorf("surviving fingerprint = %q, want the prior-run primary %q", got.Fingerprint, seedFP)
	}
	if !reflect.DeepEqual(got.CorroboratingLenses, []string{lensB}) {
		t.Errorf("CorroboratingLenses = %v, want [%q]", got.CorroboratingLenses, lensB)
	}
	if len(got.Sites) != 2 {
		t.Errorf("Sites = %+v, want 2 (seed line %d + folded candidate line %d)", got.Sites, seedLine, candLine)
	}
}

// TestTriageState_DurableFold_DistinctBugNotMerged proves the similarity guard:
// a candidate at the SAME locus but describing a DIFFERENT defect (low
// description overlap) is NOT folded — it survives as its own primary and leaves
// the prior finding untouched. Without this guard the locus key alone would
// collapse two genuinely distinct bugs that share one enclosing symbol.
func TestTriageState_DurableFold_DistinctBugNotMerged(t *testing.T) {
	ctx := context.Background()
	st, _ := openFixture(t)

	const file, lensA, lensB = "x.go", "resource-leaks", "boundary-conditions"
	const seedLine, candLine = 5, 9
	leakDesc := "the acquired handle a is leaked when use a fails because release is never called on the error path"

	root, seedFP, _ := seedPriorRunFinding(t, st, file, lensA, leakDesc, seedLine, candLine)

	snap := &ingest.Snapshot{Root: root, Files: []ingest.File{{Path: file}}}
	ts, _ := newTriageState(snap)
	var stats Stats
	cand := Candidate{
		Lens: lensB, File: file, Line: candLine,
		Title:       "integer overflow in size computation",
		Description: "multiplying width times height overflows int32 producing a negative allocation size",
		Severity:    "high", Confidence: "high",
	}
	if err := ts.process(ctx, st, &stats, cand); err != nil {
		t.Fatalf("process: %v", err)
	}

	if ready := ts.popReady(); len(ready) != 1 {
		t.Fatalf("popReady = %d, want 1 (distinct bug forwarded as its own primary): %+v", len(ready), ready)
	}
	if stats.MergedCrossLens != 0 {
		t.Errorf("MergedCrossLens = %d, want 0 (similarity guard must reject the merge)", stats.MergedCrossLens)
	}
	seeded, err := st.GetFindingByFingerprint(ctx, seedFP)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if len(seeded.CorroboratingLenses) != 0 {
		t.Errorf("prior finding gained corroboration %v; the distinct bug must not have folded in", seeded.CorroboratingLenses)
	}
	if len(seeded.Sites) != 1 {
		t.Errorf("prior finding Sites = %+v, want 1 (unchanged)", seeded.Sites)
	}
}

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
// locus the two lines share. The store carries the locus_key column, so the
// seeded row is discoverable at that exact locus_key by
// store.FindingsByFileWindow's same-file window query too.
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
	if stats.MergedCrossLensDurable != 1 {
		t.Errorf("MergedCrossLensDurable = %d, want 1", stats.MergedCrossLensDurable)
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
	if stats.MergedCrossLensDurable != 0 {
		t.Errorf("MergedCrossLensDurable = %d, want 0 (similarity guard must reject the merge)", stats.MergedCrossLensDurable)
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

// twoFuncSrc is a syntactically-parseable Go file with two SEPARATE top-level
// functions close enough together that their lines fall within
// DefaultMergeWindow of each other, but far enough apart in the syntax tree
// that tree-sitter resolves each to its OWN enclosing-symbol locus (unlike
// leakSrc's single function, which gives both candidate lines the SAME
// locus). It backs the same-file-window tests: a candidate at FuncB's line
// must be discoverable by the widened FindingsByFileWindow lookup even though
// its locus_key (and therefore its v3 fingerprint) differs from a finding
// seeded at FuncA's line.
const twoFuncSrc = `package x

func FuncA() error {
	return doA()
}

func FuncB() error {
	return doB()
}
`

// seedFileWindowFinding writes twoFuncSrc under a fresh temp root and persists
// a finding of the given status at seedLine (FuncA), returning the root,
// fingerprint, and the two functions' loci. It fails the test if seedLine and
// candLine resolve to the SAME locus — this suite exercises the case a
// locus-key-only lookup would miss.
func seedFileWindowFinding(t *testing.T, st *store.Store, file, lensA, desc string, seedLine, candLine int, status domain.Status) (root, seedFP string) {
	t.Helper()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, file), []byte(twoFuncSrc), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	resolver := NewLocusResolver(root)
	seedLocus := resolver.Resolve(file, seedLine)
	candLocus := resolver.Resolve(file, candLine)
	if seedLocus == candLocus {
		t.Fatalf("precondition: lines %d and %d resolve to the SAME locus (%q); this suite needs DIFFERENT loci within the same file window", seedLine, candLine, seedLocus)
	}
	seedFP = domain.FingerprintV3(file, seedLocus, "", "")
	if _, err := st.UpsertFinding(context.Background(), domain.Finding{
		Fingerprint: seedFP,
		LocusKey:    domain.LocusKey(file, seedLocus),
		Title:       "FuncA leaks handle on error path",
		Description: desc,
		Severity:    "high",
		Tier:        domain.TierVerified,
		Status:      status,
		Lens:        lensA,
		File:        file,
		Line:        seedLine,
		Sites:       []domain.Site{{File: file, Line: seedLine}},
	}); err != nil {
		t.Fatalf("seed file-window finding: %v", err)
	}
	return root, seedFP
}

// TestTriageState_DurableCrossLensFold_SameFileWindow_LocusDrifted proves the
// widened lookup: a candidate whose fingerprint DRIFTED away from a prior
// OPEN finding's fingerprint (different enclosing-symbol locus, e.g. the
// finder resolved a different function) is still folded in as corroboration
// when it falls within the same-file window SimilarFinding uses — the exact
// case an exact-locusKey lookup could not catch. The candidate must never be
// forwarded to verify: popReady() stays empty, so no refuter panel is ever
// spent on it.
func TestTriageState_DurableCrossLensFold_SameFileWindow_LocusDrifted(t *testing.T) {
	ctx := context.Background()
	st, _ := openFixture(t)

	const file, lensA, lensB = "x.go", "resource-leaks", "exception-safety"
	const seedLine, candLine = 3, 7 // FuncA / FuncB, 4 lines apart, well within DefaultMergeWindow (10)
	desc := "the acquired handle a is leaked when use a fails because release is never called on the error path"

	root, seedFP := seedFileWindowFinding(t, st, file, lensA, desc, seedLine, candLine, domain.StatusOpen)

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

	// The candidate must NEVER reach verify: no primary was forwarded.
	if ready := ts.popReady(); len(ready) != 0 {
		t.Fatalf("popReady = %d, want 0 (a candidate reaching verify means a refuter panel would be spent): %+v", len(ready), ready)
	}
	if stats.MergedCrossLensDurable != 1 {
		t.Errorf("MergedCrossLensDurable = %d, want 1", stats.MergedCrossLensDurable)
	}

	all, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("open findings = %d, want 1 (no duplicate row): %+v", len(all), all)
	}
	if all[0].Fingerprint != seedFP {
		t.Errorf("surviving fingerprint = %q, want the prior-run primary %q", all[0].Fingerprint, seedFP)
	}
	if !reflect.DeepEqual(all[0].CorroboratingLenses, []string{lensB}) {
		t.Errorf("CorroboratingLenses = %v, want [%q]", all[0].CorroboratingLenses, lensB)
	}
}

// TestTriageState_DurableCrossLensFold_DismissedMatch_Suppressed proves the
// dismissed-match branch: a candidate matching a DISMISSED finding in the
// same-file window is suppressed outright (its own fingerprint recorded as a
// suppression), not folded as corroboration and not forwarded to verify —
// converging with bugbot-oiem's suppression-side identity handling.
func TestTriageState_DurableCrossLensFold_DismissedMatch_Suppressed(t *testing.T) {
	ctx := context.Background()
	st, _ := openFixture(t)

	const file, lensA, lensB = "x.go", "resource-leaks", "exception-safety"
	const seedLine, candLine = 3, 7
	desc := "the acquired handle a is leaked when use a fails because release is never called on the error path"

	root, seedFP := seedFileWindowFinding(t, st, file, lensA, desc, seedLine, candLine, domain.StatusDismissed)

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
		t.Fatalf("popReady = %d, want 0 (a dismissed-match candidate must never reach verify): %+v", len(ready), ready)
	}
	if stats.DroppedSuppressedDurable != 1 {
		t.Errorf("DroppedSuppressedDurable = %d, want 1", stats.DroppedSuppressedDurable)
	}
	if stats.MergedCrossLensDurable != 0 {
		t.Errorf("MergedCrossLensDurable = %d, want 0 (a dismissed match is suppressed, not merged)", stats.MergedCrossLensDurable)
	}

	// The dismissed row itself is untouched: still exactly one row, still
	// dismissed, no corroboration attached (suppression is not corroboration).
	seeded, err := st.GetFindingByFingerprint(ctx, seedFP)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if seeded.Status != domain.StatusDismissed {
		t.Errorf("seeded finding status = %q, want dismissed (unchanged)", seeded.Status)
	}
	if len(seeded.CorroboratingLenses) != 0 {
		t.Errorf("seeded finding gained corroboration %v; a suppression must not attach a lens", seeded.CorroboratingLenses)
	}

	// The candidate's OWN fingerprint must now be independently suppressed so
	// a future re-scan is dropped at triage step 4 without ever reaching this
	// fold again.
	resolver := NewLocusResolver(root)
	candLocus := resolver.Resolve(file, candLine)
	candFP := domain.FingerprintV3(file, candLocus, "", "")
	sup, err := st.IsSuppressed(ctx, candFP, domain.LocusKey(file, candLocus))
	if err != nil {
		t.Fatalf("IsSuppressed: %v", err)
	}
	if !sup {
		t.Error("candidate's own fingerprint should be suppressed after a dismissed-match fold")
	}

	all, err := st.ListFindings(ctx, domain.FindingFilter{})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("total findings = %d, want 1 (a suppression must not mint a new row): %+v", len(all), all)
	}
}

// TestTriageState_DurableCrossLensFold_FixedMatch_RegressionReopened proves
// the fixed-match branch: a candidate matching a FIXED finding in the
// same-file window reopens THAT row as a regression (identity and tier/repro
// history preserved, no new row) instead of minting a fresh finding, and is
// never forwarded to verify.
func TestTriageState_DurableCrossLensFold_FixedMatch_RegressionReopened(t *testing.T) {
	ctx := context.Background()
	st, _ := openFixture(t)

	const file, lensA, lensB = "x.go", "resource-leaks", "exception-safety"
	const seedLine, candLine = 3, 7
	desc := "the acquired handle a is leaked when use a fails because release is never called on the error path"

	root, seedFP := seedFileWindowFinding(t, st, file, lensA, desc, seedLine, candLine, domain.StatusFixed)
	seededBefore, err := st.GetFindingByFingerprint(ctx, seedFP)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint (before): %v", err)
	}

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
		t.Fatalf("popReady = %d, want 0 (a fixed-match regression must never reach verify): %+v", len(ready), ready)
	}
	if stats.RegressionReopened != 1 {
		t.Errorf("RegressionReopened = %d, want 1", stats.RegressionReopened)
	}
	if stats.MergedCrossLensDurable != 0 {
		t.Errorf("MergedCrossLensDurable = %d, want 0 (a fixed match reopens, it does not merge as if still open)", stats.MergedCrossLensDurable)
	}

	reopened, err := st.GetFindingByFingerprint(ctx, seedFP)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint (after): %v", err)
	}
	if reopened.ID != seededBefore.ID {
		t.Fatalf("regression reopen changed the row id: got %q, want %q", reopened.ID, seededBefore.ID)
	}
	if reopened.Status != domain.StatusOpen {
		t.Fatalf("reopened finding status = %q, want open", reopened.Status)
	}
	if !reflect.DeepEqual(reopened.CorroboratingLenses, []string{lensB}) {
		t.Errorf("CorroboratingLenses = %v, want [%q]", reopened.CorroboratingLenses, lensB)
	}
	if len(reopened.Sites) != 2 {
		t.Errorf("Sites = %+v, want 2 (seed line %d + folded candidate line %d)", reopened.Sites, seedLine, candLine)
	}

	all, err := st.ListFindings(ctx, domain.FindingFilter{})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("total findings = %d, want 1 (a regression reopen must not mint a new row): %+v", len(all), all)
	}
}

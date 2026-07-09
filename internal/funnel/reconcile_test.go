package funnel

// reconcile_test.go covers the bugbot-ezmx.4 acceptance criteria for
// ReconcileDedup: a confident arbiter "yes" merges the newer OPEN finding
// into the older (canonical) one and closes it with a reason referencing the
// canonical fingerprint; "no"/"unsure" both keep two distinct findings; a
// kind-mismatched pair is never even nominated (zero arbiter calls); and the
// per-cycle cap gracefully passes through once exhausted.

import (
	"context"
	"fmt"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
)

// reconcileFindingAt seeds an OPEN finding at (file, line) with the given
// lens/title/description/kind, distinct from the other package's
// sampleFinding helpers so reconcile tests can build controlled pairs.
func reconcileFindingAt(t *testing.T, st interface {
	UpsertFinding(ctx context.Context, f domain.Finding) (domain.Finding, error)
}, lens, file string, line int, title, desc string, kind domain.DefectKind) domain.Finding {
	t.Helper()
	fp := domain.Fingerprint(lens, file, fmt.Sprintf("%d|%s", line, title))
	f, err := st.UpsertFinding(context.Background(), domain.Finding{
		Fingerprint: fp,
		Title:       title,
		Description: desc,
		Severity:    "high",
		Tier:        2,
		Status:      domain.StatusOpen,
		Lens:        lens,
		File:        file,
		Line:        line,
		DefectKind:  kind,
		Subject:     title,
	})
	if err != nil {
		t.Fatalf("seed finding %q: %v", title, err)
	}
	return f
}

// closeDescA / closeDescB are a SimilarFinding-close pair (same file/window,
// jaccard well above mergeSimilarityThreshold): worded almost identically so
// the deterministic pre-gate nominates them regardless of kind.
const (
	closeDescA = "the config pointer may be nil and is dereferenced without a guard in Greeting"
	closeDescB = "the config pointer may be nil and is dereferenced without any guard inside Greeting"
)

func requireCloseFixture(t *testing.T, file string, lineA, lineB int) {
	t.Helper()
	if !SimilarFinding(file, lineA, closeDescA, file, lineB, closeDescB) {
		t.Fatalf("fixture broken: closeDescA/closeDescB at lines %d/%d are not SimilarFinding-close", lineA, lineB)
	}
}

// TestReconcileDedup_ConfidentYesMerges proves the core merge path: an
// older/newer OPEN pair, same-file/window, compatible kind, SimilarFinding-
// close descriptions, judged "yes" by the arbiter -- the newer row is folded
// into the older (canonical) row and closed StatusSuperseded with a reason
// referencing the canonical fingerprint.
func TestReconcileDedup_ConfidentYesMerges(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	requireCloseFixture(t, "bug.go", 10, 12)

	older := reconcileFindingAt(t, st, "nil-safety", "bug.go", 10, "older nil deref", closeDescA, domain.DefectNilDeref)
	newer := reconcileFindingAt(t, st, "resource-leaks", "bug.go", 12, "newer nil deref", closeDescB, domain.DefectNilDeref)

	client := newScriptedClient().onSystemContains("SAME underlying defect", dedupVerdictJSON("yes", "same nil-deref defect, worded differently"))
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: client}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.ReconcileDedup(ctx, DefaultReconcileCap)
	if err != nil {
		t.Fatalf("ReconcileDedup: %v", err)
	}
	if res.Stats.ReconcileNominated != 1 {
		t.Fatalf("ReconcileNominated = %d, want 1", res.Stats.ReconcileNominated)
	}
	if res.Stats.ReconcileArbitrated != 1 {
		t.Fatalf("ReconcileArbitrated = %d, want 1", res.Stats.ReconcileArbitrated)
	}
	if res.Stats.ReconcileMerged != 1 {
		t.Fatalf("ReconcileMerged = %d, want 1", res.Stats.ReconcileMerged)
	}
	if client.callCount() != 1 {
		t.Fatalf("arbiter callCount = %d, want 1", client.callCount())
	}

	gotNewer, err := st.GetFinding(ctx, newer.ID)
	if err != nil {
		t.Fatalf("GetFinding(newer): %v", err)
	}
	if gotNewer.Status != domain.StatusSuperseded {
		t.Fatalf("newer.Status = %q, want %q", gotNewer.Status, domain.StatusSuperseded)
	}
	if gotNewer.SupersededBy != older.Fingerprint {
		t.Fatalf("newer.SupersededBy = %q, want canonical fingerprint %q", gotNewer.SupersededBy, older.Fingerprint)
	}
	if gotNewer.SupersededReason == "" {
		t.Fatal("newer.SupersededReason must be set")
	}

	gotOlder, err := st.GetFinding(ctx, older.ID)
	if err != nil {
		t.Fatalf("GetFinding(older): %v", err)
	}
	if gotOlder.Status != domain.StatusOpen {
		t.Fatalf("older.Status = %q, want unchanged %q (canonical survives)", gotOlder.Status, domain.StatusOpen)
	}
	found := false
	for _, l := range gotOlder.CorroboratingLenses {
		if l == newer.Lens {
			found = true
		}
	}
	if !found {
		t.Fatalf("older.CorroboratingLenses = %v, want to include newer's lens %q", gotOlder.CorroboratingLenses, newer.Lens)
	}
}

// TestReconcileDedup_NoKeepsBoth proves a distinct pair the arbiter judges
// "no" survives as two separate open findings, not merged.
func TestReconcileDedup_NoKeepsBoth(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	requireCloseFixture(t, "bug.go", 10, 12)

	older := reconcileFindingAt(t, st, "nil-safety", "bug.go", 10, "older nil deref", closeDescA, domain.DefectNilDeref)
	newer := reconcileFindingAt(t, st, "resource-leaks", "bug.go", 12, "newer nil deref", closeDescB, domain.DefectNilDeref)

	client := newScriptedClient().onSystemContains("SAME underlying defect", dedupVerdictJSON("no", "different failure modes despite similar wording"))
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: client}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.ReconcileDedup(ctx, DefaultReconcileCap)
	if err != nil {
		t.Fatalf("ReconcileDedup: %v", err)
	}
	if res.Stats.ReconcileMerged != 0 {
		t.Fatalf("ReconcileMerged = %d, want 0", res.Stats.ReconcileMerged)
	}

	for _, id := range []string{older.ID, newer.ID} {
		got, err := st.GetFinding(ctx, id)
		if err != nil {
			t.Fatalf("GetFinding(%s): %v", id, err)
		}
		if got.Status != domain.StatusOpen {
			t.Fatalf("finding %s Status = %q, want unchanged %q", id, got.Status, domain.StatusOpen)
		}
		if got.SupersededBy != "" {
			t.Fatalf("finding %s SupersededBy = %q, want empty", id, got.SupersededBy)
		}
	}
}

// TestReconcileDedup_UnsureKeepsBoth proves "unsure" is treated identically
// to "no": both findings survive, precision-first.
func TestReconcileDedup_UnsureKeepsBoth(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	requireCloseFixture(t, "bug.go", 10, 12)

	reconcileFindingAt(t, st, "nil-safety", "bug.go", 10, "older nil deref", closeDescA, domain.DefectNilDeref)
	reconcileFindingAt(t, st, "resource-leaks", "bug.go", 12, "newer nil deref", closeDescB, domain.DefectNilDeref)

	client := newScriptedClient().onSystemContains("SAME underlying defect", dedupVerdictJSON("unsure", "cannot tell from the excerpt alone"))
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: client}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.ReconcileDedup(ctx, DefaultReconcileCap)
	if err != nil {
		t.Fatalf("ReconcileDedup: %v", err)
	}
	if res.Stats.ReconcileMerged != 0 {
		t.Fatalf("ReconcileMerged = %d, want 0", res.Stats.ReconcileMerged)
	}
	if res.Stats.ReconcileArbitrated != 1 {
		t.Fatalf("ReconcileArbitrated = %d, want 1", res.Stats.ReconcileArbitrated)
	}
}

// TestReconcileDedup_KindMismatchNeverNominated proves a kind-incompatible
// pair -- otherwise same file/window and SimilarFinding-close -- is never
// even nominated: zero arbiter calls, zero stats churn, both findings kept
// open untouched.
func TestReconcileDedup_KindMismatchNeverNominated(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	requireCloseFixture(t, "bug.go", 10, 12)

	reconcileFindingAt(t, st, "nil-safety", "bug.go", 10, "older nil deref", closeDescA, domain.DefectNilDeref)
	reconcileFindingAt(t, st, "resource-leaks", "bug.go", 12, "newer resource leak", closeDescB, domain.DefectResourceLeak)

	// Fixture precondition: the two kinds really are incompatible.
	if sameOrUnknownKind(domain.DefectNilDeref, domain.DefectResourceLeak) {
		t.Fatal("fixture broken: nil-deref and resource-leak must be incompatible kinds")
	}

	client := newScriptedClient().onSystemContains("SAME underlying defect", dedupVerdictJSON("yes", "should never be asked"))
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: client}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.ReconcileDedup(ctx, DefaultReconcileCap)
	if err != nil {
		t.Fatalf("ReconcileDedup: %v", err)
	}
	if res.Stats.ReconcileNominated != 0 {
		t.Fatalf("ReconcileNominated = %d, want 0 (kind mismatch must never nominate)", res.Stats.ReconcileNominated)
	}
	if client.callCount() != 0 {
		t.Fatalf("arbiter callCount = %d, want 0 (kind mismatch must never call the arbiter)", client.callCount())
	}
	// No scan run should even open: nominateReconcilePairs returned nothing,
	// so ReconcileDedup hits the empty-pair fast path (mirrors VerifyDrain).
	if res.ScanRunID != "" {
		t.Fatalf("ScanRunID = %q, want empty (no scan run for zero nominations)", res.ScanRunID)
	}
}

// TestReconcileDedup_CapExhausted_PassesThrough proves the per-cycle
// invocation cap: with two independent nominated pairs (different files) and
// cap=1, only the first (file-order-deterministic) pair reaches the
// arbiter; the second is skipped and both its findings stay open.
func TestReconcileDedup_CapExhausted_PassesThrough(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	requireCloseFixture(t, "a.go", 10, 12)
	requireCloseFixture(t, "b.go", 10, 12)

	reconcileFindingAt(t, st, "nil-safety", "a.go", 10, "a older", closeDescA, domain.DefectNilDeref)
	reconcileFindingAt(t, st, "resource-leaks", "a.go", 12, "a newer", closeDescB, domain.DefectNilDeref)
	bOlder := reconcileFindingAt(t, st, "nil-safety", "b.go", 10, "b older", closeDescA, domain.DefectNilDeref)
	bNewer := reconcileFindingAt(t, st, "resource-leaks", "b.go", 12, "b newer", closeDescB, domain.DefectNilDeref)

	client := newScriptedClient().onSystemContains("SAME underlying defect", dedupVerdictJSON("yes", "same defect"))
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: client}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.ReconcileDedup(ctx, 1)
	if err != nil {
		t.Fatalf("ReconcileDedup: %v", err)
	}
	if res.Stats.ReconcileNominated != 2 {
		t.Fatalf("ReconcileNominated = %d, want 2", res.Stats.ReconcileNominated)
	}
	if res.Stats.ReconcileArbitrated != 1 {
		t.Fatalf("ReconcileArbitrated = %d, want 1 (cap=1)", res.Stats.ReconcileArbitrated)
	}
	if res.Stats.ReconcileSkippedCap != 1 {
		t.Fatalf("ReconcileSkippedCap = %d, want 1", res.Stats.ReconcileSkippedCap)
	}
	if res.Stats.ReconcileMerged != 1 {
		t.Fatalf("ReconcileMerged = %d, want 1", res.Stats.ReconcileMerged)
	}
	if client.callCount() != 1 {
		t.Fatalf("arbiter callCount = %d, want 1", client.callCount())
	}

	// b.go's pair (nominated second, alphabetically after a.go) must have been
	// skipped by the cap: both findings still open.
	for _, id := range []string{bOlder.ID, bNewer.ID} {
		got, err := st.GetFinding(ctx, id)
		if err != nil {
			t.Fatalf("GetFinding(%s): %v", id, err)
		}
		if got.Status != domain.StatusOpen {
			t.Fatalf("finding %s Status = %q, want unchanged %q (skipped by cap)", id, got.Status, domain.StatusOpen)
		}
	}
}

// TestReconcileDedup_NoOpenFindings_NoScanRun proves the empty fast path:
// with no OPEN findings at all, ReconcileDedup returns immediately without
// opening a scan run or touching the arbiter.
func TestReconcileDedup_NoOpenFindings_NoScanRun(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	client := newScriptedClient()
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: client}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.ReconcileDedup(ctx, DefaultReconcileCap)
	if err != nil {
		t.Fatalf("ReconcileDedup: %v", err)
	}
	if res.ScanRunID != "" {
		t.Fatalf("ScanRunID = %q, want empty", res.ScanRunID)
	}
	if client.callCount() != 0 {
		t.Fatalf("arbiter callCount = %d, want 0", client.callCount())
	}
}

// TestReconcileDedup_IdempotentOnReplay proves a second ReconcileDedup call
// immediately after a merge nominates nothing for the already-merged pair:
// the merged-away row is StatusSuperseded and excluded from the next OPEN
// query, so the re-run is a clean no-op (no new scan run, no arbiter calls).
func TestReconcileDedup_IdempotentOnReplay(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	requireCloseFixture(t, "bug.go", 10, 12)

	reconcileFindingAt(t, st, "nil-safety", "bug.go", 10, "older nil deref", closeDescA, domain.DefectNilDeref)
	reconcileFindingAt(t, st, "resource-leaks", "bug.go", 12, "newer nil deref", closeDescB, domain.DefectNilDeref)

	client := newScriptedClient().onSystemContains("SAME underlying defect", dedupVerdictJSON("yes", "same nil-deref defect"))
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: client}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}

	first, err := f.ReconcileDedup(ctx, DefaultReconcileCap)
	if err != nil {
		t.Fatalf("first ReconcileDedup: %v", err)
	}
	if first.Stats.ReconcileMerged != 1 {
		t.Fatalf("first ReconcileMerged = %d, want 1", first.Stats.ReconcileMerged)
	}

	second, err := f.ReconcileDedup(ctx, DefaultReconcileCap)
	if err != nil {
		t.Fatalf("second ReconcileDedup: %v", err)
	}
	if second.ScanRunID != "" {
		t.Fatalf("second ScanRunID = %q, want empty (no-op replay)", second.ScanRunID)
	}
	if second.Stats.ReconcileNominated != 0 {
		t.Fatalf("second ReconcileNominated = %d, want 0", second.Stats.ReconcileNominated)
	}
	if client.callCount() != 1 {
		t.Fatalf("arbiter callCount after replay = %d, want still 1 (no new calls)", client.callCount())
	}
}

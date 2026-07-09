package funnel

// dedup_arbiter_test.go covers the bugbot-ezmx.2 acceptance criteria: the
// dedup arbiter fires only on jaccard-gate collisions (near, kind-compatible,
// jaccard below mergeSimilarityThreshold), a confident "yes" folds via the
// same registry-staging path a jaccard-confirmed match would use, "no" and
// "unsure" both keep the two candidates as distinct primaries, and the
// per-scan cap gracefully passes both through once exhausted.
import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// dedupCollisionPair returns two candidates at the same file, within
// DefaultMergeWindow, same defect_kind (so sameOrUnknownKind holds and
// clusterAccepts' near-check passes), but with LOW description-token jaccard
// (paraphrased so differently they fall below mergeSimilarityThreshold) and
// different subjects (so they mint different v3 fingerprints and reach
// clustering instead of colliding at the exact-fingerprint step). Different
// lenses so a "yes" merge is observably cross-lens (staged corroborating lens).
func dedupCollisionPair() (a, b Candidate) {
	a = Candidate{
		Lens: "nil-safety/error-handling", File: "bug.go", Line: 10,
		Title: "cfg nil deref", Description: "cfg pointer may be nil and is dereferenced without a guard",
		Severity: "high", Confidence: "high",
		DefectKind: domain.DefectNilDeref, Subject: "Greeting",
	}
	b = Candidate{
		Lens: "resource-leaks", File: "bug.go", Line: 12,
		Title: "config handle unchecked", Description: "the config struct field is accessed even though it could be unset here",
		Severity: "high", Confidence: "high",
		DefectKind: domain.DefectNilDeref, Subject: "GreetingV2",
	}
	return a, b
}

// newDedupTriageState builds a triageState wired with a dedup arbiter backed
// by client and cap, ready to process dedupCollisionPair-style candidates.
func newDedupTriageState(t *testing.T, client llm.Client, cap int) (*triageState, *clusterRegistry, *store.Store) {
	t.Helper()
	st, repo := openFixture(t)
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: client}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rec := &spendRecorder{ctx: context.Background(), store: st}
	budget := newBudgetState(0, rec, 1.0)
	snap := &ingest.Snapshot{Files: []ingest.File{{Path: "bug.go"}}}
	ts, reg := newTriageState(snap)
	ts.dedupArbiter = &dedupArbiterConfig{f: f, client: client, budget: budget, cap: cap}
	return ts, reg, st
}

// fixture precondition shared by every test below: the pair really is a
// jaccard-gate collision the way clusterJaccardCollision defines it — near,
// kind-compatible, below threshold — or the tests are not exercising the path
// they claim to.
func requireCollisionFixture(t *testing.T, a, b Candidate) {
	t.Helper()
	if abs(a.Line-b.Line) > DefaultMergeWindow {
		t.Fatalf("fixture broken: lines %d/%d not within DefaultMergeWindow %d", a.Line, b.Line, DefaultMergeWindow)
	}
	if !sameOrUnknownKind(a.DefectKind, b.DefectKind) {
		t.Fatalf("fixture broken: defect kinds %q/%q not compatible", a.DefectKind, b.DefectKind)
	}
	if j := jaccard(descTokens(a.Description), descTokens(b.Description)); j >= mergeSimilarityThreshold {
		t.Fatalf("fixture broken: jaccard=%v >= mergeSimilarityThreshold=%v (would merge without the arbiter)", j, mergeSimilarityThreshold)
	}
}

// dedupVerdictJSON builds a canned dedupArbiterResponse body.
func dedupVerdictJSON(verdict, reasoning string) string {
	return `{"verdict": "` + verdict + `", "reasoning": "` + reasoning + `"}`
}

// TestDedupArbiter_ConfidentYesMerges proves the core merge path: a
// paraphrased duplicate (jaccard below threshold, same locus+kind) that the
// arbiter judges "yes" is folded into the primary's cluster exactly like a
// jaccard-confirmed match — cross-lens corroboration staged in the registry,
// MergedCrossLens counted, and only ONE primary forwarded.
func TestDedupArbiter_ConfidentYesMerges(t *testing.T) {
	ctx := context.Background()
	a, b := dedupCollisionPair()
	requireCollisionFixture(t, a, b)

	client := newScriptedClient().onTaskContains("CANDIDATE A", dedupVerdictJSON("yes", "same nil-deref mechanism, worded differently"))
	ts, reg, st := newDedupTriageState(t, client, DefaultDedupArbiterCap)

	var stats Stats
	var forwarded []Candidate
	for _, c := range []Candidate{a, b} {
		if err := ts.process(ctx, st, &stats, c); err != nil {
			t.Fatalf("process: %v", err)
		}
		forwarded = append(forwarded, ts.popReady()...)
	}
	if len(forwarded) != 1 {
		t.Fatalf("forwarded = %d, want 1 (confident yes must merge b into a's cluster)", len(forwarded))
	}
	if stats.DedupArbiterRuns != 1 {
		t.Errorf("DedupArbiterRuns = %d, want 1", stats.DedupArbiterRuns)
	}
	if stats.DedupArbiterMerges != 1 {
		t.Errorf("DedupArbiterMerges = %d, want 1", stats.DedupArbiterMerges)
	}
	if stats.MergedCrossLens != 1 {
		t.Errorf("MergedCrossLens = %d, want 1 (b's lens differs from a's)", stats.MergedCrossLens)
	}
	staged := reg.DrainStagedLenses(forwarded[0].Fingerprint)
	if len(staged) != 1 || staged[0] != b.Lens {
		t.Errorf("staged corroborating lenses = %v, want [%q]", staged, b.Lens)
	}
	stagedSites := reg.DrainStagedSites(forwarded[0].Fingerprint)
	if len(stagedSites) != 1 || stagedSites[0].File != b.File || stagedSites[0].Line != b.Line {
		t.Errorf("staged sites = %+v, want one site at %s:%d", stagedSites, b.File, b.Line)
	}
}

// TestDedupArbiter_NoKeepsBoth proves a distinct nearby pair the arbiter
// judges "no" survives as two separate findings, not merged.
func TestDedupArbiter_NoKeepsBoth(t *testing.T) {
	ctx := context.Background()
	a, b := dedupCollisionPair()
	requireCollisionFixture(t, a, b)

	client := newScriptedClient().onTaskContains("CANDIDATE A", dedupVerdictJSON("no", "different failure mode: one is a nil deref, the other an unrelated unchecked read"))
	ts, _, st := newDedupTriageState(t, client, DefaultDedupArbiterCap)

	var stats Stats
	var forwarded []Candidate
	for _, c := range []Candidate{a, b} {
		if err := ts.process(ctx, st, &stats, c); err != nil {
			t.Fatalf("process: %v", err)
		}
		forwarded = append(forwarded, ts.popReady()...)
	}
	if len(forwarded) != 2 {
		t.Fatalf("forwarded = %d, want 2 (a confident no must keep both as distinct primaries)", len(forwarded))
	}
	if stats.DedupArbiterRuns != 1 {
		t.Errorf("DedupArbiterRuns = %d, want 1", stats.DedupArbiterRuns)
	}
	if stats.DedupArbiterMerges != 0 {
		t.Errorf("DedupArbiterMerges = %d, want 0", stats.DedupArbiterMerges)
	}
	if stats.MergedCrossLens != 0 || stats.MergedWithinLens != 0 || stats.MergedRootCause != 0 {
		t.Errorf("merges = cross:%d within:%d root:%d, want all 0", stats.MergedCrossLens, stats.MergedWithinLens, stats.MergedRootCause)
	}
}

// TestDedupArbiter_UnsureKeepsBoth proves "unsure" is treated identically to
// "no": both candidates survive, precision-first (a kept near-duplicate only
// costs an extra panel; a wrong merge would bury a real finding).
func TestDedupArbiter_UnsureKeepsBoth(t *testing.T) {
	ctx := context.Background()
	a, b := dedupCollisionPair()
	requireCollisionFixture(t, a, b)

	client := newScriptedClient().onTaskContains("CANDIDATE A", dedupVerdictJSON("unsure", "cannot tell from the excerpt alone"))
	ts, _, st := newDedupTriageState(t, client, DefaultDedupArbiterCap)

	var stats Stats
	var forwarded []Candidate
	for _, c := range []Candidate{a, b} {
		if err := ts.process(ctx, st, &stats, c); err != nil {
			t.Fatalf("process: %v", err)
		}
		forwarded = append(forwarded, ts.popReady()...)
	}
	if len(forwarded) != 2 {
		t.Fatalf("forwarded = %d, want 2 (unsure must keep both)", len(forwarded))
	}
	if stats.DedupArbiterRuns != 1 {
		t.Errorf("DedupArbiterRuns = %d, want 1", stats.DedupArbiterRuns)
	}
	if stats.DedupArbiterMerges != 0 {
		t.Errorf("DedupArbiterMerges = %d, want 0 (unsure is not a merge)", stats.DedupArbiterMerges)
	}
}

// TestDedupArbiter_CapExhausted_PassesThrough proves the per-scan invocation
// cap: once exhausted, a further collision is never sent to the arbiter and
// both candidates pass through kept, exactly like a "no"/"unsure" verdict —
// graceful degradation, never a block or a silent merge.
func TestDedupArbiter_CapExhausted_PassesThrough(t *testing.T) {
	ctx := context.Background()
	a, b := dedupCollisionPair()
	requireCollisionFixture(t, a, b)

	// A "yes" client: if the cap did not gate the call, this test would
	// (wrongly) observe a merge instead of a pass-through.
	client := newScriptedClient().onTaskContains("CANDIDATE A", dedupVerdictJSON("yes", "would merge if invoked"))
	ts, _, st := newDedupTriageState(t, client, 0) // cap already exhausted

	var stats Stats
	var forwarded []Candidate
	for _, c := range []Candidate{a, b} {
		if err := ts.process(ctx, st, &stats, c); err != nil {
			t.Fatalf("process: %v", err)
		}
		forwarded = append(forwarded, ts.popReady()...)
	}
	if len(forwarded) != 2 {
		t.Fatalf("forwarded = %d, want 2 (cap-exhausted collision must pass through, both kept)", len(forwarded))
	}
	if stats.DedupArbiterRuns != 0 {
		t.Errorf("DedupArbiterRuns = %d, want 0 (cap of 0 must prevent every invocation)", stats.DedupArbiterRuns)
	}
	if stats.DedupArbiterSkippedCap != 1 {
		t.Errorf("DedupArbiterSkippedCap = %d, want 1", stats.DedupArbiterSkippedCap)
	}
	if client.callCount() != 0 {
		t.Errorf("client.callCount() = %d, want 0 (cap must gate BEFORE the RunJSON call, not after)", client.callCount())
	}
}

// TestDedupArbiter_NilConfigDisabled proves the zero-value / disabled state:
// every existing newTriageState(snap) test call in this package leaves
// dedupArbiter nil, and a jaccard-gate collision must fall through to today's
// behavior (two separate primaries) with zero LLM calls and zero stats churn.
func TestDedupArbiter_NilConfigDisabled(t *testing.T) {
	ctx := context.Background()
	st, _ := openFixture(t)
	a, b := dedupCollisionPair()
	requireCollisionFixture(t, a, b)

	snap := &ingest.Snapshot{Files: []ingest.File{{Path: "bug.go"}}}
	ts, _ := newTriageState(snap) // dedupArbiter left nil, as every pre-existing test does
	var stats Stats
	var forwarded []Candidate
	for _, c := range []Candidate{a, b} {
		if err := ts.process(ctx, st, &stats, c); err != nil {
			t.Fatalf("process: %v", err)
		}
		forwarded = append(forwarded, ts.popReady()...)
	}
	if len(forwarded) != 2 {
		t.Fatalf("forwarded = %d, want 2 (arbiter disabled: collision falls through unmerged)", len(forwarded))
	}
	if stats.DedupArbiterRuns != 0 || stats.DedupArbiterMerges != 0 || stats.DedupArbiterSkippedCap != 0 {
		t.Errorf("dedup arbiter stats non-zero with no arbiter configured: runs=%d merges=%d skipped=%d",
			stats.DedupArbiterRuns, stats.DedupArbiterMerges, stats.DedupArbiterSkippedCap)
	}
}

// TestDedupArbiter_ExcerptReadFromDisk proves the code excerpt is actually
// read from snap.Root and embedded in the task, and that a missing root
// degrades to an empty excerpt rather than failing the call.
func TestDedupArbiter_ExcerptReadFromDisk(t *testing.T) {
	root := t.TempDir()
	writeFile := func(rel, content string) {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("bug.go", "package x\n\nfunc F() {\n\t_ = 1 // UNIQUE_EXCERPT_MARKER\n}\n")

	excerpt := dedupCodeExcerpt(root, "bug.go", 4, dedupExcerptWindow)
	if !strings.Contains(excerpt, "UNIQUE_EXCERPT_MARKER") {
		t.Errorf("excerpt = %q, want it to contain the source line around line 4", excerpt)
	}

	if got := dedupCodeExcerpt("", "bug.go", 4, dedupExcerptWindow); got != "" {
		t.Errorf("empty root: excerpt = %q, want empty (best-effort degrade)", got)
	}
	if got := dedupCodeExcerpt(root, "missing.go", 4, dedupExcerptWindow); got != "" {
		t.Errorf("missing file: excerpt = %q, want empty (best-effort degrade)", got)
	}
}

// TestDedupArbiter_DurableFold_YesMerges proves the SECOND collision site
// (durableCrossLensFold's SimilarFinding fallback): a WAL-replayed candidate
// at the same locus as a prior-run finding, same defect_kind, different lens,
// whose description jaccard falls short of SimilarFinding's threshold, is
// folded by a confident arbiter "yes" exactly like a SimilarFinding match —
// MergedCrossLensDurable counted, corroborating lens + site attached to the
// EXISTING row (no duplicate).
func TestDedupArbiter_DurableFold_YesMerges(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	const file, lensA, lensB = "x.go", "resource-leaks", "exception-safety"
	const seedLine, candLine = 5, 9
	seedDesc := "the acquired handle a is leaked when use a fails because release is never called on the error path"
	candDesc := "when the call to use throws, nothing runs the cleanup step for the resource opened above"

	root, seedFP, _ := seedPriorRunFinding(t, st, file, lensA, seedDesc, seedLine, candLine)
	if j := jaccard(descTokens(seedDesc), descTokens(candDesc)); j >= mergeSimilarityThreshold {
		t.Fatalf("fixture broken: jaccard=%v >= mergeSimilarityThreshold=%v (SimilarFinding would already merge without the arbiter)", j, mergeSimilarityThreshold)
	}

	client := newScriptedClient().onTaskContains("CANDIDATE A", dedupVerdictJSON("yes", "same leaked-handle defect, described from the error path instead of the acquisition site"))
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: client}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rec := &spendRecorder{ctx: ctx, store: st}
	budget := newBudgetState(0, rec, 1.0)

	snap := &ingest.Snapshot{Root: root, Files: []ingest.File{{Path: file}}}
	ts, _ := newTriageState(snap)
	ts.dedupArbiter = &dedupArbiterConfig{f: f, client: client, budget: budget, root: root, cap: DefaultDedupArbiterCap}

	var stats Stats
	cand := Candidate{
		Lens: lensB, File: file, Line: candLine,
		Title: "cleanup skipped on throw", Description: candDesc,
		Severity: "high", Confidence: "high",
	}
	if err := ts.process(ctx, st, &stats, cand); err != nil {
		t.Fatalf("process: %v", err)
	}

	if ready := ts.popReady(); len(ready) != 0 {
		t.Fatalf("popReady = %d, want 0 (arbiter yes must fold, not forward a second primary): %+v", len(ready), ready)
	}
	if stats.DedupArbiterRuns != 1 {
		t.Errorf("DedupArbiterRuns = %d, want 1", stats.DedupArbiterRuns)
	}
	if stats.DedupArbiterMerges != 1 {
		t.Errorf("DedupArbiterMerges = %d, want 1", stats.DedupArbiterMerges)
	}
	if stats.MergedCrossLensDurable != 1 {
		t.Errorf("MergedCrossLensDurable = %d, want 1", stats.MergedCrossLensDurable)
	}

	got, err := st.GetFindingByFingerprint(ctx, seedFP)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if len(got.CorroboratingLenses) != 1 || got.CorroboratingLenses[0] != lensB {
		t.Errorf("CorroboratingLenses = %v, want [%q]", got.CorroboratingLenses, lensB)
	}
	if len(got.Sites) != 2 {
		t.Errorf("Sites = %+v, want 2 (seed line %d + folded candidate line %d)", got.Sites, seedLine, candLine)
	}
}

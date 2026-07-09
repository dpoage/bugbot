package funnel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// hopSrc is a two-function fixture: Caller calls Callee. Two DIFFERENT files
// (no filename relationship: both are plain .go, which has no paired
// extension in sourceExtensions, so crossFilePeerKeys/5c never bridges them)
// simulate the common Go multi-site shape the bead targets.
const (
	calleeSrc = `package x

func Callee() error {
	return doWork()
}
`
	callerSrc = `package x

func Caller() error {
	return Callee()
}
`
)

// stubRefNav is a scripted codeNavRefs: it records every call (for the
// bounded-query assertion) and returns a canned answer per (file, symbol).
type stubRefNav struct {
	calls   int
	answers map[string][]agent.RefLocation
	err     error
}

func (s *stubRefNav) References(_ context.Context, file string, _ int, symbol string) ([]agent.RefLocation, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.answers[file+"\x00"+symbol], nil
}

// newHopFixture writes calleeSrc/callerSrc under a fresh temp root and
// returns the root plus a snapshot spanning both files.
func newHopFixture(t *testing.T) (root string, snap *ingest.Snapshot) {
	t.Helper()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "callee.go"), []byte(calleeSrc), 0o644); err != nil {
		t.Fatalf("write callee.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "caller.go"), []byte(callerSrc), 0o644); err != nil {
		t.Fatalf("write caller.go: %v", err)
	}
	snap = &ingest.Snapshot{
		Root:  root,
		Files: []ingest.File{{Path: "callee.go"}, {Path: "caller.go"}},
	}
	return root, snap
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return st
}

// TestTriageState_CodeNavHopFold_CallerCalleeSameFinding proves the core
// acceptance scenario: a callee-site candidate and a caller-site candidate,
// same defect_kind, one reference hop apart via the stubbed navigator, fold
// to ONE finding (one forwarded primary) carrying both sites.
func TestTriageState_CodeNavHopFold_CallerCalleeSameFinding(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	_, snap := newHopFixture(t)

	ts, _ := newTriageState(snap)
	nav := &stubRefNav{answers: map[string][]agent.RefLocation{
		// References to Callee (queried when the callee-site candidate is
		// evaluated) include the call site inside Caller — one hop away.
		"callee.go\x00Callee": {{File: "caller.go", Line: 4}},
	}}
	ts.nav = nav

	var stats Stats
	calleeCand := Candidate{
		Lens: "resource-leaks", File: "callee.go", Line: 4,
		Title: "handle leak in Callee", Description: "the handle is never released on the error path in Callee",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectResourceLeak,
	}
	callerCand := Candidate{
		Lens: "exception-safety", File: "caller.go", Line: 4,
		Title: "leak propagates through Caller", Description: "Caller does not release the handle acquired by the callee on error",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectResourceLeak,
	}

	// Caller-site candidate arrives first and becomes the in-run primary
	// target; the callee-site candidate arrives second and is the one whose
	// OWN symbol ("Callee") is queried — find_references(Callee) surfaces the
	// call site inside Caller, one hop away (see codeNavRootCauseFold's doc on
	// the fold's query direction).
	if err := ts.process(ctx, st, &stats, callerCand); err != nil {
		t.Fatalf("process(caller): %v", err)
	}
	forwarded := ts.popReady()
	if err := ts.process(ctx, st, &stats, calleeCand); err != nil {
		t.Fatalf("process(callee): %v", err)
	}
	forwarded = append(forwarded, ts.popReady()...)

	if len(forwarded) != 1 {
		t.Fatalf("forwarded = %d, want 1 (caller/callee fold into one primary): %+v", len(forwarded), forwarded)
	}
	if stats.MergedRootCauseCodeNav != 1 {
		t.Errorf("MergedRootCauseCodeNav = %d, want 1", stats.MergedRootCauseCodeNav)
	}
	// One query per collision: the caller-side candidate's own collision
	// (no target yet) issues one, the callee-side candidate's collision
	// (which finds the fold) issues its own — 2 total, never more than one
	// PER collision.
	if nav.calls != 2 {
		t.Errorf("nav.calls = %d, want 2 (one per collision, never more)", nav.calls)
	}

	// The surviving primary carries its own site; the callee's site is
	// staged in the registry for DrainStagedSites to attach at persist time
	// (mirrors TestTriageState_SameFileSameRootCause's assertion pattern).
	primary := forwarded[0]
	if len(primary.Sites) != 1 || primary.Sites[0] != (Site{File: "caller.go", Line: 4}) {
		t.Errorf("primary.Sites = %+v, want [{caller.go 4}]", primary.Sites)
	}
	staged := ts.registry.DrainStagedSites(primary.Fingerprint)
	if len(staged) != 1 || staged[0] != (domain.Site{File: "callee.go", Line: 4}) {
		t.Errorf("staged sites = %+v, want [{callee.go 4}] (the folded callee-site candidate)", staged)
	}
}

// TestTriageState_CodeNavHopFold_DifferentKindNotMerged proves the kind guard:
// an otherwise one-hop pair with DIFFERENT defect_kind stays separate — two
// primaries are forwarded, and no code-nav fold stat is incremented.
func TestTriageState_CodeNavHopFold_DifferentKindNotMerged(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	_, snap := newHopFixture(t)

	ts, _ := newTriageState(snap)
	nav := &stubRefNav{answers: map[string][]agent.RefLocation{
		"callee.go\x00Callee": {{File: "caller.go", Line: 4}},
	}}
	ts.nav = nav

	var stats Stats
	calleeCand := Candidate{
		Lens: "resource-leaks", File: "callee.go", Line: 4,
		Title: "handle leak in Callee", Description: "the handle is never released on the error path in Callee",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectResourceLeak,
	}
	callerCand := Candidate{
		Lens: "concurrency", File: "caller.go", Line: 4,
		Title: "race in Caller", Description: "Caller reads a shared field without holding the lock other goroutines use",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectRace,
	}

	var forwarded []Candidate
	for _, c := range []Candidate{calleeCand, callerCand} {
		if err := ts.process(ctx, st, &stats, c); err != nil {
			t.Fatalf("process: %v", err)
		}
		forwarded = append(forwarded, ts.popReady()...)
	}

	if len(forwarded) != 2 {
		t.Fatalf("forwarded = %d, want 2 (mismatched defect_kind must never fold)", len(forwarded))
	}
	if stats.MergedRootCauseCodeNav != 0 {
		t.Errorf("MergedRootCauseCodeNav = %d, want 0", stats.MergedRootCauseCodeNav)
	}
}

// TestTriageState_CodeNavHopFold_Bounded proves the memoization: two
// candidates whose OWN enclosing symbol is the same (both reported inside
// Callee, far enough apart / dissimilar enough to miss every earlier merge
// step) each reach the code-nav fold, but the second reuses the cached
// reference query instead of issuing a new one.
func TestTriageState_CodeNavHopFold_Bounded(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	root := t.TempDir()
	// A bigger Callee body so two distinct-description candidates 20 lines
	// apart both resolve to the SAME enclosing symbol locus, and stay far
	// enough apart (> DefaultMergeWindow) plus dissimilar enough that steps
	// 5/5b never merge them against each other first.
	bigCallee := "package x\n\nfunc Callee() error {\n" +
		"\ta := acquireHandle()\n" +
		strings.Repeat("\t_ = a\n", 20) +
		"\treturn releaseHandle(a)\n}\n"
	if err := os.WriteFile(filepath.Join(root, "callee.go"), []byte(bigCallee), 0o644); err != nil {
		t.Fatalf("write callee.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "caller.go"), []byte(callerSrc), 0o644); err != nil {
		t.Fatalf("write caller.go: %v", err)
	}
	snap := &ingest.Snapshot{Root: root, Files: []ingest.File{{Path: "callee.go"}, {Path: "caller.go"}}}

	ts, _ := newTriageState(snap)
	nav := &stubRefNav{answers: map[string][]agent.RefLocation{
		"callee.go\x00Callee": {{File: "caller.go", Line: 4}},
	}}
	ts.nav = nav

	var stats Stats
	// Caller-site primary registered first, so the two Callee-side candidates
	// below both find it as a same-kind fold target.
	callerCand := Candidate{
		Lens: "exception-safety", File: "caller.go", Line: 4,
		Title: "leak propagates through Caller", Description: "Caller never releases the handle acquired by the callee on any path",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectResourceLeak,
	}
	// Distinct Subject on A/B: both share Callee's locus, so without this they
	// would mint the IDENTICAL v3 fingerprint (empty subject) and collide at
	// triage step 3 (exact-fingerprint dedup) before ever reaching step 5e —
	// this test needs both to independently reach the fold.
	calleeCandA := Candidate{
		Lens: "resource-leaks", File: "callee.go", Line: 4,
		Title: "handle acquired without a matching release", Description: "acquireHandle result a is stored but the function may return before release is reached",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectResourceLeak, Subject: "acquireHandle",
	}
	calleeCandB := Candidate{
		Lens: "resource-leaks", File: "callee.go", Line: 25,
		Title: "handle leaked on an alternate exit", Description: "a second unrelated code path near the bottom of the function also skips release entirely",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectResourceLeak, Subject: "releaseHandle",
	}

	var forwarded []Candidate
	for _, c := range []Candidate{callerCand, calleeCandA, calleeCandB} {
		if err := ts.process(ctx, st, &stats, c); err != nil {
			t.Fatalf("process: %v", err)
		}
		forwarded = append(forwarded, ts.popReady()...)
	}

	if len(forwarded) != 1 {
		t.Fatalf("forwarded = %d, want 1 (all three fold into the caller primary): %+v", len(forwarded), forwarded)
	}
	if stats.MergedRootCauseCodeNav != 2 {
		t.Errorf("MergedRootCauseCodeNav = %d, want 2", stats.MergedRootCauseCodeNav)
	}
	// One query for the caller's own (target-less) collision, one for
	// calleeCandA's collision (which finds the fold), and calleeCandB's
	// collision reuses the cache — 2 total, not 3.
	if nav.calls != 2 {
		t.Errorf("nav.calls = %d, want 2 (the repeated symbol's second collision must hit the cache, not issue a 3rd query)", nav.calls)
	}
}

// TestTriageState_CodeNavHopFold_NavErrorSurvivesAsPrimaries proves the
// safety net: when code navigation is unavailable or errors, both candidates
// survive as their own primaries — the fold never blocks or crashes triage.
func TestTriageState_CodeNavHopFold_NavErrorSurvivesAsPrimaries(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	_, snap := newHopFixture(t)

	ts, _ := newTriageState(snap)
	ts.nav = &stubRefNav{err: context.DeadlineExceeded}

	var stats Stats
	calleeCand := Candidate{
		Lens: "resource-leaks", File: "callee.go", Line: 4,
		Title: "handle leak in Callee", Description: "the handle is never released on the error path in Callee",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectResourceLeak,
	}
	callerCand := Candidate{
		Lens: "exception-safety", File: "caller.go", Line: 4,
		Title: "leak propagates through Caller", Description: "Caller does not release the handle acquired by the callee on error",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectResourceLeak,
	}

	var forwarded []Candidate
	for _, c := range []Candidate{calleeCand, callerCand} {
		if err := ts.process(ctx, st, &stats, c); err != nil {
			t.Fatalf("process: %v (nav errors must never fail triage)", err)
		}
		forwarded = append(forwarded, ts.popReady()...)
	}

	if len(forwarded) != 2 {
		t.Fatalf("forwarded = %d, want 2 (nav error must degrade to no-fold, both survive)", len(forwarded))
	}
	if stats.MergedRootCauseCodeNav != 0 {
		t.Errorf("MergedRootCauseCodeNav = %d, want 0", stats.MergedRootCauseCodeNav)
	}
}

// TestTriageState_CodeNavHopFold_NavNilSkips proves the no-nav path: with
// ts.nav left nil (the zero value — code navigation never configured), the
// fold is a pure no-op and both candidates survive as primaries.
func TestTriageState_CodeNavHopFold_NavNilSkips(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	_, snap := newHopFixture(t)

	ts, _ := newTriageState(snap)
	// ts.nav intentionally left nil.

	var stats Stats
	calleeCand := Candidate{
		Lens: "resource-leaks", File: "callee.go", Line: 4,
		Title: "handle leak in Callee", Description: "the handle is never released on the error path in Callee",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectResourceLeak,
	}
	callerCand := Candidate{
		Lens: "exception-safety", File: "caller.go", Line: 4,
		Title: "leak propagates through Caller", Description: "Caller does not release the handle acquired by the callee on error",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectResourceLeak,
	}

	var forwarded []Candidate
	for _, c := range []Candidate{calleeCand, callerCand} {
		if err := ts.process(ctx, st, &stats, c); err != nil {
			t.Fatalf("process: %v", err)
		}
		forwarded = append(forwarded, ts.popReady()...)
	}

	if len(forwarded) != 2 {
		t.Fatalf("forwarded = %d, want 2 (nil nav must skip the fold entirely)", len(forwarded))
	}
}

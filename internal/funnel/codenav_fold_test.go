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

// newCodeNavArbiterConfig builds a dedupArbiterConfig whose single scripted
// verdict is returned for every collision it judges, wired the same way
// run_pipeline.go wires bugbot-ezmx.2's arbiter into a live scan. The
// code-nav fold (step 5e) routes every kind+hop nomination through
// ts.dedupVerdictFor, so any test that expects a fold now needs an arbiter
// that says "yes" — leaving ts.dedupArbiter nil (the pre-arbiter-gating
// default) makes dedupVerdictFor return "", which codeNavRootCauseFold
// treats as "no fold", per its own doc.
func newCodeNavArbiterConfig(t *testing.T, root, verdict, reasoning string) *dedupArbiterConfig {
	t.Helper()
	fst, repo := openFixture(t)
	client := newScriptedClientWithFallback(dedupVerdictJSON(verdict, reasoning))
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: client}, fst, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rec := &spendRecorder{ctx: context.Background(), store: fst}
	budget := newBudgetState(0, rec, 1.0)
	return &dedupArbiterConfig{f: f, client: client, budget: budget, root: root, cap: DefaultDedupArbiterCap}
}

// TestTriageState_CodeNavHopFold_CallerCalleeSameFinding proves the core
// acceptance scenario: a callee-site candidate and a caller-site candidate,
// same defect_kind, one reference hop apart via the stubbed navigator, and a
// confident arbiter "yes" on the nominated pair, fold to ONE finding (one
// forwarded primary) carrying both sites. The reference hop alone is only a
// nomination (BLOCKING-1) — this is the genuine same-defect yes-path the
// nomination is meant to catch.
func TestTriageState_CodeNavHopFold_CallerCalleeSameFinding(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	root, snap := newHopFixture(t)

	ts, _ := newTriageState(snap)
	nav := &stubRefNav{answers: map[string][]agent.RefLocation{
		// References to Callee (queried when the callee-site candidate is
		// evaluated) include the call site inside Caller — one hop away.
		"callee.go\x00Callee": {{File: "caller.go", Line: 4}},
	}}
	ts.nav = nav
	ts.dedupArbiter = newCodeNavArbiterConfig(t, root, "yes", "the caller propagates the exact leak reported inside the callee")

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

// TestTriageState_CodeNavHopFold_Bounded proves both the memoization AND
// BLOCKING-1's nomination-not-decision guarantee: two candidates whose OWN
// enclosing symbol is the same (both reported inside Callee, far enough
// apart / dissimilar enough to miss every earlier merge step) each reach the
// code-nav fold and are nominated against the caller primary, but the
// arbiter judges the pair an admittedly-distinct pair of defects ("no") —
// both stay separate, and the second candidate's reference query still
// reuses the cache instead of issuing a new one.
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
	ts.dedupArbiter = newCodeNavArbiterConfig(t, root, "no", "the caller's leak and the callee's two internal paths are independent defects that merely happen to be one hop apart")

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

	if len(forwarded) != 3 {
		t.Fatalf("forwarded = %d, want 3 (arbiter no on an admittedly-distinct pair must keep every candidate separate): %+v", len(forwarded), forwarded)
	}
	if stats.MergedRootCauseCodeNav != 0 {
		t.Errorf("MergedRootCauseCodeNav = %d, want 0 (a hop is a nomination, never a decision)", stats.MergedRootCauseCodeNav)
	}
	// Both callee-side candidates are nominated against the caller primary
	// and routed through the arbiter, which says "no" both times.
	if stats.DedupArbiterRuns != 2 {
		t.Errorf("DedupArbiterRuns = %d, want 2 (calleeCandA and calleeCandB each nominate against the caller primary)", stats.DedupArbiterRuns)
	}
	if stats.DedupArbiterMerges != 0 {
		t.Errorf("DedupArbiterMerges = %d, want 0", stats.DedupArbiterMerges)
	}
	// One query for the caller's own (target-less) collision, one for
	// calleeCandA's collision (which finds the hop), and calleeCandB's
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

// TestTriageState_CodeNavHopFold_ReverifyNeverNominates proves BLOCKING-2:
// a Reverify candidate — one re-judging its own durable row — must never be
// absorbed into a different finding via the code-nav hop, mirroring
// durableCrossLensFold's own "Reverify owns its durable row" invariant. Even
// with an arbiter that would say "yes" and an otherwise-matching kind+hop
// pair, the Reverify candidate must survive as its own primary, and its own
// enclosing symbol must never even be queried (the guard trips before refs()).
func TestTriageState_CodeNavHopFold_ReverifyNeverNominates(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	root, snap := newHopFixture(t)

	ts, _ := newTriageState(snap)
	nav := &stubRefNav{answers: map[string][]agent.RefLocation{
		"callee.go\x00Callee": {{File: "caller.go", Line: 4}},
	}}
	ts.nav = nav
	ts.dedupArbiter = newCodeNavArbiterConfig(t, root, "yes", "would merge if the Reverify guard did not gate it first")

	var stats Stats
	callerCand := Candidate{
		Lens: "exception-safety", File: "caller.go", Line: 4,
		Title: "leak propagates through Caller", Description: "Caller does not release the handle acquired by the callee on error",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectResourceLeak,
	}
	calleeCand := Candidate{
		Lens: "resource-leaks", File: "callee.go", Line: 4,
		Title: "handle leak in Callee", Description: "the handle is never released on the error path in Callee",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectResourceLeak,
		Reverify: true,
	}

	var forwarded []Candidate
	for _, c := range []Candidate{callerCand, calleeCand} {
		if err := ts.process(ctx, st, &stats, c); err != nil {
			t.Fatalf("process: %v", err)
		}
		forwarded = append(forwarded, ts.popReady()...)
	}

	if len(forwarded) != 2 {
		t.Fatalf("forwarded = %d, want 2 (a Reverify candidate must never be absorbed into another finding)", len(forwarded))
	}
	if stats.MergedRootCauseCodeNav != 0 {
		t.Errorf("MergedRootCauseCodeNav = %d, want 0", stats.MergedRootCauseCodeNav)
	}
	if stats.DedupArbiterRuns != 0 {
		t.Errorf("DedupArbiterRuns = %d, want 0 (the Reverify guard must trip before any nomination reaches the arbiter)", stats.DedupArbiterRuns)
	}
	// Only the caller's own (target-less) collision issues a query; the
	// Reverify candidate's guard trips before refs() is ever called.
	if nav.calls != 1 {
		t.Errorf("nav.calls = %d, want 1 (Reverify candidate must never issue a reference query)", nav.calls)
	}
}

// seedPriorRunHopFinding writes the caller/callee fixture under a fresh temp
// root and persists an OPEN, prior-run finding at the caller's call-site
// locus (lensA), exactly as bugbot-ezmx.3's durable-branch precondition
// requires: a persisted row this run's own in-memory clustering cannot see.
func seedPriorRunHopFinding(t *testing.T, st *store.Store, lensA, desc string) (root string, seedFP string) {
	t.Helper()
	root, snap := newHopFixture(t)
	_ = snap
	const file, line = "caller.go", 4
	resolver := NewLocusResolver(root)
	locus := resolver.Resolve(file, line)
	seedFP = domain.Fingerprint(lensA, file, locus)
	if _, err := st.UpsertFinding(context.Background(), domain.Finding{
		Fingerprint: seedFP,
		LocusKey:    domain.LocusKey(file, locus),
		Title:       "leak propagates through Caller",
		Description: desc,
		Severity:    "high",
		Tier:        domain.TierVerified,
		Status:      domain.StatusOpen,
		Lens:        lensA,
		DefectKind:  domain.DefectResourceLeak,
		File:        file,
		Line:        line,
		Sites:       []domain.Site{{File: file, Line: line}},
	}); err != nil {
		t.Fatalf("seed prior-run finding: %v", err)
	}
	return root, seedFP
}

// TestTriageState_CodeNavHopFold_DurablePersistedOpenFinding proves
// BLOCKING-3: the durable branch of step 5e. A finding persisted by a prior
// run at the caller's locus (same defect_kind) is folded against by a fresh
// callee-site candidate whose stubbed reference hop lands inside the caller
// — but ONLY once the nominated pair clears the arbiter with a confident
// "yes". The candidate is absorbed into the PERSISTED row (not forwarded as
// a second primary), MergedCrossLensDurableCodeNav is the ONLY code-nav stat
// incremented (never MergedRootCauseCodeNav, since there is no in-run
// cluster target), DuplicateRate's numerator is unaffected by the durable
// increment (mirrors MergedCrossLensDurable's own exclusion), and the site +
// corroborating lens land on the persisted row.
func TestTriageState_CodeNavHopFold_DurablePersistedOpenFinding(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	const lensA, lensB = "exception-safety", "resource-leaks"
	desc := "Caller does not release the handle acquired by the callee on error"
	root, seedFP := seedPriorRunHopFinding(t, st, lensA, desc)

	// Fresh triageState over the same snapshot: no in-memory cluster knows
	// about the prior-run primary — only the durable store does.
	_, snap := newHopFixture(t)
	snap.Root = root
	ts, _ := newTriageState(snap)
	nav := &stubRefNav{answers: map[string][]agent.RefLocation{
		// The callee candidate's OWN symbol ("Callee") is queried; its
		// references include the persisted finding's call site.
		"callee.go\x00Callee": {{File: "caller.go", Line: 4}},
	}}
	ts.nav = nav
	ts.dedupArbiter = newCodeNavArbiterConfig(t, root, "yes", "the callee-side report is the same leak already tracked at the caller's call site")

	var stats Stats
	stats.Hypothesized = 1 // non-zero pool so DuplicateRate is meaningful
	calleeCand := Candidate{
		Lens: lensB, File: "callee.go", Line: 4,
		Title: "handle leak in Callee", Description: "the handle is never released on the error path in Callee",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectResourceLeak,
	}
	if err := ts.process(ctx, st, &stats, calleeCand); err != nil {
		t.Fatalf("process: %v", err)
	}

	if ready := ts.popReady(); len(ready) != 0 {
		t.Fatalf("popReady = %d, want 0 (durable fold must not forward a second primary): %+v", len(ready), ready)
	}
	if stats.MergedCrossLensDurableCodeNav != 1 {
		t.Errorf("MergedCrossLensDurableCodeNav = %d, want 1", stats.MergedCrossLensDurableCodeNav)
	}
	if stats.MergedRootCauseCodeNav != 0 {
		t.Errorf("MergedRootCauseCodeNav = %d, want 0 (no in-run cluster target exists)", stats.MergedRootCauseCodeNav)
	}
	if got := stats.DuplicateRate(); got != 0 {
		t.Errorf("DuplicateRate = %v, want 0 (MergedCrossLensDurableCodeNav must stay excluded from the numerator)", got)
	}

	got, err := st.GetFindingByFingerprint(ctx, seedFP)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if len(got.CorroboratingLenses) != 1 || got.CorroboratingLenses[0] != lensB {
		t.Errorf("CorroboratingLenses = %v, want [%q]", got.CorroboratingLenses, lensB)
	}
	if len(got.Sites) != 2 {
		t.Errorf("Sites = %+v, want 2 (seed site + folded callee-site candidate)", got.Sites)
	}
}

// TestTriageState_CodeNavHopFold_ExcludedKindsNeverNominate proves the
// catch-all-kind guard: "logic" and "other" are low-signal enough (almost
// anything can be tagged one when a finder is unsure) that step 5e must
// never nominate on them, even with a matching kind and a genuine reference
// hop, and even with an arbiter that would say "yes". Both survive as their
// own primaries, no arbiter turn is spent, and the excluded candidate's own
// symbol is never queried (the guard trips before refs()).
func TestTriageState_CodeNavHopFold_ExcludedKindsNeverNominate(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	root, snap := newHopFixture(t)

	ts, _ := newTriageState(snap)
	nav := &stubRefNav{answers: map[string][]agent.RefLocation{
		"callee.go\x00Callee": {{File: "caller.go", Line: 4}},
	}}
	ts.nav = nav
	ts.dedupArbiter = newCodeNavArbiterConfig(t, root, "yes", "would merge if the excluded-kind guard did not gate it first")

	var stats Stats
	callerCand := Candidate{
		Lens: "exception-safety", File: "caller.go", Line: 4,
		Title: "leak propagates through Caller", Description: "Caller does not release the handle acquired by the callee on error",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectLogic,
	}
	calleeCand := Candidate{
		Lens: "resource-leaks", File: "callee.go", Line: 4,
		Title: "handle leak in Callee", Description: "the handle is never released on the error path in Callee",
		Severity: "high", Confidence: "high", DefectKind: domain.DefectLogic,
	}

	var forwarded []Candidate
	for _, c := range []Candidate{callerCand, calleeCand} {
		if err := ts.process(ctx, st, &stats, c); err != nil {
			t.Fatalf("process: %v", err)
		}
		forwarded = append(forwarded, ts.popReady()...)
	}

	if len(forwarded) != 2 {
		t.Fatalf("forwarded = %d, want 2 (the catch-all \"logic\" kind must never nominate)", len(forwarded))
	}
	if stats.MergedRootCauseCodeNav != 0 {
		t.Errorf("MergedRootCauseCodeNav = %d, want 0", stats.MergedRootCauseCodeNav)
	}
	if stats.DedupArbiterRuns != 0 {
		t.Errorf("DedupArbiterRuns = %d, want 0 (excluded-kind guard must trip before any nomination reaches the arbiter)", stats.DedupArbiterRuns)
	}
	if nav.calls != 0 {
		t.Errorf("nav.calls = %d, want 0 (neither candidate's kind is eligible to nominate, so no query is ever issued)", nav.calls)
	}
}

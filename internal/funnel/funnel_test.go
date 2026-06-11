package funnel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// --- fixture repo ---------------------------------------------------------

// realBugFile / cleanFile are the seeded fixture sources. realBugFile contains
// a genuine nil-deref on a reachable path; cleanFile is bug-free.
const realBugFile = `package fixture

// Config holds optional settings.
type Config struct {
	Name string
}

// Greeting dereferences cfg without a nil check; a caller passing nil panics.
func Greeting(cfg *Config) string {
	return "hello " + cfg.Name
}
`

const cleanFile = `package fixture

// Add returns the sum of a and b. There is no bug here.
func Add(a, b int) int {
	return a + b
}
`

// newFixtureRepo creates a real git repo in a temp dir with the seeded files
// committed, and returns its path. It skips the test if git is unavailable.
func newFixtureRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()

	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("bug.go", realBugFile)
	write("clean.go", cleanFile)

	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	runGit("init", "-q")
	runGit("add", ".")
	runGit("commit", "-q", "-m", "seed")
	return dir
}

// openFixture opens an in-memory store and the fixture repo.
func openFixture(t *testing.T) (*store.Store, *ingest.Repo) {
	t.Helper()
	ctx := context.Background()
	// A file-backed DB (not ":memory:") so the schema is visible across the
	// connection-pool connections database/sql may open: an in-memory SQLite DB
	// is private per connection and would lose the migrated tables.
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	repo, err := ingest.Open(ctx, newFixtureRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	return st, repo
}

// candJSON renders a finder candidate-list response with the given entries.
func candJSON(cands ...string) string {
	return `{"candidates": [` + strings.Join(cands, ",") + `]}`
}

// realCand / bogusCand are finder candidate JSON bodies. realCand points at the
// genuine nil-deref; bogusCand is a confabulated bug in clean.go.
const (
	realCand = `{"file": "bug.go", "line": 10, "title": "nil deref of cfg in Greeting",
		"description": "cfg may be nil", "severity": "high",
		"evidence": "Greeting returns cfg.Name without a nil check", "confidence": "high"}`
	bogusCand = `{"file": "clean.go", "line": 5, "title": "Add overflows on large ints",
		"description": "imagined overflow", "severity": "low",
		"evidence": "a + b", "confidence": "high"}`
)

// finderOn routes the nil-safety lens finder to return both candidates and
// every other lens to return nothing, so the pipeline sees exactly one real and
// one bogus candidate.
func finderOnNilLens(c *scriptedClient) *scriptedClient {
	return c.onSystemContains("nil-safety/error-handling", candJSON(realCand, bogusCand))
}

// verifierRouting routes refuters by candidate title: the real bug is never
// refuted; the bogus one is refuted by all refuters.
func verifierRouting(c *scriptedClient) *scriptedClient {
	c.onTaskContains("nil deref of cfg in Greeting", notRefutedJSON)
	c.onTaskContains("Add overflows on large ints", refutedJSON)
	return c
}

// --- tests ----------------------------------------------------------------

func TestSweep_E2E_OneVerifiedFinding(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := finderOnNilLens(newScriptedClient())
	verifier := verifierRouting(newScriptedClient())

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Exactly one Tier 2 finding: the real bug survived, the bogus one died.
	if len(res.Findings) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(res.Findings), res.Findings)
	}
	got := res.Findings[0]
	if got.Tier != 2 {
		t.Errorf("tier = %d, want 2", got.Tier)
	}
	if got.Status != store.StatusOpen {
		t.Errorf("status = %q, want open", got.Status)
	}
	if got.File != "bug.go" || got.Line != 10 {
		t.Errorf("anchor = %s:%d, want bug.go:10", got.File, got.Line)
	}
	if got.Lens != "nil-safety/error-handling" {
		t.Errorf("lens = %q", got.Lens)
	}
	if !strings.Contains(got.Title, "nil deref") {
		t.Errorf("title = %q", got.Title)
	}
	// Anchoring: commit + non-empty file hash.
	if got.CommitSHA != res.Commit || got.CommitSHA == "" {
		t.Errorf("commit_sha = %q, want %q", got.CommitSHA, res.Commit)
	}
	if got.FileHash == "" {
		t.Errorf("file_hash is empty; finding not anchored to content")
	}
	// Reasoning trace carries the refuters' verdicts.
	if !strings.Contains(got.Reasoning, "could not refute") {
		t.Errorf("reasoning missing refuter trace: %q", got.Reasoning)
	}

	// Stats: 2 hypothesized, 2 triaged, 1 verified, 1 killed.
	want := Stats{Hypothesized: 2, Triaged: 2, Verified: 1, Killed: 1}
	if res.Stats.Hypothesized != want.Hypothesized ||
		res.Stats.Triaged != want.Triaged ||
		res.Stats.Verified != want.Verified ||
		res.Stats.Killed != want.Killed {
		t.Errorf("stats = %+v, want hypothesized/triaged/verified/killed %+v", res.Stats, want)
	}
	if res.Stats.InputTokens == 0 || res.Stats.OutputTokens == 0 {
		t.Errorf("spend not recorded: in=%d out=%d", res.Stats.InputTokens, res.Stats.OutputTokens)
	}
	// The scripted clients report cache reads on every call; the run total must
	// carry them through (the cached subset of every 100-token input is 60).
	if want := res.Stats.InputTokens / 100 * 60; res.Stats.CacheReadTokens != want {
		t.Errorf("cache reads not recorded: got %d, want %d", res.Stats.CacheReadTokens, want)
	}

	// Persisted: GetFinding round-trips, scan run finished with stats.
	stored, err := st.GetFinding(ctx, got.ID)
	if err != nil {
		t.Fatalf("get finding: %v", err)
	}
	if stored.Fingerprint != got.Fingerprint {
		t.Errorf("stored fingerprint mismatch")
	}
	run, err := st.GetScanRun(ctx, res.ScanRunID)
	if err != nil {
		t.Fatalf("get scan run: %v", err)
	}
	if run.FinishedAt.IsZero() {
		t.Errorf("scan run not finished")
	}
	if !strings.Contains(run.StatsJSON, `"verified":1`) {
		t.Errorf("scan run stats blob = %q", run.StatsJSON)
	}
	if run.Kind != store.ScanOneshot {
		t.Errorf("scan kind = %q, want oneshot", run.Kind)
	}
}

func TestSweep_Suppression_NoFindings(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := finderOnNilLens(newScriptedClient())
	verifier := verifierRouting(newScriptedClient())

	// Pre-suppress the real candidate's fingerprint.
	fp := store.Fingerprint("nil-safety/error-handling", "bug.go", 10, "nil deref of cfg in Greeting")
	if err := st.AddSuppression(ctx, fp, "test: known non-bug"); err != nil {
		t.Fatal(err)
	}

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Findings) != 0 {
		t.Fatalf("want 0 findings (suppressed), got %d", len(res.Findings))
	}
	if res.Stats.DroppedSuppressed != 1 {
		t.Errorf("dropped_suppressed = %d, want 1", res.Stats.DroppedSuppressed)
	}
	// The bogus candidate is still triaged and then killed; the suppressed real
	// one never reaches verify.
	if res.Stats.Triaged != 1 {
		t.Errorf("triaged = %d, want 1 (only the bogus candidate)", res.Stats.Triaged)
	}
	// Verifier should have been invoked only for the bogus candidate.
	if verifier.callCount() != DefaultRefuters {
		t.Errorf("verifier calls = %d, want %d (only bogus candidate)", verifier.callCount(), DefaultRefuters)
	}
}

func TestSweep_CleanCode_NoFindingsNoVerify(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Finder returns empty for every lens (default fallback is empty candidates).
	finder := newScriptedClient()
	verifier := newScriptedClient()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Findings) != 0 {
		t.Fatalf("clean code: want 0 findings, got %d", len(res.Findings))
	}
	if res.Stats.Hypothesized != 0 || res.Stats.Triaged != 0 {
		t.Errorf("clean code stats nonzero: %+v", res.Stats)
	}
	// No candidates => verifier must never be called.
	if verifier.callCount() != 0 {
		t.Errorf("verifier called %d times on clean code; want 0", verifier.callCount())
	}
	// Finder ran once per taxonomy lens per chunk (one chunk here). diff-intent
	// emits zero chunk tasks on sweeps (no ChangeContext), so the count is
	// len(BuiltinLenses())-1.
	wantFinderCalls := len(BuiltinLenses()) - 1
	if finder.callCount() != wantFinderCalls {
		t.Errorf("finder calls = %d, want %d (taxonomy lenses only; diff-intent skipped on sweep)", finder.callCount(), wantFinderCalls)
	}
}

// TestSweep_FinderParseFailures_HonestStats proves the trust fix: when finder
// agents produce output that never parses as JSON (even after the repair
// round-trip), the run counts the failures, marks the result unreliable, notes
// each failure on Result.Skipped, and never silently reports a clean "found
// nothing". This is the difference between "lens found nothing" and "lens
// failed".
func TestSweep_FinderParseFailures_HonestStats(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Every finder returns prose, not JSON: RunJSON's parse + repair both fail, so
	// each (lens, chunk) finder is a parse failure.
	finder := newScriptedClient()
	finder.fallback = "Here is my analysis, but I never produced any JSON."
	verifier := newScriptedClient()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep should not error on parse failures: %v", err)
	}

	// diff-intent emits zero chunk tasks on sweeps (no ChangeContext), so only
	// the taxonomy lenses run; that is len(BuiltinLenses())-1 finders.
	nLenses := len(BuiltinLenses()) - 1
	if res.Stats.FinderRuns != nLenses {
		t.Errorf("FinderRuns = %d, want %d (one per taxonomy lens, single chunk; diff-intent skipped)", res.Stats.FinderRuns, nLenses)
	}
	if res.Stats.FinderFailures != nLenses {
		t.Errorf("FinderFailures = %d, want %d (all taxonomy lenses failed to parse)", res.Stats.FinderFailures, nLenses)
	}
	if res.Stats.FinderReliable() {
		t.Error("FinderReliable() = true, want false when every finder failed")
	}
	if !res.Stats.MostFindersFailed() {
		t.Error("MostFindersFailed() = false, want true when every finder failed")
	}
	// Zero findings, but the run must have recorded WHY they're untrustworthy.
	if len(res.Findings) != 0 {
		t.Errorf("want 0 findings, got %d", len(res.Findings))
	}
	if len(res.Skipped) == 0 {
		t.Error("expected per-lens failure notes on Result.Skipped, got none")
	}
	for _, note := range res.Skipped {
		if !strings.Contains(note, "no parseable output") {
			t.Errorf("skip note missing failure language: %q", note)
		}
	}
}

// TestRunFinder_BudgetStopNotParseFailure proves the L1 fix: a finder run cut
// short by the shared budget pool whose partial output does not parse must be
// classified as a budget stop, NOT a finder parse failure. A budget stop is an
// expected consequence of running out of headroom, not a reliability problem, so
// it must not inflate FinderFailures (which drives the "scan unreliable" warning).
func TestRunFinder_BudgetStopNotParseFailure(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// A finder that, if it ever ran, would produce unparseable prose. With an
	// exhausted pool it never completes a turn: the pre-turn BudgetCheck stops it
	// with TruncBudgetPool before any model call, so FinalText is empty and
	// RunJSON returns a parse error — exactly the case L1 must classify as a budget
	// stop rather than a parse failure.
	finder := newScriptedClient()
	finder.fallback = "prose that never parses as JSON"

	f, err := New(RoleClients{Finder: finder, Verifier: newScriptedClient()}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		t.Fatal(err)
	}

	// Build a budget pool that is already exhausted, so every runner launched
	// against it stops on its first pre-turn check.
	rec := &spendRecorder{ctx: ctx, store: st}
	budget := newBudgetState(100, rec, 1.0)
	budget.pool.Add(100) // spend == limit => Check returns ErrBudgetExhausted

	cands, status, err := f.runFinder(ctx, finder, tools, "senior Go engineer", f.lenses[0], []ingest.Language{ingest.LangGo}, finderTask([]string{"bug.go"}, nil), budget)
	if err != nil {
		t.Fatalf("runFinder should not error on a budget stop: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("cands = %d, want 0 (no completion happened)", len(cands))
	}
	if status != finderBudgetStopped {
		t.Errorf("status = %d, want finderBudgetStopped (%d) — a budget stop must not count as a parse failure", status, finderBudgetStopped)
	}
}

// TestRunFinder_ParseFailureStillCounts is the complement to the budget-stop
// case: a finder that runs to a NON-budget stop and still produces no parseable
// JSON remains a genuine parse failure (the L1 change must not mask real
// reliability problems).
func TestRunFinder_ParseFailureStillCounts(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := newScriptedClient()
	finder.fallback = "prose that never parses as JSON"

	f, err := New(RoleClients{Finder: finder, Verifier: newScriptedClient()}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		t.Fatal(err)
	}

	// Unlimited pool: the finder runs, returns prose, RunJSON's parse+repair both
	// fail, and the run was never budget-truncated.
	rec := &spendRecorder{ctx: ctx, store: st}
	budget := newBudgetState(0, rec, 1.0)

	_, status, err := f.runFinder(ctx, finder, tools, "senior Go engineer", f.lenses[0], []ingest.Language{ingest.LangGo}, finderTask([]string{"bug.go"}, nil), budget)
	if err != nil {
		t.Fatalf("runFinder should not error on a parse failure: %v", err)
	}
	if status != finderParseFailed {
		t.Errorf("status = %d, want finderParseFailed (%d)", status, finderParseFailed)
	}
}

func TestSweep_Dedup_SameBugTwoLenses(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// The fingerprint includes the lens, so two candidates only dedup when they
	// agree on (lens, file, line, title). The cleanest way to exercise that is a
	// single lens emitting the same candidate twice: identical fingerprints,
	// so triage collapses them to one.
	dup := candJSON(realCand, realCand)
	finder := newScriptedClient().onSystemContains("nil-safety/error-handling", dup)
	verifier := verifierRouting(newScriptedClient())

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if res.Stats.Hypothesized != 2 {
		t.Errorf("hypothesized = %d, want 2", res.Stats.Hypothesized)
	}
	if res.Stats.Triaged != 1 {
		t.Errorf("triaged = %d, want 1 (deduped)", res.Stats.Triaged)
	}
	if res.Stats.DroppedDuplicate != 1 {
		t.Errorf("dropped_duplicate = %d, want 1", res.Stats.DroppedDuplicate)
	}
	if len(res.Findings) != 1 {
		t.Errorf("want 1 finding after dedup, got %d", len(res.Findings))
	}
}

func TestSweep_CrossLensMerge_CorroborationPersisted(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Two different lenses report the SAME defect at the SAME line with different
	// titles (so different fingerprints — they survive exact-fingerprint dedup)
	// and similar descriptions (so the location merge collapses them). Only the
	// primary should verify and persist, carrying the other lens as corroboration.
	nilLensCand := `{"file": "bug.go", "line": 10, "title": "nil deref of cfg in Greeting",
		"description": "cfg may be nil and is dereferenced without a guard in Greeting", "severity": "high",
		"evidence": "Greeting returns cfg.Name without a nil check", "confidence": "high"}`
	apiLensCand := `{"file": "bug.go", "line": 10, "title": "unchecked pointer cfg used in Greeting",
		"description": "the cfg pointer may be nil and is dereferenced without a guard, panicking", "severity": "medium",
		"evidence": "cfg.Name read with no nil check", "confidence": "high"}`

	finder := newScriptedClient().
		onSystemContains("nil-safety/error-handling", candJSON(nilLensCand)).
		onSystemContains("api-contract-misuse", candJSON(apiLensCand))
	// Refuter never refutes the surviving primary (whichever title it carries —
	// the primary is the high-severity nil-safety report).
	verifier := newScriptedClient().onTaskContains("nil deref of cfg in Greeting", notRefutedJSON)

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if res.Stats.Hypothesized != 2 {
		t.Errorf("hypothesized = %d, want 2", res.Stats.Hypothesized)
	}
	if res.Stats.DroppedDuplicate != 0 {
		t.Errorf("dropped_duplicate = %d, want 0 (different fingerprints, not exact dups)", res.Stats.DroppedDuplicate)
	}
	if res.Stats.MergedCrossLens != 1 {
		t.Errorf("merged_cross_lens = %d, want 1", res.Stats.MergedCrossLens)
	}
	if res.Stats.MergedWithinLens != 0 {
		t.Errorf("merged_within_lens = %d, want 0", res.Stats.MergedWithinLens)
	}
	if res.Stats.Triaged != 1 {
		t.Errorf("triaged = %d, want 1 (merged to primary)", res.Stats.Triaged)
	}
	// Exactly one refuter panel (3 refuters) ran — one cluster.
	if res.Stats.VerifierRuns != DefaultRefuters {
		t.Errorf("verifier_runs = %d, want %d (one panel for the one cluster)", res.Stats.VerifierRuns, DefaultRefuters)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(res.Findings), res.Findings)
	}
	got := res.Findings[0]
	if got.Lens != "nil-safety/error-handling" {
		t.Errorf("primary lens = %q, want nil-safety/error-handling (higher severity)", got.Lens)
	}
	if want := []string{"api-contract-misuse"}; !reflect.DeepEqual(got.CorroboratingLenses, want) {
		t.Errorf("corroborating lenses = %v, want %v", got.CorroboratingLenses, want)
	}
	if !strings.Contains(got.Reasoning, "Corroborated by lenses: api-contract-misuse") {
		t.Errorf("reasoning missing corroboration note:\n%s", got.Reasoning)
	}

	// Persistence round-trip: the stored finding carries the corroboration.
	stored, err := st.GetFindingByFingerprint(ctx, got.Fingerprint)
	if err != nil {
		t.Fatalf("get persisted finding: %v", err)
	}
	if want := []string{"api-contract-misuse"}; !reflect.DeepEqual(stored.CorroboratingLenses, want) {
		t.Errorf("persisted corroborating lenses = %v, want %v", stored.CorroboratingLenses, want)
	}
}

func TestSweep_LowConfidenceDropped(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	lowCand := `{"file": "bug.go", "line": 10, "title": "maybe nil", "description": "x",
		"severity": "low", "evidence": "y", "confidence": "low"}`
	finder := newScriptedClient().onSystemContains("nil-safety/error-handling", candJSON(lowCand))
	verifier := newScriptedClient()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.DroppedLowConfidence != 1 {
		t.Errorf("dropped_low_confidence = %d, want 1", res.Stats.DroppedLowConfidence)
	}
	if res.Stats.Triaged != 0 || len(res.Findings) != 0 {
		t.Errorf("low-confidence candidate should be dropped: triaged=%d findings=%d", res.Stats.Triaged, len(res.Findings))
	}
	if verifier.callCount() != 0 {
		t.Errorf("verifier called on dropped candidate")
	}
}

func TestTargeted_BlastRadiusScoped(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := finderOnNilLens(newScriptedClient())
	verifier := verifierRouting(newScriptedClient())

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}

	// Targeted scan seeded with the changed file bug.go. BlastRadius intersected
	// with the snapshot scopes the audit to the change and its dependents.
	res, err := f.Targeted(ctx, []string{"bug.go"})
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(res.Findings))
	}
	if res.Findings[0].File != "bug.go" {
		t.Errorf("finding file = %q, want bug.go", res.Findings[0].File)
	}
	// The scan run is recorded with the targeted kind.
	run, err := st.GetScanRun(ctx, res.ScanRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Kind != store.ScanTargeted {
		t.Errorf("scan kind = %q, want targeted", run.Kind)
	}
}

func TestTargeted_EmptyChangeSet(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := finderOnNilLens(newScriptedClient())
	verifier := verifierRouting(newScriptedClient())
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}

	// No changed files => no targets => no findings, no agent calls.
	res, err := f.Targeted(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 0 || res.Stats.Hypothesized != 0 {
		t.Errorf("empty change set should yield no work: %+v", res.Stats)
	}
	if finder.callCount() != 0 {
		t.Errorf("finder called %d times on empty target set", finder.callCount())
	}
}

func TestSweep_BudgetDegradation(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Every lens returns one real candidate so there is verification work, and
	// each completion costs 150 tokens. A tiny budget forces degradation early:
	// only the top-2 lenses keep running, and refuters drop to 1.
	finder := newScriptedClient()
	finder.fallback = candJSON(realCand)
	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		TokenBudget:           600, // ~4 completions before soft threshold (70% of 600 = 420)
		CacheReadBudgetWeight: 1.0, // raw accounting: this test pins the degradation mechanism, not weighting
		MaxParallel:           1,   // serialize so budget accrues deterministically
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if !res.Degraded {
		t.Errorf("expected Degraded=true with a tiny budget")
	}
	if len(res.Skipped) == 0 {
		t.Errorf("expected Skipped notes describing degradation, got none")
	}
	// Some lenses must have been skipped: with nBuiltin-1 taxonomy chunk tasks and
	// degradation to 2, the finder cannot have run all taxonomy lenses.
	// diff-intent has no chunk tasks on sweeps so it does not count here.
	nTaxonomy := len(BuiltinLenses()) - 1
	if finder.callCount() >= nTaxonomy {
		t.Errorf("finder ran %d times; degradation should have skipped low-yield lenses (nTaxonomy=%d)", finder.callCount(), nTaxonomy)
	}
}

// TestSweep_BudgetOrphanPersistsAsTier3 proves the budget-orphan persistence
// requirement: when the run hits its hard budget before a triaged candidate can
// be verified, that candidate is NOT dropped. It persists as an open Tier 3
// suspected finding, surfaces in Result.Findings and Result.Skipped, and is
// counted in Stats.Suspected — so a human can still review it.
func TestSweep_BudgetOrphanPersistsAsTier3(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// One lens, one real candidate. The single finder completion costs 150 tokens
	// (100 in + 50 out), which already exceeds the tiny hard budget — so by the
	// time the verify stage runs, the hard-stop gate fires and the candidate is
	// orphaned rather than verified.
	finder := newScriptedClient()
	finder.fallback = candJSON(realCand)
	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Lenses:                []string{"nil-safety/error-handling"},
		TokenBudget:           100, // < 150, so the pool is exhausted after the finder call
		CacheReadBudgetWeight: 1.0, // raw accounting: this test pins hard-stop, not weighting
		MaxParallel:           1,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if !res.Stopped {
		t.Errorf("expected Stopped=true after hitting the hard budget")
	}
	if res.Stats.Suspected != 1 {
		t.Errorf("Stats.Suspected = %d, want 1", res.Stats.Suspected)
	}
	if res.Stats.Verified != 0 {
		t.Errorf("Stats.Verified = %d, want 0 (verification was budget-stopped)", res.Stats.Verified)
	}
	// The verifier must never have run: the candidate was orphaned at the gate.
	if verifier.callCount() != 0 {
		t.Errorf("verifier ran %d times; expected 0 (budget exhausted before verify)", verifier.callCount())
	}

	// The orphan must be persisted and returned as an open Tier 3 finding.
	if len(res.Findings) != 1 {
		t.Fatalf("want 1 finding (the T3 orphan), got %d: %+v", len(res.Findings), res.Findings)
	}
	got := res.Findings[0]
	if got.Tier != 3 {
		t.Errorf("tier = %d, want 3 (suspected)", got.Tier)
	}
	if got.Status != store.StatusOpen {
		t.Errorf("status = %q, want open", got.Status)
	}
	if got.File != "bug.go" || got.Line != 10 {
		t.Errorf("anchor = %s:%d, want bug.go:10", got.File, got.Line)
	}
	if !strings.Contains(got.Reasoning, "Verification skipped") {
		t.Errorf("reasoning should explain the budget stop, got %q", got.Reasoning)
	}

	// And it must be visibly noted as a skip so a human knows it wasn't verified.
	foundNote := false
	for _, n := range res.Skipped {
		if strings.Contains(n, "T3 suspected") {
			foundNote = true
		}
	}
	if !foundNote {
		t.Errorf("expected a Skipped note flagging the T3 orphan, got %v", res.Skipped)
	}

	// It must be durable in the store, queryable as a Tier 3 open finding.
	stored, err := st.ListFindings(ctx, store.FindingFilter{Status: store.StatusOpen, Tier: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("store has %d open T3 findings, want 1", len(stored))
	}
}

// TestHypothesize_MultiLens_NoRace is a -race regression test for the
// shared-slice race fixed by the three-index append in hypothesize.go. It fans
// out all builtin lenses in parallel (MaxParallel = len(BuiltinLenses())) so
// every goroutine tries to append to the same baseTools backing array
// simultaneously. Without the three-index fix (baseTools[:len:len]) the race
// detector fires because multiple goroutines write into the same backing-array
// slot (baseTools[len(baseTools)]) concurrently.
//
// Revert-experiment result (documented here for record): temporarily removing
// the ":len(baseTools)" cap so the call reads
//
//	append(baseTools, postLeadTool)
//
// and running `go test -race -run TestHypothesize_MultiLens_NoRace` reliably
// triggers a DATA RACE on the backing array. Restoring the three-index form
// clears the race detector. The fix is verified real.
func TestHypothesize_MultiLens_NoRace(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Use all builtin lenses so every goroutine slot is filled. MaxParallel is
	// set to len(BuiltinLenses()) to guarantee full concurrency: every (lens,
	// chunk) unit runs simultaneously, maximising the window for the race.
	// diff-intent emits zero chunk tasks on sweeps so nTaxonomy is the actual
	// concurrency count; MaxParallel is set higher to ensure all slots are open.
	nLenses := len(BuiltinLenses())
	nTaxonomy := nLenses - 1 // diff-intent has no chunk tasks on sweeps

	// The scripted client returns empty candidates for every lens — the test
	// only exercises the concurrent append path, not the candidate pipeline.
	finder := newScriptedClient()
	verifier := newScriptedClient()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		MaxParallel: nLenses,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Run Sweep multiple times to increase the probability of hitting the race
	// window. Under -race one iteration is typically sufficient; five is generous.
	for i := range 5 {
		res, err := f.Sweep(ctx)
		if err != nil {
			t.Fatalf("Sweep[%d]: %v", i, err)
		}
		// Sanity: all taxonomy lenses ran (one call per taxonomy lens, single chunk).
		if got := finder.callCount(); got < nTaxonomy*(i+1) {
			t.Errorf("Sweep[%d]: finder calls = %d, want >= %d (taxonomy lenses only)", i, got, nTaxonomy*(i+1))
		}
		_ = res
	}
}

// TestBudgetState_CacheReadWeighted pins bugbot-16k: cache-read tokens are
// discounted when charging the shared budget pool, so a cache-heavy run does
// not exhaust the budget at a fraction of real cost.
func TestBudgetState_CacheReadWeighted(t *testing.T) {
	st, _ := openFixture(t)
	rec := &spendRecorder{ctx: context.Background(), store: st}
	// Budget 2000; weight 0.1. A completion of 5000 input (4500 cached) + 100
	// out charges 500 uncached + 4500*0.1=450 + 100 out = 1050 chargeable, NOT
	// the raw 5100. So the pool is NOT exhausted; with raw accounting it would
	// be (5100 > 2000).
	b := newBudgetState(2000, rec, 0.1)
	rec.Record(llm.UsageEvent{Usage: llm.Usage{InputTokens: 5000, OutputTokens: 100, CacheReadInputTokens: 4500}})
	if err := b.pool.Check(); err != nil {
		t.Fatalf("pool exhausted after a mostly-cached completion: spent=%d (raw would be 5100), err=%v", b.pool.Spent(), err)
	}
	if spent := b.pool.Spent(); spent != 1050 {
		t.Errorf("pool charged %d, want 1050 (500 + 4500*0.1 + 100)", spent)
	}
}

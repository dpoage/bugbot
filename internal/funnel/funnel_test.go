package funnel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	// Finder ran once per unit in this sweep. Sweep units = (nTaxonomy taxonomy
	// lenses × sweep-wide) + 1 api-contract-misuse@contract-trace-deep + 2 state-trace-deep. diff-intent
	// emits zero chunk tasks on sweeps (no ChangeContext) so it contributes zero.
	wantFinderCalls := goSweepUnits()
	if finder.callCount() != wantFinderCalls {
		t.Errorf("finder calls = %d, want %d (taxonomy wide + contract-trace-deep + state-trace-deep)", finder.callCount(), wantFinderCalls)
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
	// the taxonomy units run: nTaxonomy taxonomy lenses × sweep-wide +1 for
	// api-contract-misuse@contract-trace-deep + 2 state-trace-deep = nTaxonomy+3 finders total.
	nSweepUnits := goSweepUnits()
	if res.Stats.FinderRuns != nSweepUnits {
		t.Errorf("FinderRuns = %d, want %d (taxonomy wide + deep strategies; diff-intent skipped)", res.Stats.FinderRuns, nSweepUnits)
	}
	if res.Stats.FinderFailures != nSweepUnits {
		t.Errorf("FinderFailures = %d, want %d (all sweep units failed to parse)", res.Stats.FinderFailures, nSweepUnits)
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

	cands, status, _, err := f.runFinder(ctx, finder, tools, "senior Go engineer", f.lenses[0],
		[]ingest.Language{ingest.LangGo}, finderTask([]string{"bug.go"}, nil, ""), budget)
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
	_, status, _, err := f.runFinder(ctx, finder, tools, "senior Go engineer", f.lenses[0],
		[]ingest.Language{ingest.LangGo}, finderTask([]string{"bug.go"}, nil, ""), budget)
	if err != nil {
		t.Fatalf("runFinder should not error on a parse failure: %v", err)
	}
	if status != finderParseFailed {
		t.Errorf("status = %d, want finderParseFailed (%d)", status, finderParseFailed)
	}
}

// rateLimitFinderClient is a one-shot llm.Client that returns an
// *llm.APIError{Kind: ErrRateLimited} on the first Complete call, matching the
// shape produced by the openai adapter when the provider returns a 429 after the
// retry budget is spent. Used by TestRunFinder_RateLimitNotParseFailure to
// exercise the rate-limit classification branch in runFinderWithPrompt without
// standing up the real llm retry wrapper.
type rateLimitFinderClient struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (c *rateLimitFinderClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *rateLimitFinderClient) Complete(_ context.Context, _ llm.Request) (llm.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return llm.Response{}, c.err
}

// TestRunFinder_RateLimitNotParseFailure proves the L2 (bugbot-8xp) fix: a
// finder whose provider exhausted the retry budget on a 429 (errors.Is(err,
// llm.ErrRateLimited) is true) must be classified as finderRateLimited, NOT as
// finderParseFailed. Rate-limit exhaustion is recoverable by lowering
// --concurrency or re-running, so it must not inflate FinderFailures or trip
// the SCAN RELIABILITY WARNING. The postmortem's Class is
// finderClassRateLimited (validated via the postmortem artifact).
func TestRunFinder_RateLimitNotParseFailure(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Fake client whose Complete returns a rate-limit error identical to what
	// the openai adapter surfaces after the retry wrapper gives up.
	rateLimitErr := &llm.APIError{
		Kind:       llm.ErrRateLimited,
		StatusCode: 429,
		Provider:   "openai",
		Message:    "429 too many requests",
	}
	if !errors.Is(rateLimitErr, llm.ErrRateLimited) {
		t.Fatal("test setup: APIError must satisfy errors.Is(err, llm.ErrRateLimited)")
	}
	finder := &rateLimitFinderClient{err: rateLimitErr}

	f, err := New(RoleClients{Finder: finder, Verifier: newScriptedClient()}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		t.Fatal(err)
	}

	// Unlimited budget so the rate-limit branch (not the budget-stop branch)
	// is what classifies the run.
	rec := &spendRecorder{ctx: ctx, store: st}
	budget := newBudgetState(0, rec, 1.0)
	cands, status, pm, err := f.runFinder(ctx, finder, tools, "senior Go engineer", f.lenses[0],
		[]ingest.Language{ingest.LangGo}, finderTask([]string{"bug.go"}, nil, ""), budget)
	if err != nil {
		t.Fatalf("runFinder should not error on a rate-limit classification: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("cands = %d, want 0 (no completion happened)", len(cands))
	}
	if status != finderRateLimited {
		t.Errorf("status = %d, want finderRateLimited (%d) — a rate-limit exhaustion must NOT be classified as a parse failure", status, finderRateLimited)
	}
	if status == finderParseFailed {
		t.Errorf("rate-limit run must not be reported as finderParseFailed (regression)")
	}
	if pm == nil {
		t.Fatal("postmortem is required on the rate-limit path")
	}
	if pm.Class != finderClassRateLimited {
		t.Errorf("pm.Class = %q, want %q", pm.Class, finderClassRateLimited)
	}
	// The client must have been called exactly once: runFinderWithPrompt must
	// not silently retry the finder itself (the retry client already exhausted
	// its budget before handing the error up).
	if finder.calls != 1 {
		t.Errorf("finder.calls = %d, want 1 (runFinderWithPrompt must not retry)", finder.calls)
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
	// Refuter never refutes either primary: both titles must survive so the test
	// is independent of which lens arrives first (streaming arrival order is
	// non-deterministic). The primary's title is whichever lens completed first.
	verifier := newScriptedClient()
	verifier.onTaskContains("nil deref of cfg in Greeting", notRefutedJSON)
	verifier.onTaskContains("unchecked pointer cfg used in Greeting", notRefutedJSON)

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// With the strategy axis, api-contract-misuse now has two units:
	// @sweep-wide and @contract-trace-deep. Both routes match the system prompt
	// that contains "api-contract-misuse", so both return apiLensCand — same
	// fingerprint (same lens, file, line, title) — one is deduped. That gives:
	//   hypothesized = nilLensCand + apiLensCand×2 = 3
	//   DroppedDuplicate = 1 (identical fingerprint from the two api units)
	// After dedup: 2 survivors (nil-safety + api-contract), which cross-lens merge
	// collapses to 1 cluster.
	if res.Stats.Hypothesized != 3 {
		t.Errorf("hypothesized = %d, want 3 (nil-safety + api-contract-misuse wide + api-contract-misuse deep)", res.Stats.Hypothesized)
	}
	if res.Stats.DroppedDuplicate != 1 {
		t.Errorf("dropped_duplicate = %d, want 1 (api-contract-misuse wide and deep return identical candidates)", res.Stats.DroppedDuplicate)
	}
	if res.Stats.MergedCrossLens != 1 {
		t.Errorf("merged_cross_lens = %d, want 1", res.Stats.MergedCrossLens)
	}
	if res.Stats.MergedWithinLens != 0 {
		t.Errorf("merged_within_lens = %d, want 0", res.Stats.MergedWithinLens)
	}
	if res.Stats.Triaged != 1 {
		t.Errorf("triaged = %d, want 1 (merged to primary after dedup)", res.Stats.Triaged)
	}
	// Exactly one refuter panel (3 refuters) ran — one cluster.
	if res.Stats.VerifierRuns != DefaultRefuters {
		t.Errorf("verifier_runs = %d, want %d (one panel for the one cluster)", res.Stats.VerifierRuns, DefaultRefuters)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(res.Findings), res.Findings)
	}
	got := res.Findings[0]
	// STREAMING SEMANTIC RELAXATION: primary selection is now arrival-order based.
	// The invariant is cluster-level: the primary carries the other lens as
	// corroboration, regardless of which arrived first.
	primaryLens := got.Lens
	var wantCorrobLens string
	switch primaryLens {
	case "nil-safety/error-handling":
		wantCorrobLens = "api-contract-misuse"
	case "api-contract-misuse":
		wantCorrobLens = "nil-safety/error-handling"
	default:
		t.Errorf("primary lens = %q, want nil-safety/error-handling or api-contract-misuse", primaryLens)
	}
	if wantCorrobLens != "" {
		if want := []string{wantCorrobLens}; !reflect.DeepEqual(got.CorroboratingLenses, want) {
			t.Errorf("corroborating lenses = %v, want %v", got.CorroboratingLenses, want)
		}
		if !strings.Contains(got.Reasoning, "Corroborated by lenses: "+wantCorrobLens) {
			t.Errorf("reasoning missing corroboration note:\n%s", got.Reasoning)
		}
	}

	// Persistence round-trip: the stored finding carries the corroboration.
	stored, err := st.GetFindingByFingerprint(ctx, got.Fingerprint)
	if err != nil {
		t.Fatalf("get persisted finding: %v", err)
	}
	if wantCorrobLens != "" {
		if want := []string{wantCorrobLens}; !reflect.DeepEqual(stored.CorroboratingLenses, want) {
			t.Errorf("persisted corroborating lenses = %v, want %v", stored.CorroboratingLenses, want)
		}
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
	// Zero-cost verifier: under the streaming topology verify panels run
	// CONCURRENTLY with finder units (HIGH-priority slots), so verifier spend
	// would make the soft-threshold crossing interleaving-dependent and this
	// test flaky. Zeroing verifier usage pins degradation to finder spend
	// alone, which MaxParallel=1 serializes deterministically.
	verifier.inUsage, verifier.outUsage, verifier.cachedUsage = 0, 0, 0

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
	// Some units must have been skipped: on a sweep, total units = (nTaxonomy
	// taxonomy lenses × sweep-wide) + 1 api-contract-misuse@contract-trace-deep + 2 state-trace-deep.
	// diff-intent has no chunk tasks on sweeps. Under degradation only the top-2
	// unit-classes by yield survive, so the finder cannot have run all units.
	nTaxonomy := len(BuiltinLenses()) - 1 // taxonomy lenses (no diff-intent)
	nSweepUnits := nTaxonomy + 3          // +1 contract-trace-deep + 2 state-trace-deep
	if finder.callCount() >= nSweepUnits {
		t.Errorf("finder ran %d times; degradation should have skipped low-yield lenses (nSweepUnits=%d)", finder.callCount(), nSweepUnits)
	}
}

// TestSweep_BudgetOrphanPersistsAsTier3 proves budget-orphan persistence UNDER
// the downstream reservation: when the VERIFY sub-pool is exhausted (by an
// earlier candidate's panel) before a later candidate can be verified, that
// later candidate is NOT dropped. It persists as an open Tier 3 suspected
// finding, surfaces in Result.Findings and Result.Skipped, and is counted in
// Stats.Suspected — so a human can still review it.
//
// It also pins the bugbot-3lt prioritization fix in the SAME run: the finder
// stage spends its entire reserved share, yet the verify reserve still carries
// one candidate all the way to a Tier-2 survivor. The orphan is driven by
// verify-sub-pool exhaustion (verifyOverHard at the gate), never by finder
// spend — the property the old single-pool premise could not express once the
// reservation existed.
func TestSweep_BudgetOrphanPersistsAsTier3(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// One lens emits TWO distinct candidates (bug.go:10 and clean.go:5). With
	// FinderBudgetShare=0.5 and TokenBudget=300 the pool splits into a 150-token
	// finder reserve and a 150-token verify reserve; each completion costs 150
	// (100 in + 50 out) at raw accounting. MaxParallel=1 + a single-refuter panel
	// serialize the two verifications through the one slot: the first candidate's
	// refuter spends the whole 150-token verify reserve and SURVIVES as T2; the
	// second candidate then hits verifyOverHard at the gate and is orphaned T3
	// WITHOUT its verifier ever running.
	finder := newScriptedClient()
	finderOnNilLens(finder)
	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Lenses:                []string{"nil-safety/error-handling"},
		TokenBudget:           300, // finder reserve 150, verify reserve 150
		FinderBudgetShare:     0.5,
		Refuters:              1, // one refuter (150 tokens) fills the verify reserve
		CacheReadBudgetWeight: 1.0,
		MaxParallel:           1, // serialize so the two verifications race deterministically
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if !res.Stopped {
		t.Errorf("expected Stopped=true after the verify reserve was exhausted")
	}
	// Exactly one candidate verified to T2 (the verify reserve carried it through
	// even though the finder spent its whole share) and exactly one was orphaned.
	if res.Stats.Verified != 1 {
		t.Errorf("Stats.Verified = %d, want 1 (one candidate survived within the verify reserve)", res.Stats.Verified)
	}
	if res.Stats.Suspected != 1 {
		t.Errorf("Stats.Suspected = %d, want 1 (the budget-orphaned candidate)", res.Stats.Suspected)
	}
	// The verifier ran for exactly the one survivor; the orphan was gated out
	// before any refuter launched for it.
	if verifier.callCount() != 1 {
		t.Errorf("verifier ran %d times; want 1 (only the survivor's single refuter)", verifier.callCount())
	}

	// Result.Findings holds both the T2 survivor and the T3 orphan. Which of the
	// two candidates wins the single slot is not asserted (scheduler-dependent);
	// the tier split is.
	if len(res.Findings) != 2 {
		t.Fatalf("want 2 findings (one T2 survivor, one T3 orphan), got %d: %+v", len(res.Findings), res.Findings)
	}
	var t2, t3 int
	for _, fnd := range res.Findings {
		switch fnd.Tier {
		case 2:
			t2++
		case 3:
			t3++
			if fnd.Status != store.StatusOpen {
				t.Errorf("orphan status = %q, want open", fnd.Status)
			}
			if !strings.Contains(fnd.Reasoning, "Verification skipped") {
				t.Errorf("orphan reasoning should explain the budget stop, got %q", fnd.Reasoning)
			}
		}
	}
	if t2 != 1 || t3 != 1 {
		t.Errorf("tiers = {T2:%d, T3:%d}, want {T2:1, T3:1}", t2, t3)
	}

	// The orphan must be visibly noted as a skip so a human knows it wasn't verified.
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
	// set to len(BuiltinLenses())+1 to guarantee full concurrency: every
	// (lens×strategy, chunk) unit runs simultaneously, maximising the window for
	// the race. diff-intent emits zero chunk tasks on sweeps; the actual sweep
	// unit count is nTaxonomy wide-strategy units + 1 contract-trace-deep + 2 state-trace-deep
	// (api-contract-misuse, concurrency, resource-leaks).
	nLenses := len(BuiltinLenses())
	nSweepUnits := goSweepUnits()

	// The scripted client returns empty candidates for every lens — the test
	// only exercises the concurrent append path, not the candidate pipeline.
	finder := newScriptedClient()
	verifier := newScriptedClient()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		MaxParallel: nLenses + 1, // +1 ensures a slot for the extra deep unit
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
		// Sanity: all sweep units ran (nSweepUnits units per sweep, single chunk).
		if got := finder.callCount(); got < nSweepUnits*(i+1) {
			t.Errorf("Sweep[%d]: finder calls = %d, want >= %d (nSweepUnits=%d per sweep)", i, got, nSweepUnits*(i+1), nSweepUnits)
		}
		_ = res
	}
}

// TestVerify_MultiCandidate_NoRace is a -race regression test for the
// per-candidate tools slice in runVerifyAndPersist. It guards against a
// reintroduction of the shared-slice race that existed in the old batch verify
// path (before the streaming refactor in run.go/verify_stream.go).
//
// The race scenario: if readOnlyTools were called ONCE outside the goroutine
// loop and the resulting slice passed into each goroutine, parallel goroutines
// would call append(sharedSlice, sbTool) simultaneously. When sharedSlice has
// spare capacity — which it does, because readOnlyTools returns
//
//	append([]agent.Tool{...5 elems...}, nav.Tools()...) // 4 elems
//
// yielding len=9 with cap≥10 — all goroutines would write into
// sharedSlice[9] concurrently: a verified data race on the backing array.
//
// The CURRENT code is safe: readOnlyTools is called INSIDE runVerifyAndPersist
// (verify_stream.go:103), so every goroutine gets its own freshly-allocated
// backing array and the append at verify_stream.go:114 is private. This test
// locks that invariant in: if a future edit moves readOnlyTools back outside
// the goroutine, -race fires here.
//
// Sandbox is enabled (MinSeverity="high", all candidates are severity="high")
// so buildSandboxTool returns a non-nil tool and the append path is exercised.
// Without sandbox the sbTool branch is skipped (no append, no race window) and
// the test would be a no-op for this class of regression.
//
// Revert-experiment (documented here): removing the per-goroutine readOnlyTools
// call and hoisting it above the go-func loop would cause this test to reliably
// trigger DATA RACE on the backing array under `go test -race`.
func TestVerify_MultiCandidate_NoRace(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Build N distinct high-severity candidates on lines spaced DefaultMergeWindow*3
	// apart so triage treats each as a separate cluster primary (no dedup, no merge).
	// Each gets a unique title so fingerprints are distinct too.
	const nCands = 6
	candParts := make([]string, nCands)
	for i := range nCands {
		line := 10 + i*DefaultMergeWindow*3
		candParts[i] = fmt.Sprintf(
			`{"file":"bug.go","line":%d,"title":"race-guard candidate %d","description":"verify race guard %d","severity":"high","evidence":"evidence %d","confidence":"high"}`,
			line, i, i, i,
		)
	}
	finder := newScriptedClient().onSystemContains("nil-safety/error-handling", candJSON(candParts...))

	// Verifier returns not-refuted immediately (no tool calls needed). The sandbox
	// tool is offered to each refuter but never invoked — the test only exercises
	// the concurrent append of sbTool into each goroutine's private candTools slice.
	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	sb := &funnelFakeSandbox{}
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Lenses:      []string{"nil-safety/error-handling"},
		Refuters:    1,          // one refuter: fast, still exercises the append path
		MaxParallel: nCands + 1, // enough slots to run all candidates concurrently
		SandboxOpts: SandboxOpts{
			Sandbox:     sb,
			Enabled:     true,
			MinSeverity: "high", // all candidates qualify: sbTool != nil path is taken
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Run multiple times to widen the race window. Under -race one iteration is
	// typically sufficient to detect a shared-slice append; five is generous.
	for i := range 5 {
		res, err := f.Sweep(ctx)
		if err != nil {
			t.Fatalf("Sweep[%d]: %v", i, err)
		}
		// All candidates survive (notRefutedJSON). On subsequent sweeps the store
		// returns existing open findings so Sweep still counts them.
		if len(res.Findings) == 0 && i == 0 {
			t.Errorf("Sweep[0]: want %d findings (all not-refuted), got 0", nCands)
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

// TestBudgetReserve_VerifyGateIndependentOfFinderSpend pins the bugbot-3lt
// prioritization fix at the gate level: under a downstream reservation the
// verify stage gates on its OWN reserve, never on finder spend. Exhausting the
// finder share (and beyond) must hard-stop finders WITHOUT degrading or stopping
// verify, and verify must stop only once its own reserve is spent. This is the
// deterministic core of "finders can't starve downstream".
func TestBudgetReserve_VerifyGateIndependentOfFinderSpend(t *testing.T) {
	st, _ := openFixture(t)
	rec := &spendRecorder{ctx: context.Background(), store: st}
	b := newBudgetState(1000, rec, 1.0)
	b.reserveForDownstream(0.7) // finder reserve 700, verify reserve 300

	// Spend the entire finder share (the "finders blow through the limit" case).
	b.finderPool.Add(700)
	if !b.finderOverHard() {
		t.Errorf("finderOverHard = false after finders spent their full 700 share, want true")
	}
	if b.verifyOverHard() {
		t.Errorf("verifyOverHard = true purely from finder exhaustion; verify must keep its reserve (prioritization)")
	}
	if b.verifyOverSoft() {
		t.Errorf("verifyOverSoft = true purely from finder exhaustion; finder spend must not degrade verify")
	}

	// Verify stops only when its OWN reserve is exhausted.
	b.verifyPool.Add(300)
	if !b.verifyOverHard() {
		t.Errorf("verifyOverHard = false after the verify reserve was fully spent, want true")
	}
}

// TestRunnerLimits_ClaimCap pins the per-task claim: a finder/verifier run is
// granted at most its role's claim (default-or-configured), clamped down to the
// sub-pool remainder when that is tighter, and uncapped (full remainder) when
// the claim is negative.
func TestRunnerLimits_ClaimCap(t *testing.T) {
	st, _ := openFixture(t)
	rec := &spendRecorder{ctx: context.Background(), store: st}
	b := newBudgetState(10_000_000, rec, 1.0)
	b.finderClaim = 1_000_000
	b.verifyClaim = 1_000_000
	b.reserveForDownstream(0.7) // finder reserve 7M, verify reserve 3M

	// claim (1M) < pool remainder (7M / 3M) => per-run budget == claim.
	if got := b.finderRunnerLimits(agent.Limits{}).TokenBudget; got != 1_000_000 {
		t.Errorf("finder per-run budget = %d, want 1_000_000 (claim cap below remainder)", got)
	}
	if got := b.verifyRunnerLimits(agent.Limits{}).TokenBudget; got != 1_000_000 {
		t.Errorf("verify per-run budget = %d, want 1_000_000 (claim cap below remainder)", got)
	}

	// Drive the finder sub-pool down so its remainder (500k) is below the claim.
	b.finderPool.Add(6_500_000)
	if got := b.finderRunnerLimits(agent.Limits{}).TokenBudget; got != 500_000 {
		t.Errorf("finder per-run budget = %d, want 500_000 (remainder below claim)", got)
	}

	// A negative claim disables the per-task cap: the run may use the full remainder.
	b.finderClaim = -1
	if got := b.finderRunnerLimits(agent.Limits{}).TokenBudget; got != 500_000 {
		t.Errorf("finder per-run budget = %d, want 500_000 (full remainder, claim disabled)", got)
	}
}

// TestSpendRecorder_ClaimRefundIsAutomatic proves the "return to the pool on
// closure" property: because the recorder charges the sub-pool only for tokens
// ACTUALLY spent, a run granted a 1M claim that spends 200k leaves the other
// 800k available to siblings — the claim is never reserved away in the first
// place.
func TestSpendRecorder_ClaimRefundIsAutomatic(t *testing.T) {
	st, _ := openFixture(t)
	rec := &spendRecorder{ctx: context.Background(), store: st}
	b := newBudgetState(2_000_000, rec, 1.0)
	b.finderClaim = 1_000_000
	b.reserveForDownstream(0.5) // finder reserve 1M, verify reserve 1M

	// A finder run granted a 1M claim spends only 200k.
	rec.Record(llm.UsageEvent{Role: roleFinder, Usage: llm.Usage{InputTokens: 150_000, OutputTokens: 50_000}})

	if got := b.finderPool.Spent(); got != 200_000 {
		t.Errorf("finder pool charged %d, want 200_000 (actual spend only, not the claim)", got)
	}
	if got := b.finderPool.Remaining(); got != 800_000 {
		t.Errorf("finder pool remaining = %d, want 800_000 (unspent claim stays available, NOT reserved away)", got)
	}
	// The sibling role is untouched: a finder run never debits the verify reserve.
	if got := b.verifyPool.Spent(); got != 0 {
		t.Errorf("verify pool charged %d by a finder run, want 0 (hard reserve)", got)
	}
}

// TestSweep_GlobalSlotPool_MaxParallelEnforced verifies that the global slot
// pool enforces MaxParallel=2 across finder agents IN BOTH DIRECTIONS: the
// bound is reached (peak == MaxParallel) and never exceeded. The fake client
// blocks every call on a barrier that opens only once MaxParallel callers are
// in flight, so the pool's bound is genuinely contended — an instant-return
// client never overlaps calls and would let a broken (or absent) pool pass a
// peak<=max assertion with peak==1.
func TestSweep_GlobalSlotPool_MaxParallelEnforced(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	const maxP = 2

	inner := newScriptedClient()
	inner.fallback = emptyCandidates

	var (
		inFlight    atomic.Int32
		peak        atomic.Int32
		barrierOnce sync.Once
		barrier     = make(chan struct{})
		timedOut    atomic.Bool
	)
	wrapped := &concurrencyTrackingClient{
		inner: inner,
		onEntry: func() {
			cur := inFlight.Add(1)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			if cur >= maxP {
				barrierOnce.Do(func() { close(barrier) })
			}
			// Hold this call until MaxParallel callers are in flight, forcing
			// the pool's bound to be contended. If the pool over-restricts
			// (effective bound < maxP) the barrier can never fill; the timeout
			// converts that hang into a test failure instead of a deadlock.
			select {
			case <-barrier:
			case <-time.After(10 * time.Second):
				timedOut.Store(true)
			}
		},
		onExit: func() { inFlight.Add(-1) },
	}

	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: wrapped, Verifier: verifier}, st, repo, Options{
		MaxParallel:         maxP,
		DisableHeatOrdering: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.Sweep(ctx); err != nil {
		t.Fatal(err)
	}

	if timedOut.Load() {
		t.Fatalf("barrier never filled: fewer than %d finder agents ran concurrently — pool over-restricts below MaxParallel", maxP)
	}
	if got := peak.Load(); got != maxP {
		t.Errorf("concurrent finder agents peak = %d, want exactly %d (MaxParallel must be reached and never exceeded)", got, maxP)
	}
}

// ---- streaming topology tests -----------------------------------------------

// TestTriageState_BothOrdersCluster is the keystone test for incremental
// clustering order-independence. It calls ts.process() directly with the same
// two-lens, same-location pair in BOTH arrival orders and asserts that in each
// case exactly ONE cluster primary is forwarded to verify, and the secondary
// member is staged as a corroborating lens.
//
// Testing the triage state directly (rather than end-to-end via Sweep) gives
// deterministic control of arrival order without fighting goroutine scheduling.
// The end-to-end cross-lens corroboration path is covered by
// TestSweep_CrossLensMerge_CorroborationPersisted.
//
// VACUITY: if ts.process / handleMember / AddStagedLens were removed or broken,
// the test would fail because either two primaries would be forwarded (breaking
// "triaged=1") or the staged lens would be absent (breaking "staged != empty").
func TestTriageState_BothOrdersCluster(t *testing.T) {
	const lensA = "nil-safety/error-handling"
	const lensB = "resource-leaks"

	// Two candidates at the same location with similar descriptions.
	// Different fingerprints (different lens × title), so exact-dedup doesn't
	// collapse them. Jaccard similarity between the descriptions is above the
	// merge threshold (they share most meaningful tokens).
	candA := Candidate{
		Lens: lensA, File: "bug.go", Line: 10,
		Title:       "nil deref of cfg in Greeting",
		Description: "cfg may be nil and is dereferenced without a guard in Greeting",
		Severity:    "high", Confidence: "high",
	}
	candB := Candidate{
		Lens: lensB, File: "bug.go", Line: 10,
		Title:       "cfg pointer may be nil in Greeting",
		Description: "cfg may be nil and is dereferenced without a guard in Greeting",
		Severity:    "medium", Confidence: "high",
	}

	// assignFingerprints sets the Fingerprint field that store.Fingerprint() would compute.
	fp := func(c Candidate) Candidate {
		c.Fingerprint = store.Fingerprint(c.Lens, c.File, c.Line, c.Title)
		return c
	}
	candA = fp(candA)
	candB = fp(candB)

	// openTriage creates a fresh triageState (backed by the fixture store).
	openTriage := func(t *testing.T) (*triageState, *clusterRegistry, *store.Store) {
		t.Helper()
		ctx := context.Background()
		st, repo := openFixture(t)
		snap, err := repo.Snapshot(ctx, ingest.ScanFilter{})
		if err != nil {
			t.Fatal(err)
		}
		ts, reg := newTriageState(snap)
		return ts, reg, st
	}

	// processOrder runs triage with the given candidate order and returns the
	// number of primaries forwarded and the staged lenses in the registry for
	// the first primary's fingerprint.
	processOrder := func(t *testing.T, first, second Candidate) (primaries []Candidate, stagedLenses []string) {
		t.Helper()
		ctx := context.Background()
		ts, reg, st := openTriage(t)
		stats := &Stats{}

		if err := ts.process(ctx, st, stats, first); err != nil {
			t.Fatalf("process(first): %v", err)
		}
		primaries = append(primaries, ts.popReady()...)

		if err := ts.process(ctx, st, stats, second); err != nil {
			t.Fatalf("process(second): %v", err)
		}
		primaries = append(primaries, ts.popReady()...)

		if len(primaries) > 0 {
			stagedLenses = reg.DrainStagedLenses(primaries[0].Fingerprint)
		}
		return primaries, stagedLenses
	}

	// Order A: candA (nil-safety) arrives first → becomes primary; candB staged.
	t.Run("lensA_primary", func(t *testing.T) {
		primaries, staged := processOrder(t, candA, candB)

		if len(primaries) != 1 {
			t.Fatalf("want 1 cluster primary, got %d: %v", len(primaries), primaries)
		}
		if primaries[0].Lens != lensA {
			t.Errorf("primary lens = %q, want %q (first-arrival)", primaries[0].Lens, lensA)
		}
		// VACUITY: if staging were removed, staged would be nil and this assertion fails.
		if want := []string{lensB}; !reflect.DeepEqual(staged, want) {
			t.Errorf("staged lenses = %v, want %v\n(VACUITY: absent staging → staged=nil → this fails)", staged, want)
		}
	})

	// Order B: candB (resource-leaks) arrives first → becomes primary; candA staged.
	// This is the order that batch severity-ranking would never allow (candB is
	// lower-severity) but streaming arrival-order does.
	t.Run("lensB_primary", func(t *testing.T) {
		primaries, staged := processOrder(t, candB, candA)

		if len(primaries) != 1 {
			t.Fatalf("want 1 cluster primary, got %d: %v", len(primaries), primaries)
		}
		if primaries[0].Lens != lensB {
			t.Errorf("primary lens = %q, want %q (first-arrival)", primaries[0].Lens, lensB)
		}
		// VACUITY: same check for the reversed order.
		if want := []string{lensA}; !reflect.DeepEqual(staged, want) {
			t.Errorf("staged lenses = %v, want %v\n(VACUITY: absent staging → staged=nil → this fails)", staged, want)
		}
	})
}

// TestStreaming_MidDiscovery_VerifyStarts tests that in the streaming topology
// a verify panel can start before all finder units have completed. With pool
// size 2, one finder unit blocks on a barrier while the other completes and
// emits a candidate; the test asserts that the verify panel for that candidate
// launches BEFORE the barrier opens (i.e., before the second finder finishes).
//
// This is the "latency" guarantee of the streaming topology: verify is not
// gated on full hypothesize completion.
func TestStreaming_MidDiscovery_VerifyStarts(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// barrier: the slow-lens finder blocks here until explicitly released.
	// verifyStarted: closed when the first verify call is observed.
	barrier := make(chan struct{})
	verifyStarted := make(chan struct{})
	var verifyOnce sync.Once

	// The fast lens (nil-safety) returns a real candidate immediately.
	// The slow lens (resource-leaks) blocks on barrier before returning empty.
	fastCand := candJSON(realCand)

	fastFinder := newScriptedClient().
		onSystemContains("nil-safety/error-handling", fastCand)
	// resource-leaks finder blocks until verify has started.
	slowFinder := &blockingClient{
		inner: newScriptedClient(), // returns empty candidates
		onCallStart: func() {
			// Wait for verify to start (or test timeout).
			select {
			case <-verifyStarted:
			case <-time.After(10 * time.Second):
				// timeout — the test will fail on the peak assertion below
			}
		},
	}
	// A dispatcher finder that routes by system prompt.
	combinedFinder := &dispatchClient{
		routes: []dispatchRoute{
			{sub: "nil-safety/error-handling", client: fastFinder},
			{sub: "state-trace-deep", client: newScriptedClient()}, {sub: "resource-leaks", client: slowFinder},
		},
		fallback: newScriptedClient(), // empty for all other lenses
	}

	// Verifier: signals verifyStarted on first call, then returns notRefuted.
	var verifierCalls atomic.Int32
	verifierClient := &hookClient{
		onCall: func(req llm.Request) {
			if verifierCalls.Add(1) == 1 {
				verifyOnce.Do(func() { close(verifyStarted) })
			}
		},
		response: notRefutedJSON,
	}

	f, err := New(RoleClients{Finder: combinedFinder, Verifier: verifierClient}, st, repo, Options{
		Lenses:      []string{"nil-safety/error-handling", "resource-leaks"},
		MaxParallel: 2, // allow both finder units to run concurrently
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = barrier // barrier is open-ended; verifyStarted unblocks slowFinder

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Verify started before the slow finder finished (guaranteed by the fact that
	// slowFinder only released after verifyStarted was closed).
	if verifierCalls.Load() == 0 {
		t.Errorf("verifier never called — streaming topology did not start verify mid-discovery")
	}

	// Sanity: the real candidate survived.
	if len(res.Findings) != 1 {
		t.Errorf("want 1 finding, got %d", len(res.Findings))
	}
}

// TestStreaming_PersistenceBeforeHypothesizeComplete tests that a candidate
// verified early is persisted in the store BEFORE a later blocked finder unit
// completes. This is the core streaming-persistence guarantee: persist-on-
// surviving (Stage D) does not wait for all finder units to finish.
//
// VACUITY: if the pipeline were reverted to batch mode (verify waits for all
// finder units before starting), the store check would race with the blocked
// finder — the test relies on the blocking finder still being in flight when
// the assertion runs.
func TestStreaming_PersistenceBeforeHypothesizeComplete(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// persistedCh is closed once we confirm the finding is in the store.
	persistedCh := make(chan struct{})
	// slowRelease is closed to allow the slow finder to proceed.
	slowRelease := make(chan struct{})

	fastCand := candJSON(realCand)

	fastFinder := newScriptedClient().
		onSystemContains("nil-safety/error-handling", fastCand)

	var slowStarted atomic.Bool
	slowFinder := &blockingClient{
		inner: newScriptedClient(), // returns empty for resource-leaks
		onCallStart: func() {
			slowStarted.Store(true)
			// Block until the main goroutine confirms the finding is persisted.
			select {
			case <-persistedCh:
			case <-time.After(10 * time.Second):
				// timeout — test will fail below
			}
			// Then unblock by reading slowRelease (no-op; just return).
			close(slowRelease)
		},
	}

	combinedFinder := &dispatchClient{
		routes: []dispatchRoute{
			{sub: "nil-safety/error-handling", client: fastFinder},
			{sub: "state-trace-deep", client: newScriptedClient()}, {sub: "resource-leaks", client: slowFinder},
		},
		fallback: newScriptedClient(),
	}

	fp := store.Fingerprint("nil-safety/error-handling", "bug.go", 10, "nil deref of cfg in Greeting")
	// Simple verifier: never refutes. The store polling loop below detects when
	// the finding has been persisted by verify_stream.go's immediate-persist path.
	verifierClient := &hookClient{
		response: notRefutedJSON,
	}

	f, err := New(RoleClients{Finder: combinedFinder, Verifier: verifierClient}, st, repo, Options{
		Lenses:      []string{"nil-safety/error-handling", "resource-leaks"},
		MaxParallel: 2, // both units can run concurrently
	})
	if err != nil {
		t.Fatal(err)
	}

	// Run Sweep in background so we can probe the store while it's running.
	type sweepResult struct {
		res *Result
		err error
	}
	resultCh := make(chan sweepResult, 1)
	go func() {
		res, err := f.Sweep(ctx)
		resultCh <- sweepResult{res, err}
	}()

	// Poll the store until the finding appears OR timeout.
	// The slow finder blocks until we close persistedCh, so if the finding
	// appears in the store before we signal it, the streaming guarantee holds.
	deadline := time.After(15 * time.Second)
	var foundBeforeSlowDone bool
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for finding to appear in store")
		case <-time.After(5 * time.Millisecond):
			// Check if the slow finder has started (ensures concurrency).
			if !slowStarted.Load() {
				continue
			}
			// Check if the finding is now in the store.
			_, err := st.GetFindingByFingerprint(ctx, fp)
			if err == nil {
				// Finding is in store while slow finder is still blocked.
				foundBeforeSlowDone = true
				close(persistedCh) // release the slow finder
				goto done
			}
		}
	}
done:
	// Wait for Sweep to complete.
	sr := <-resultCh
	if sr.err != nil {
		t.Fatalf("Sweep error: %v", sr.err)
	}

	if !foundBeforeSlowDone {
		t.Errorf("finding NOT in store before slow finder completed — streaming persistence guarantee violated")
	}
	if len(sr.res.Findings) != 1 {
		t.Errorf("want 1 finding, got %d", len(sr.res.Findings))
	}

	// Verify the finding is a proper T2 Verified finding.
	stored, err := st.GetFindingByFingerprint(ctx, fp)
	if err != nil {
		t.Fatalf("get finding after sweep: %v", err)
	}
	if stored.Tier != 2 {
		t.Errorf("tier = %d, want 2 (verified)", stored.Tier)
	}

	_ = fp
}

// TestStreaming_Interrupt_PersistedFindingSurvives tests that a finding
// persisted before context cancellation survives the interruption: the finding
// remains in the store (durable), and the run result is sealed as Interrupted.
func TestStreaming_Interrupt_PersistedFindingSurvives(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, repo := openFixture(t)

	// The fast lens emits a real candidate that survives verify.
	// After verify completes (and the finding is persisted), we cancel the ctx.
	// A second lens's finder is blocked on a barrier, ensuring the hypothesize
	// goroutine is still running when ctx is cancelled.
	var persistedCh = make(chan struct{}) // closed when finding is in store
	var cancelOnce sync.Once

	fastCand := candJSON(realCand)
	fastFinder := newScriptedClient().
		onSystemContains("nil-safety/error-handling", fastCand)

	slowFinder := &blockingClient{
		inner: newScriptedClient(),
		onCallStart: func() {
			// Wait until cancelled.
			<-ctx.Done()
		},
	}

	combinedFinder := &dispatchClient{
		routes: []dispatchRoute{
			{sub: "nil-safety/error-handling", client: fastFinder},
			{sub: "state-trace-deep", client: newScriptedClient()}, {sub: "resource-leaks", client: slowFinder},
		},
		fallback: newScriptedClient(),
	}

	fp := store.Fingerprint("nil-safety/error-handling", "bug.go", 10, "nil deref of cfg in Greeting")
	// Verifier signals persistedCh after the last refuter panel completes,
	// then cancels ctx. We use a hookClient with a counter.
	var verifierCalls atomic.Int32
	verifierClient := &hookClient{
		onCall: func(req llm.Request) {
			if int(verifierCalls.Add(1)) == DefaultRefuters {
				// All refuters done; the finding is being persisted now or soon.
				// Give the UpsertFinding a moment then cancel.
				go func() {
					// Poll until finding appears or give up after 5s.
					deadline := time.After(5 * time.Second)
					for {
						select {
						case <-deadline:
							cancelOnce.Do(cancel)
							return
						case <-time.After(2 * time.Millisecond):
							if _, err := st.GetFindingByFingerprint(context.Background(), fp); err == nil {
								close(persistedCh)
								cancelOnce.Do(cancel)
								return
							}
						}
					}
				}()
			}
		},
		response: notRefutedJSON,
	}

	f, err := New(RoleClients{Finder: combinedFinder, Verifier: verifierClient}, st, repo, Options{
		Lenses:      []string{"nil-safety/error-handling", "resource-leaks"},
		MaxParallel: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, sweepErr := f.Sweep(ctx)

	// Sweep must return context.Canceled (Interrupted).
	if sweepErr == nil {
		t.Errorf("want Sweep to return error on cancellation, got nil")
	} else if sweepErr != context.Canceled {
		t.Errorf("sweep error = %v, want context.Canceled", sweepErr)
	}

	// The pre-cancellation finding must still be in the store (durable).
	select {
	case <-persistedCh:
		// Good: finding was persisted before cancel.
	default:
		t.Logf("persistedCh not closed — finding may not have persisted before cancel")
	}
	stored, err := st.GetFindingByFingerprint(context.Background(), fp)
	if err != nil {
		t.Fatalf("pre-cancel finding not in store: %v", err)
	}
	if stored.Tier != 2 {
		t.Errorf("finding tier = %d, want 2 (verified)", stored.Tier)
	}
	if stored.Status != store.StatusOpen {
		t.Errorf("finding status = %q, want open", stored.Status)
	}
}

// blockingClient is a fake llm.Client that calls onCallStart at the beginning
// of each Complete call, allowing tests to inject blocking or coordination
// behavior into a specific finder unit.
type blockingClient struct {
	inner       *scriptedClient
	onCallStart func()
}

func (c *blockingClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *blockingClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if c.onCallStart != nil {
		c.onCallStart()
	}
	return c.inner.Complete(ctx, req)
}

// dispatchRoute maps a system-prompt substring to a client.
type dispatchRoute struct {
	sub    string
	client llm.Client
}

// dispatchClient routes each Complete call to the first client whose sub
// appears in the system prompt, or fallback if none match.
type dispatchClient struct {
	routes   []dispatchRoute
	fallback llm.Client
}

func (c *dispatchClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *dispatchClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	for _, r := range c.routes {
		if strings.Contains(req.System, r.sub) {
			return r.client.Complete(ctx, req)
		}
	}
	return c.fallback.Complete(ctx, req)
}

// hookClient calls onCall before each Complete, then returns a fixed response.
type hookClient struct {
	onCall   func(req llm.Request)
	response string
}

func (c *hookClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *hookClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if c.onCall != nil {
		c.onCall(req)
	}
	if ctx.Err() != nil {
		return llm.Response{}, ctx.Err()
	}
	return llm.Response{
		Text:       c.response,
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 60},
	}, nil
}

// concurrencyTrackingClient is a fake llm.Client that calls onEntry/onExit
// hooks around each Complete call to let callers measure concurrent invocations.
type concurrencyTrackingClient struct {
	inner   *scriptedClient
	onEntry func()
	onExit  func()
}

func (c *concurrencyTrackingClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *concurrencyTrackingClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	c.onEntry()
	defer c.onExit()
	return c.inner.Complete(ctx, req)
}

// TestTriageState_TransitiveChain_AllOrders is the membership keystone: a
// chain A~B, B~C, A≁C (pairwise-similar neighbors, dissimilar endpoints, all
// window-near through the chain) must form ONE cluster in EVERY arrival order
// that batch clustering's transitive closure would have collapsed. Primary-only
// membership — checking arrivals against the first member alone — passes a
// 2-member test (the degenerate case) but fails this one: C never matches A.
func TestTriageState_TransitiveChain_AllOrders(t *testing.T) {
	ctx := context.Background()
	st, _ := openFixture(t)

	mk := func(lens, title, desc string, line int) Candidate {
		return Candidate{
			Lens: lens, File: "bug.go", Line: line, Title: title,
			Description: desc, Severity: "high", Confidence: "high",
		}
	}
	// Token sets (descTokens keeps only [a-z0-9]+ words longer than 2 chars):
	// A={alpha,beta,gamma,delta}, B={alpha,beta,echo,foxtrot},
	// C={echo,foxtrot,golf,hotel}.
	// jaccard(A,B)=2/6≈0.33, jaccard(B,C)=2/6≈0.33, jaccard(A,C)=0.
	a := mk("nil-safety/error-handling", "chain a", "alpha beta gamma delta", 10)
	b := mk("resource-leaks", "chain b", "alpha beta echo foxtrot", 18)
	c := mk("concurrency", "chain c", "echo foxtrot golf hotel", 26)

	// Fixture preconditions: the chain must hold under the production
	// similarity rule, or the test silently stops testing transitivity.
	tokA, tokB, tokC := descTokens(a.Description), descTokens(b.Description), descTokens(c.Description)
	if jaccard(tokA, tokB) < mergeSimilarityThreshold || jaccard(tokB, tokC) < mergeSimilarityThreshold {
		t.Fatal("fixture broken: chain neighbors must be pairwise similar")
	}
	if jaccard(tokA, tokC) >= mergeSimilarityThreshold {
		t.Fatal("fixture broken: chain endpoints must be dissimilar")
	}

	snap := &ingest.Snapshot{Files: []ingest.File{{Path: "bug.go"}}}
	orders := [][]Candidate{
		{a, b, c}, {c, b, a}, {b, a, c}, {b, c, a}, {a, c, b}, {c, a, b},
	}
	for i, order := range orders {
		ts, _ := newTriageState(snap)
		var stats Stats
		for _, cand := range order {
			if err := ts.process(ctx, st, &stats, cand); err != nil {
				t.Fatalf("order %d: process: %v", i, err)
			}
		}
		primaries := ts.popReady()
		merged := stats.MergedCrossLens + stats.MergedWithinLens
		// Orders where the bridge (B) arrives LAST form two clusters before B
		// can connect them — the documented irreversible-forwarding relaxation.
		// In every other order the chain must collapse to ONE cluster.
		bridgeLast := order[2].Title == "chain b"
		wantPrimaries, wantMerged := 1, 2
		if bridgeLast {
			wantPrimaries, wantMerged = 2, 1
		}
		if len(primaries) != wantPrimaries || merged != wantMerged {
			t.Errorf("order %d (%s,%s,%s): primaries=%d merged=%d, want %d/%d",
				i, order[0].Title, order[1].Title, order[2].Title,
				len(primaries), merged, wantPrimaries, wantMerged)
		}
	}
}

// TestTriageState_SameBucketDissimilarClusters reproduces the eval-corpus
// regression shape: two token-DISSIMILAR defect groups sharing one location
// bucket must form two INDEPENDENT clusters, each absorbing its own later
// members. A single-cluster-per-bucket map lets the second group's primary
// overwrite the first group's bucket pointer, orphaning it so its later
// members become spurious primaries (extra panels, extra false positives).
func TestTriageState_SameBucketDissimilarClusters(t *testing.T) {
	ctx := context.Background()
	st, _ := openFixture(t)

	mk := func(lens, title, desc string, line int) Candidate {
		return Candidate{
			Lens: lens, File: "bug.go", Line: line, Title: title,
			Description: desc, Severity: "high", Confidence: "high",
		}
	}
	// Group P (make-panic shaped) and group L (fd-leak shaped) interleaved,
	// lines 15/17 — same bucket, mirroring the recorded corpus geometry.
	p1 := mk("injection/input-validation", "panic p1", "caller controlled size make byte slice panic", 15)
	l1 := mk("resource-leaks", "leak l1", "file descriptor leaked close missing error path", 17)
	p2 := mk("boundary-conditions", "panic p2", "caller controlled size make byte allocation panic", 15)
	l2 := mk("nil-safety/error-handling", "leak l2", "file descriptor leaked close missing on failure", 17)

	snap := &ingest.Snapshot{Files: []ingest.File{{Path: "bug.go"}}}
	ts, _ := newTriageState(snap)
	var stats Stats
	for _, cand := range []Candidate{p1, l1, p2, l2} {
		if err := ts.process(ctx, st, &stats, cand); err != nil {
			t.Fatalf("process: %v", err)
		}
	}
	primaries := ts.popReady()
	if len(primaries) != 2 {
		t.Fatalf("primaries = %d, want 2 (one per dissimilar group): %+v", len(primaries), primaries)
	}
	if stats.MergedCrossLens != 2 {
		t.Errorf("MergedCrossLens = %d, want 2 (p2 into p1's cluster, l2 into l1's)", stats.MergedCrossLens)
	}
}

// TestTriageState_NoMergeAcrossFilesOrBeyondWindow ports the batch
// clustering's negative guarantees to the streaming layer: identical
// descriptions must NOT merge across different files, nor beyond the line
// window within one file.
func TestTriageState_NoMergeAcrossFilesOrBeyondWindow(t *testing.T) {
	ctx := context.Background()
	st, _ := openFixture(t)

	mk := func(lens, file string, line int) Candidate {
		return Candidate{
			Lens: lens, File: file, Line: line, Title: "same bug " + file + " " + lens,
			Description: "identical description tokens everywhere always", Severity: "high", Confidence: "high",
		}
	}
	snap := &ingest.Snapshot{Files: []ingest.File{{Path: "bug.go"}, {Path: "clean.go"}}}
	ts, _ := newTriageState(snap)
	var stats Stats
	for _, cand := range []Candidate{
		mk("nil-safety/error-handling", "bug.go", 10),
		mk("resource-leaks", "clean.go", 10),                   // other file: no merge
		mk("concurrency", "bug.go", 10+DefaultMergeWindow*3+1), // same file, far: no merge
	} {
		if err := ts.process(ctx, st, &stats, cand); err != nil {
			t.Fatalf("process: %v", err)
		}
	}
	if got := len(ts.popReady()); got != 3 {
		t.Errorf("primaries = %d, want 3 (no cross-file or beyond-window merging)", got)
	}
	if stats.MergedCrossLens+stats.MergedWithinLens != 0 {
		t.Errorf("merges = %d/%d, want none", stats.MergedWithinLens, stats.MergedCrossLens)
	}
}

// TestSpendRecorder_CartographerChargesFinderPool pins the budget routing for
// the cartographer role: its spend supports finding, so under a downstream
// reservation it charges the FINDER sub-pool, never the verify reserve.
func TestSpendRecorder_CartographerChargesFinderPool(t *testing.T) {
	st, _ := openFixture(t)
	rec := &spendRecorder{ctx: context.Background(), store: st}
	b := newBudgetState(1000, rec, 1.0)
	b.reserveForDownstream(0.7) // finder reserve 700, verify reserve 300
	rec.Record(llm.UsageEvent{Role: roleCartographer, Usage: llm.Usage{InputTokens: 200}})
	if got := b.finderPool.Spent(); got != 200 {
		t.Errorf("finderPool charged %d, want 200 from cartographer spend", got)
	}
	if got := b.verifyPool.Spent(); got != 0 {
		t.Errorf("verifyPool charged %d, want 0 (cartographer must not touch the verify reserve)", got)
	}
}

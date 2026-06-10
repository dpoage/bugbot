package funnel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
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
	// Finder ran once per lens per chunk (one chunk here).
	if finder.callCount() != len(BuiltinLenses()) {
		t.Errorf("finder calls = %d, want %d", finder.callCount(), len(BuiltinLenses()))
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
		TokenBudget: 600, // ~4 completions before soft threshold (70% of 600 = 420)
		MaxParallel: 1,   // serialize so budget accrues deterministically
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
	// Some lenses must have been skipped: with 6 lenses and degradation to 2,
	// the finder cannot have run all 6.
	if finder.callCount() >= len(BuiltinLenses()) {
		t.Errorf("finder ran %d times; degradation should have skipped low-yield lenses", finder.callCount())
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
		Lenses:      []string{"nil-safety/error-handling"},
		TokenBudget: 100, // < 150, so the pool is exhausted after the finder call
		MaxParallel: 1,
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

package funnel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// cartographyFixture adds a sub-package "sub" with one file alongside the
// standard bug.go/clean.go fixture. The sub package is a real, non-root
// package — required so the cartographer's package-fingerprint cache
// (which uses the directory as the row key) has at least one non-empty
// key to seed. The fixture is committed so git tracks the file and
// ingest.Snapshot picks it up.
const cartographySubPkgFile = `package sub

// Helper returns a constant. Bug-free on purpose.
func Helper() int { return 42 }
`

// openCartographyFixture opens the standard fixture repo and layers a
// committed sub/ sub-package on top of it. The returned repo is the
// ingest.Repo the funnel uses; the temp dir is managed by t.TempDir so
// the cleanup is automatic.
func openCartographyFixture(t *testing.T) (*store.Store, *ingest.Repo) {
	t.Helper()
	ctx := context.Background()

	// Reuse the standard file-backed store; cartography tests need a
	// real DB for the summary table round-trip.
	tmpDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Build a fixture repo with the standard files plus one sub/ file.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repoDir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(repoDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("bug.go", realBugFile)
	write("clean.go", cleanFile)
	write("sub/sub.go", cartographySubPkgFile)
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
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

	repo, err := ingest.Open(ctx, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	return st, repo
}

// TestCartography_InjectIntoFinderTask pins the happy path: with
// Options.Cartographer=true, a finder request's TASK user message contains
// the injected summary string. We pre-seed a summary row in the store
// (the cartographer's lazy pass reuses cached rows whose fingerprint matches)
// and assert the resulting injection block contains the seeded text.
//
// We test through the newCartographer -> ensureContextFor -> finderTask path so
// the test exercises the real lazy injection logic without spinning up the full
// Sweep pipeline.
func TestCartography_InjectIntoFinderTask(t *testing.T) {
	ctx := context.Background()
	st, repo := openCartographyFixture(t)

	finder := newScriptedClient()
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Features: FeatureFlags{Cartographer: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	snap, err := repo.Snapshot(ctx, ingest.ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	fps, err := repo.Fingerprints(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	// Target the sub/ package so the row key is non-empty (the
	// root-package key "" is invalid for the store summary table; see
	// store.ErrInvalidPackageSummary).
	targets := []string{"sub/sub.go"}
	pkgs := packagesSpanned(targets)
	pkg := "sub"
	members := pkgs[pkg]
	if len(members) == 0 {
		t.Fatal("expected sub/ to be enumerated as a package")
	}
	fp := packageFingerprint(pkg, members, fps)
	want := "FAKE-SUMMARY-FOR-" + pkg
	if err := st.UpsertPackageSummaries(ctx, []store.PackageSummary{
		{Pkg: pkg, Fingerprint: fp, Summary: want},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cart := f.newCartographer(ctx, &Result{}, finder, snap, targets, fps, nil)
	if cart == nil {
		t.Fatal("newCartographer returned nil; expected non-nil with Cartographer=true")
	}

	injection := cart.ensureContextFor(ctx, targets)
	// Verify the memo was populated from the store hit.
	cart.mu.Lock()
	gotSummary := cart.summaries[pkg]
	cart.mu.Unlock()
	if gotSummary != want {
		t.Errorf("cartography.summaries[%q] = %q, want %q", pkg, gotSummary, want)
	}

	task := finderTask(targets, nil, injection)
	if !strings.Contains(task, "REPO CONTEXT — package summaries") {
		t.Errorf("finder task missing cartography block; got:\n%s", task)
	}
	if !strings.Contains(task, want) {
		t.Errorf("finder task missing seeded summary text %q; got:\n%s", want, task)
	}

	// The injection must be in the TASK message, NOT the system prompt.
	for _, r := range finder.allRequests() {
		if strings.Contains(r.System, "REPO CONTEXT") {
			t.Errorf("cartography block leaked into system prompt: %s", r.System)
		}
	}
}

// TestCartography_DisabledByDefault pins the regression-guard: with
// Options.Cartographer=false (the default), newCartographer returns nil and
// ensureContextFor on a nil receiver returns "".
func TestCartography_DisabledByDefault(t *testing.T) {
	ctx := context.Background()
	st, repo := openCartographyFixture(t)

	finder := newScriptedClient()
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}

	snap, err := repo.Snapshot(ctx, ingest.ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	fps, err := repo.Fingerprints(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	targets := []string{"sub/sub.go"}

	cart := f.newCartographer(ctx, &Result{}, finder, snap, targets, fps, nil)
	if cart != nil {
		t.Errorf("newCartographer with Cartographer=false returned %+v, want nil", cart)
	}

	var nilCart *cartography
	injection := nilCart.ensureContextFor(ctx, targets)
	task := finderTask(targets, nil, injection)
	if strings.Contains(task, "REPO CONTEXT") {
		t.Errorf("finder task contains REPO CONTEXT block despite Cartographer=false:\n%s", task)
	}
}

// TestCartography_FingerprintCache pins the cache discipline: a second
// ensureContextFor call with the same fingerprints does NOT re-call the LLM.
// The first call generates and persists the summary; the second call picks it
// up from the store (fingerprint match) without invoking the client.
func TestCartography_FingerprintCache(t *testing.T) {
	ctx := context.Background()
	st, repo := openCartographyFixture(t)

	finder := newScriptedClient()
	finder.fallback = `{"summary":"A canned summary."}`
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Features: FeatureFlags{Cartographer: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	snap, err := repo.Snapshot(ctx, ingest.ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	fps, err := repo.Fingerprints(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	targets := []string{"sub/sub.go"}

	// First provider: generates and persists.
	cart1 := f.newCartographer(ctx, &Result{}, finder, snap, targets, fps, nil)
	_ = cart1.ensureContextFor(ctx, targets)
	afterFirst := summaryCallCount(finder)
	if afterFirst == 0 {
		t.Fatalf("expected at least one summary completion on first pass, got 0")
	}

	// Second provider (fresh instance, same store): must hit the persisted row.
	// Suppress the unused-store warning — st is used for the fixture DB.
	_ = st
	cart2 := f.newCartographer(ctx, &Result{}, finder, snap, targets, fps, nil)
	_ = cart2.ensureContextFor(ctx, targets)
	afterSecond := summaryCallCount(finder)
	if afterSecond != afterFirst {
		t.Errorf("summary completion count rose from %d to %d on second pass; expected no regeneration", afterFirst, afterSecond)
	}
}

// TestCartography_DegradesOnLLMError pins the graceful-degrade contract:
// if the summary-generation completion errors, ensureContextFor still returns
// "" (not a panic) and the provider is non-nil.
func TestCartography_DegradesOnLLMError(t *testing.T) {
	ctx := context.Background()
	st, repo := openCartographyFixture(t)
	_ = st

	finder := newScriptedClient()
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Features: FeatureFlags{Cartographer: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	snap, err := repo.Snapshot(ctx, ingest.ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	fps, err := repo.Fingerprints(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	targets := []string{"sub/sub.go"}

	errC := &errClient{err: errFakeLLM}
	cart := f.newCartographer(ctx, &Result{}, errC, snap, targets, fps, nil)
	if cart == nil {
		t.Fatal("newCartographer returned nil; expected non-nil with Cartographer=true")
	}
	if got := cart.ensureContextFor(ctx, targets); got != "" {
		t.Errorf("ensureContextFor returned non-empty block despite LLM error: %q", got)
	}
	// Memo must be empty — no summary materialised.
	cart.mu.Lock()
	n := len(cart.summaries)
	cart.mu.Unlock()
	if n != 0 {
		t.Errorf("expected no summaries after LLM error, got %d", n)
	}
}

// TestCartography_ContextFor_NilAndEmpty pins the no-op edges of
// contextFor: a nil receiver and an empty receiver both return "".
func TestCartography_ContextFor_NilAndEmpty(t *testing.T) {
	var c *cartography
	if got := c.contextFor([]string{"a.go"}); got != "" {
		t.Errorf("nil receiver: got %q, want \"\"", got)
	}
	empty := &cartography{summaries: map[string]string{}, importers: map[string][]string{}}
	if got := empty.contextFor([]string{"a.go"}); got != "" {
		t.Errorf("empty cartography: got %q, want \"\"", got)
	}
}

// TestCartography_ContextFor_DependencyInjection pins the dependency
// injection: a package that imports our target shows up in the
// injection block marked "(dependency)". We build the importers map
// directly rather than through PackageImporters (the graph is small
// and the test stays self-contained).
func TestCartography_ContextFor_DependencyInjection(t *testing.T) {
	c := &cartography{
		summaries: map[string]string{
			"target": "TARGET-SUMMARY",
			"dep":    "DEP-SUMMARY",
		},
		importers: map[string][]string{
			"target": {"dep"},
		},
	}
	got := c.contextFor([]string{"target/file.go"})
	if !strings.Contains(got, "REPO CONTEXT — package summaries") {
		t.Errorf("missing header: %q", got)
	}
	if !strings.Contains(got, "target: TARGET-SUMMARY") {
		t.Errorf("missing own-package line: %q", got)
	}
	if !strings.Contains(got, "dep (dependency): DEP-SUMMARY") {
		t.Errorf("missing dependency line: %q", got)
	}
}

// summaryCallCount counts how many recorded requests targeted the
// cartographer (system prompt begins with the cartography
// instruction). The cartographer is the only LLM caller that uses that
// system prompt, so this isolates its completions from the finder's.
func summaryCallCount(c *scriptedClient) int {
	n := 0
	for _, r := range c.allRequests() {
		if strings.HasPrefix(r.System, "Summarize this package") {
			n++
		}
	}
	return n
}

var errFakeLLM = stringErr("cartography test: synthetic LLM error")

// stringErr lets the test inject a plain error without importing
// "errors" or "fmt" at the top of the file.
type stringErr string

func (e stringErr) Error() string { return string(e) }

// errClient is an llm.Client that always errors on Complete. It is the
// cheapest way to exercise the graceful-degrade branch of cartograph
// without standing up a fully scripted client that routes by system
// prompt.
type errClient struct{ err error }

func (c *errClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (c *errClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	return llm.Response{}, c.err
}

// TestCartography_StripsThinkBlock pins the fix for the reasoning-model
// pollution bug: MiniMax-M3 emits an inline <think>...</think> preamble in the
// content, and the cartographer must store/return only the summary after it —
// not the raw reasoning. Exercises the lazy path: ensureContextFor triggers
// summarizePackage which routes through RunJSON (think-block stripping).
func TestCartography_StripsThinkBlock(t *testing.T) {
	ctx := context.Background()
	st, repo := openCartographyFixture(t)

	finder := newScriptedClient()
	finder.fallback = "<think>\nThe user wants a summary. Let me reason about the package...\n</think>\n\n{\"summary\":\"Purpose: a helper package.\"}"
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{Features: FeatureFlags{Cartographer: true}})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := repo.Snapshot(ctx, ingest.ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	fps, err := repo.Fingerprints(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}

	cart := f.newCartographer(ctx, &Result{}, finder, snap, []string{"sub/sub.go"}, fps, nil)
	if cart == nil {
		t.Fatal("nil cartography")
	}
	_ = cart.ensureContextFor(ctx, []string{"sub/sub.go"})

	cart.mu.Lock()
	got := cart.summaries["sub"]
	cart.mu.Unlock()
	if got != "Purpose: a helper package." {
		t.Errorf("returned summary = %q, want the text after </think> with the think block stripped", got)
	}
	// The persisted row must be clean too.
	rows, err := st.ListPackageSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range rows {
		if r.Pkg == "sub" {
			found = true
			if strings.Contains(r.Summary, "<think>") || strings.Contains(r.Summary, "Let me reason") {
				t.Errorf("persisted summary still carries reasoning: %q", r.Summary)
			}
		}
	}
	if !found {
		t.Error("sub package summary was not persisted")
	}
}

// TestCartography_RepairsMalformedSummary pins bugbot-89r.1: routing the summary
// through agent.Runner.RunJSON means a first completion that is not valid JSON is
// REPAIRED on a second round-trip rather than silently dropped. Exercises the
// lazy path: ensureContextFor triggers summarizePackage which uses RunJSON.
func TestCartography_RepairsMalformedSummary(t *testing.T) {
	ctx := context.Background()
	st, repo := openCartographyFixture(t)
	_ = st

	valid := `{"summary":"Repaired package summary."}`
	finder := newScriptedSequenceClient(valid, "this is not json, only reasoning prose", valid)
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{Features: FeatureFlags{Cartographer: true}})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := repo.Snapshot(ctx, ingest.ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	fps, err := repo.Fingerprints(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}

	cart := f.newCartographer(ctx, &Result{}, finder, snap, []string{"sub/sub.go"}, fps, nil)
	if cart == nil {
		t.Fatal("nil cartography")
	}
	_ = cart.ensureContextFor(ctx, []string{"sub/sub.go"})

	cart.mu.Lock()
	got := cart.summaries["sub"]
	cart.mu.Unlock()
	if got != "Repaired package summary." {
		t.Errorf("summary = %q, want the repaired value; a malformed first completion must be repaired, not dropped", got)
	}
	if n := len(finder.allRequests()); n < 2 {
		t.Errorf("expected >=2 completions (initial + repair round-trip), got %d", n)
	}
}

// TestPackagesSpanned_SkipsRoot guards the batch-poisoning fix: repo-root files
// (path.Dir ".") must NOT produce an empty-keyed package. UpsertPackageSummaries
// rejects an empty Pkg, and as one transaction a single such row drops the whole
// batch — so a full-repo run (which always includes go.mod/README/etc.) would
// persist nothing.
func TestPackagesSpanned_SkipsRoot(t *testing.T) {
	got := packagesSpanned([]string{"go.mod", "README.md", "sub/sub.go", "a/b/c.go"})
	if _, ok := got[""]; ok {
		t.Errorf(`root package "" must be skipped (empty key is unstorable); got %v`, got)
	}
	if _, ok := got["sub"]; !ok {
		t.Errorf("expected package %q in %v", "sub", got)
	}
	if _, ok := got["a/b"]; !ok {
		t.Errorf("expected package %q in %v", "a/b", got)
	}
}

// TestCartography_QueryGraph_Importers tests the importers direction: packages
// that import the queried package.
func TestCartography_QueryGraph_Importers(t *testing.T) {
	c := &cartography{
		summaries: map[string]string{},
		importers: map[string][]string{
			"internal/store": {"internal/funnel", "internal/cli"},
		},
	}
	importerList, importList := c.QueryGraph("internal/store", "importers")
	if len(importList) != 0 {
		t.Fatalf("importers direction: expected empty importList, got %v", importList)
	}
	if len(importerList) != 2 {
		t.Fatalf("expected 2 importers, got %v", importerList)
	}
	// Results must be sorted.
	if importerList[0] != "internal/cli" || importerList[1] != "internal/funnel" {
		t.Fatalf("expected sorted importers, got %v", importerList)
	}
}

// TestCartography_QueryGraph_Imports tests the imports direction: packages that
// the queried package imports (derived by inverting the importers map).
func TestCartography_QueryGraph_Imports(t *testing.T) {
	c := &cartography{
		summaries: map[string]string{},
		importers: map[string][]string{
			"internal/store": {"internal/funnel"}, // funnel imports store
		},
	}
	importerList, importList := c.QueryGraph("internal/funnel", "imports")
	if len(importerList) != 0 {
		t.Fatalf("imports direction: expected empty importerList, got %v", importerList)
	}
	if len(importList) != 1 || importList[0] != "internal/store" {
		t.Fatalf("expected [internal/store] in imports, got %v", importList)
	}
}

// TestCartography_QueryGraph_Both tests that direction "both" returns both
// sides of the graph.
func TestCartography_QueryGraph_Both(t *testing.T) {
	c := &cartography{
		summaries: map[string]string{},
		importers: map[string][]string{
			"internal/store":  {"internal/funnel"}, // funnel imports store
			"internal/funnel": {"internal/cli"},    // cli imports funnel
		},
	}
	// internal/funnel: importers=[internal/cli], imports=[internal/store]
	importerList, importList := c.QueryGraph("internal/funnel", "both")
	if len(importerList) != 1 || importerList[0] != "internal/cli" {
		t.Fatalf("expected [internal/cli] as importers, got %v", importerList)
	}
	if len(importList) != 1 || importList[0] != "internal/store" {
		t.Fatalf("expected [internal/store] as imports, got %v", importList)
	}
}

// TestCartography_QueryGraph_UnknownPkg tests that an unknown package returns
// empty slices (not an error).
func TestCartography_QueryGraph_UnknownPkg(t *testing.T) {
	c := &cartography{
		summaries: map[string]string{},
		importers: map[string][]string{},
	}
	importerList, importList := c.QueryGraph("internal/nonexistent", "both")
	if importerList != nil {
		t.Fatalf("expected nil importerList, got %v", importerList)
	}
	if importList != nil {
		t.Fatalf("expected nil importList, got %v", importList)
	}
}

// TestCartography_QueryGraph_NilReceiver tests that a nil cartography (feature
// off) returns empty slices without panicking.
func TestCartography_QueryGraph_NilReceiver(t *testing.T) {
	var c *cartography
	importerList, importList := c.QueryGraph("internal/store", "both")
	if importerList != nil || importList != nil {
		t.Fatalf("nil receiver must return nil slices, got %v / %v", importerList, importList)
	}
}

// TestCartography_QueryGraph_Sorted tests that results are deterministically
// sorted regardless of map iteration order.
func TestCartography_QueryGraph_Sorted(t *testing.T) {
	c := &cartography{
		summaries: map[string]string{},
		importers: map[string][]string{
			"internal/store": {"internal/z", "internal/a", "internal/m"},
		},
	}
	importerList, _ := c.QueryGraph("internal/store", "importers")
	for i := 1; i < len(importerList); i++ {
		if importerList[i] < importerList[i-1] {
			t.Fatalf("importerList not sorted: %v", importerList)
		}
	}
}

// --- lazy-pass tests (newCartographer / ensureContextFor) ---

// openLazyFixture returns a Funnel configured with Cartographer=true and the
// standard sub/ fixture. It also returns a snapshot and fps map ready for use.
func openLazyFixture(t *testing.T) (*store.Store, *ingest.Repo, *Funnel, *scriptedClient, *ingest.Snapshot, map[string]string) {
	t.Helper()
	ctx := context.Background()
	st, repo := openCartographyFixture(t)
	finder := newScriptedClient()
	finder.fallback = `{"summary":"Lazy test summary."}`
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Features: FeatureFlags{Cartographer: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := repo.Snapshot(ctx, ingest.ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	fps, err := repo.Fingerprints(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	return st, repo, f, finder, snap, fps
}

// TestLazyCartography_SingleGeneration asserts that the first ensureContextFor
// call for an un-summarized package triggers exactly ONE summarizePackage/client
// call and persists the row (verified via GetPackageSummaries round-trip).
func TestLazyCartography_SingleGeneration(t *testing.T) {
	ctx := context.Background()
	st, _, f, finder, snap, fps := openLazyFixture(t)
	targets := []string{"sub/sub.go"}

	cart := f.newCartographer(ctx, &Result{}, finder, snap, targets, fps, nil)
	if cart == nil {
		t.Fatal("newCartographer returned nil; expected non-nil with Cartographer=true")
	}

	before := summaryCallCount(finder)

	got := cart.ensureContextFor(ctx, targets)
	if got == "" {
		t.Error("ensureContextFor returned empty string; expected injection block")
	}
	if !strings.Contains(got, "REPO CONTEXT — package summaries") {
		t.Errorf("missing header in: %q", got)
	}

	after := summaryCallCount(finder)
	if after-before != 1 {
		t.Errorf("expected exactly 1 summary client call, got %d", after-before)
	}

	// Verify row persisted.
	rows, err := st.GetPackageSummaries(ctx, []string{"sub"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rows["sub"]; !ok {
		t.Error("expected persisted PackageSummary for 'sub' after ensureContextFor")
	}
}

// TestLazyCartography_MemoHit asserts that a second ensureContextFor call over
// the same package triggers zero new client calls (memo satisfied).
func TestLazyCartography_MemoHit(t *testing.T) {
	ctx := context.Background()
	_, _, f, finder, snap, fps := openLazyFixture(t)
	targets := []string{"sub/sub.go"}

	cart := f.newCartographer(ctx, &Result{}, finder, snap, targets, fps, nil)

	// First call — populates memo.
	_ = cart.ensureContextFor(ctx, targets)
	after1 := summaryCallCount(finder)

	// Second call — memo hit; no new generation.
	_ = cart.ensureContextFor(ctx, targets)
	after2 := summaryCallCount(finder)

	if after2 != after1 {
		t.Errorf("second ensureContextFor triggered %d additional client calls; expected 0", after2-after1)
	}
}

// TestLazyCartography_Singleflight asserts that two concurrent ensureContextFor
// calls on the same un-summarized package produce exactly ONE client call.
func TestLazyCartography_Singleflight(t *testing.T) {
	ctx := context.Background()
	_, _, f, finder, snap, fps := openLazyFixture(t)
	targets := []string{"sub/sub.go"}

	cart := f.newCartographer(ctx, &Result{}, finder, snap, targets, fps, nil)

	// Add a small delay to the client so both goroutines enter sf.Do
	// before the first one returns (otherwise the second may hit the memo).
	// We install a per-call sleep via a scriptedClient wrapper; since
	// scriptedClient has a fallback we just rely on both goroutines racing
	// the real singleflight group — the deduplication guarantee holds
	// regardless of scheduling.
	var wg sync.WaitGroup
	wg.Add(2)
	errs := make([]string, 2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer wg.Done()
			got := cart.ensureContextFor(ctx, targets)
			if got == "" {
				errs[i] = "ensureContextFor returned empty; expected injection block"
			}
		}()
	}
	wg.Wait()
	for _, e := range errs {
		if e != "" {
			t.Error(e)
		}
	}

	if n := summaryCallCount(finder); n != 1 {
		t.Errorf("expected exactly 1 summary client call across 2 concurrent goroutines, got %d", n)
	}
}

// TestLazyCartography_BudgetHardSkip asserts that when the budget hard limit
// is already tripped, ensureContextFor skips generation and renders only
// cached/memoized summaries (returns "" if none cached).
func TestLazyCartography_BudgetHardSkip(t *testing.T) {
	ctx := context.Background()
	_, _, f, finder, snap, fps := openLazyFixture(t)
	targets := []string{"sub/sub.go"}

	// Construct a budget where finderOverHard() == true:
	// use a finderPool of capacity 1 and immediately spend 1 token.
	pool := agent.NewBudgetPool(1)
	pool.Add(1) // now Spent() == limit => finderOverHard() true
	b := &budgetState{
		budget:      1,
		finderBudget: 1,
		finderPool:  pool,
	}

	cart := f.newCartographer(ctx, &Result{}, finder, snap, targets, fps, b)

	got := cart.ensureContextFor(ctx, targets)
	// No cached summaries in store, budget hard — must return "".
	if got != "" {
		t.Errorf("expected empty string with budget hard-tripped; got %q", got)
	}
	if n := summaryCallCount(finder); n != 0 {
		t.Errorf("expected 0 client calls with budget hard-tripped, got %d", n)
	}
}

// TestLazyCartography_OffPath asserts that Features.Cartographer=false causes
// newCartographer to return nil and ensureContextFor on nil to return "".
func TestLazyCartography_OffPath(t *testing.T) {
	ctx := context.Background()
	st, repo := openCartographyFixture(t)
	finder := newScriptedClient()
	finder.fallback = `{"summary":"should not appear"}`
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Features: FeatureFlags{Cartographer: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, snapErr := repo.Snapshot(ctx, ingest.ScanFilter{})
	if snapErr != nil {
		t.Fatal(snapErr)
	}
	fps, fpsErr := repo.Fingerprints(ctx, snap)
	if fpsErr != nil {
		t.Fatal(fpsErr)
	}

	cart := f.newCartographer(ctx, &Result{}, finder, snap, []string{"sub/sub.go"}, fps, nil)
	if cart != nil {
		t.Errorf("newCartographer with Cartographer=false returned non-nil: %+v", cart)
	}

	var c *cartography
	if got := c.ensureContextFor(ctx, []string{"sub/sub.go"}); got != "" {
		t.Errorf("nil ensureContextFor returned %q; want \"\"", got)
	}
	if n := summaryCallCount(finder); n != 0 {
		t.Errorf("expected 0 client calls on off path, got %d", n)
	}
}

// TestLazyCartography_StoreSeedHit verifies that a pre-seeded store row is
// picked up as a store hit (no LLM call) on first ensureContextFor.
func TestLazyCartography_StoreSeedHit(t *testing.T) {
	ctx := context.Background()
	st, _, f, finder, snap, fps := openLazyFixture(t)
	targets := []string{"sub/sub.go"}

	// Pre-seed the summary row with the correct fingerprint.
	pkgs := packagesSpanned(targets)
	members := pkgs["sub"]
	fp := packageFingerprint("sub", members, fps)
	want := "Pre-seeded summary."
	if err := st.UpsertPackageSummaries(ctx, []store.PackageSummary{
		{Pkg: "sub", Fingerprint: fp, Summary: want},
	}); err != nil {
		t.Fatal(err)
	}

	// finder.fallback is set but should not be called.
	finder.fallback = `{"summary":"should not be called"}`
	cart := f.newCartographer(ctx, &Result{}, finder, snap, targets, fps, nil)

	got := cart.ensureContextFor(ctx, targets)
	if !strings.Contains(got, want) {
		t.Errorf("expected pre-seeded summary %q in output; got %q", want, got)
	}
	if n := summaryCallCount(finder); n != 0 {
		t.Errorf("expected 0 LLM calls (store hit); got %d", n)
	}
}

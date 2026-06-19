package funnel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
// (the cartographer's pass reuses cached rows whose fingerprint matches)
// and assert the resulting injection block contains the seeded text.
//
// We test through the cartograph -> contextFor -> finderTask path so the
// test exercises the real injection logic without spinning up the full
// Sweep pipeline (which would also exercise the verifier / triage).
func TestCartography_InjectIntoFinderTask(t *testing.T) {
	ctx := context.Background()
	st, repo := openCartographyFixture(t)

	finder := newScriptedClient()
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Cartographer: true,
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

	cart := f.cartograph(ctx, &Result{}, finder, snap, targets, fps, nil)
	if cart == nil {
		t.Fatal("cartograph returned nil; expected non-nil with Cartographer=true")
	}
	if cart.summaries[pkg] != want {
		t.Errorf("cartography.summaries[%q] = %q, want %q", pkg, cart.summaries[pkg], want)
	}

	task := finderTask(targets, nil, cart.contextFor(targets))
	if !strings.Contains(task, "REPO CONTEXT — package summaries") {
		t.Errorf("finder task missing cartography block; got:\n%s", task)
	}
	if !strings.Contains(task, want) {
		t.Errorf("finder task missing seeded summary text %q; got:\n%s", want, task)
	}

	// And the injection must be in the TASK message (the first user
	// message), NOT the system prompt. We constructed the task above
	// directly so this is by construction — but a separate path
	// assertion is worth pinning: the cartography block never escapes
	// into the system prompt.
	for _, r := range finder.allRequests() {
		if strings.Contains(r.System, "REPO CONTEXT") {
			t.Errorf("cartography block leaked into system prompt: %s", r.System)
		}
	}
}

// TestCartography_DisabledByDefault pins the regression-guard: with
// Options.Cartographer=false (the default), no summary block appears in
// finder tasks and the cartograph pass returns nil.
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

	cart := f.cartograph(ctx, &Result{}, finder, snap, targets, fps, nil)
	if cart != nil {
		t.Errorf("cartograph with Cartographer=false returned %+v, want nil", cart)
	}

	task := finderTask(targets, nil, "")
	if strings.Contains(task, "REPO CONTEXT") {
		t.Errorf("finder task contains REPO CONTEXT block despite Cartographer=false:\n%s", task)
	}
}

// TestCartography_FingerprintCache pins the cache discipline: a second
// call with the same fingerprints does NOT re-call the LLM. We rely on
// the scripted client's allRequests log: any completion with the
// cartography system prompt counts as a summary-generation call.
func TestCartography_FingerprintCache(t *testing.T) {
	ctx := context.Background()
	st, repo := openCartographyFixture(t)

	finder := newScriptedClient()
	finder.fallback = `{"summary":"A canned summary."}`
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Cartographer: true,
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

	_ = f.cartograph(ctx, &Result{}, finder, snap, targets, fps, nil)
	afterFirst := summaryCallCount(finder)
	if afterFirst == 0 {
		t.Fatalf("expected at least one summary completion on first pass, got 0")
	}

	_ = f.cartograph(ctx, &Result{}, finder, snap, targets, fps, nil)
	afterSecond := summaryCallCount(finder)
	if afterSecond != afterFirst {
		t.Errorf("summary completion count rose from %d to %d on second pass; expected no regeneration", afterFirst, afterSecond)
	}
}

// TestCartography_DegradesOnLLMError pins the graceful-degrade contract:
// if the summary-generation completion errors, the cartograph pass still
// returns a non-nil cartography (with empty summaries), and the
// injection block is empty.
func TestCartography_DegradesOnLLMError(t *testing.T) {
	ctx := context.Background()
	st, repo := openCartographyFixture(t)

	finder := newScriptedClient()
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Cartographer: true,
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

	errClient := &errClient{err: errFakeLLM}
	cart := f.cartograph(ctx, &Result{}, errClient, snap, targets, fps, nil)
	if cart == nil {
		t.Fatal("cartograph returned nil on LLM error; expected non-nil empty cartography for graceful degrade")
	}
	if len(cart.summaries) != 0 {
		t.Errorf("expected no summaries after LLM error, got %d", len(cart.summaries))
	}
	if got := cart.contextFor(targets); got != "" {
		t.Errorf("contextFor returned non-empty block despite no summaries: %q", got)
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
// not the raw reasoning.
func TestCartography_StripsThinkBlock(t *testing.T) {
	ctx := context.Background()
	st, repo := openCartographyFixture(t)

	finder := newScriptedClient()
	finder.fallback = "<think>\nThe user wants a summary. Let me reason about the package...\n</think>\n\n{\"summary\":\"Purpose: a helper package.\"}"
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{Cartographer: true})
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

	cart := f.cartograph(ctx, &Result{}, finder, snap, []string{"sub/sub.go"}, fps, nil)
	if cart == nil {
		t.Fatal("nil cartography")
	}
	if got := cart.summaries["sub"]; got != "Purpose: a helper package." {
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
// REPAIRED on a second round-trip rather than silently dropped (the old
// client.Complete path returned the raw text and lost a malformed answer). The
// sequence client returns junk first, then a schema-valid object; the stored
// summary must be the repaired value, and a second completion must have run.
func TestCartography_RepairsMalformedSummary(t *testing.T) {
	ctx := context.Background()
	st, repo := openCartographyFixture(t)

	valid := `{"summary":"Repaired package summary."}`
	finder := newScriptedSequenceClient(valid, "this is not json, only reasoning prose", valid)
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{Cartographer: true})
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

	cart := f.cartograph(ctx, &Result{}, finder, snap, []string{"sub/sub.go"}, fps, nil)
	if cart == nil {
		t.Fatal("nil cartography")
	}
	if got := cart.summaries["sub"]; got != "Repaired package summary." {
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

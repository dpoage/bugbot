package funnel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/bugbot/internal/treesitter"
)

// ---- repo helpers ----

// makeRepoWithFiles creates files in a temp dir and returns the dir path.
// Does NOT create a git repo; for impactSweep integration tests that need git,
// see makeGitRepo.
func makeRepoWithFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// makeGitRepo creates a git repo from files, commits everything, and returns
// the repo path. Skips the test if git is unavailable.
func makeGitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := makeRepoWithFiles(t, files)
	runImpactGit(t, dir, "init", "-q")
	runImpactGit(t, dir, "add", ".")
	runImpactGit(t, dir, "commit", "-q", "-m", "seed", "--allow-empty")
	return dir
}

func runImpactGit(t *testing.T, dir string, args ...string) {
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

// openImpactStore opens a migrated store in a temp dir.
func openImpactStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// makeImpactFunnel builds a Funnel over a git repo for impactSweep tests.
func makeImpactFunnel(t *testing.T, st *store.Store, repoDir string, client *scriptedClient) *Funnel {
	t.Helper()
	ctx := context.Background()
	repo, err := ingest.Open(ctx, repoDir)
	if err != nil {
		t.Skipf("ingest.Open: %v (git required)", err)
	}
	f, err := New(RoleClients{Finder: client, Verifier: client}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New funnel: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// seedFinding inserts a finding into the store and returns the stored copy (with ID set).
func seedFinding(t *testing.T, st *store.Store, f store.Finding) store.Finding {
	t.Helper()
	ctx := context.Background()
	got, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("UpsertFinding: %v", err)
	}
	return got
}

// makeImpactFinding returns a minimal Finding for impact-sweep tests.
func makeImpactFinding(id, file string, line int, sev domain.Severity) store.Finding {
	return store.Finding{
		ID:          id,
		Fingerprint: "fp-" + id,
		Title:       "prose title: some defect description",
		File:        file,
		Line:        line,
		Severity:    sev,
		Tier:        domain.TierVerified,
		Status:      store.StatusOpen,
	}
}

// ---- real classifyReachability unit tests (real tree-sitter, real files) ----

// TestClassify_UnexportedDeadCpp: unexported C++ free function with zero
// callers → reachKnownDownrank / low with caller-trace rationale.
func TestClassify_UnexportedDeadCpp(t *testing.T) {
	repo := makeRepoWithFiles(t, map[string]string{
		"src/dead.cpp": "void deadHelper() {\n    // never called\n}\n",
		"src/main.cpp": "int main() { return 0; }\n",
	})
	ts := treesitter.New(repo)
	fi := makeImpactFinding("d1", "src/dead.cpp", 1, domain.SeverityHigh)
	r := classifyReachability(&fi, repo, ts)

	if r.class != reachKnownDownrank {
		t.Errorf("class=%d rationale=%q: want reachKnownDownrank", r.class, r.rationale)
	}
	if r.severity != domain.SeverityLow {
		t.Errorf("severity=%s, want low", r.severity)
	}
	if !strings.Contains(r.rationale, "zero non-test callers") {
		t.Errorf("rationale should mention zero callers: %q", r.rationale)
	}
	t.Logf("rationale: %s", r.rationale)
}

// TestClassify_ReachableCpp: a C++ function called from another file → keep.
func TestClassify_ReachableCpp(t *testing.T) {
	repo := makeRepoWithFiles(t, map[string]string{
		"src/util.cpp": "void processRequest() {\n    // handles requests\n}\n",
		"src/main.cpp": "void processRequest();\nint main() { processRequest(); return 0; }\n",
	})
	ts := treesitter.New(repo)
	fi := makeImpactFinding("r1", "src/util.cpp", 1, domain.SeverityHigh)
	r := classifyReachability(&fi, repo, ts)

	if r.class == reachKnownDownrank {
		t.Errorf("reachable function wrongly downranked: rationale=%q", r.rationale)
	}
	if r.class == reachKnownKeep && r.severity != domain.SeverityHigh {
		t.Errorf("severity changed for reachable function: %s → %s", domain.SeverityHigh, r.severity)
	}
	t.Logf("class=%d rationale=%s", r.class, r.rationale)
}

// TestClassify_WindowsGoFile: _windows.go → platform-gated → low.
func TestClassify_WindowsGoFile(t *testing.T) {
	repo := makeRepoWithFiles(t, map[string]string{
		"internal/os/syscall_windows.go": "package os\nfunc winSyscall() {}\n",
	})
	ts := treesitter.New(repo)
	fi := makeImpactFinding("w1", "internal/os/syscall_windows.go", 2, domain.SeverityHigh)
	r := classifyReachability(&fi, repo, ts)

	if r.class != reachKnownDownrank {
		t.Errorf("_windows.go should be downranked, got class=%d", r.class)
	}
	if r.severity != domain.SeverityLow {
		t.Errorf("severity=%s, want low", r.severity)
	}
	if !strings.Contains(r.rationale, "platform-only") {
		t.Errorf("rationale: %q", r.rationale)
	}
}

// TestClassify_IfdefWin32: C file with top-level #ifdef _WIN32 → low.
func TestClassify_IfdefWin32(t *testing.T) {
	repo := makeRepoWithFiles(t, map[string]string{
		"win_impl.c": "#ifdef _WIN32\nvoid winFunc() {}\n#endif\n",
	})
	ts := treesitter.New(repo)
	fi := makeImpactFinding("wc1", "win_impl.c", 2, domain.SeverityMedium)
	r := classifyReachability(&fi, repo, ts)

	if r.class != reachKnownDownrank {
		t.Errorf("#ifdef _WIN32 file should be downranked, got class=%d", r.class)
	}
	if r.severity != domain.SeverityLow {
		t.Errorf("severity=%s, want low", r.severity)
	}
}

// TestClassify_TestFile: finding in a _test.go file → low.
func TestClassify_TestFile(t *testing.T) {
	repo := makeRepoWithFiles(t, map[string]string{
		"internal/foo/foo_test.go": "package foo\nfunc TestHelper() {}\n",
	})
	ts := treesitter.New(repo)
	fi := makeImpactFinding("t1", "internal/foo/foo_test.go", 2, domain.SeverityMedium)
	r := classifyReachability(&fi, repo, ts)

	if r.class != reachKnownDownrank {
		t.Errorf("test file should be downranked, got class=%d", r.class)
	}
	if r.severity != domain.SeverityLow {
		t.Errorf("severity=%s, want low", r.severity)
	}
	if !strings.Contains(r.rationale, "test file") {
		t.Errorf("rationale should mention test file: %q", r.rationale)
	}
}

// TestClassify_ExportedGoSymbol_KeptWithRationale: exported Go function with
// zero in-repo callers → reachKnownKeep with non-empty rationale (not silently
// kept, NOT deterministically downranked). This is the bugbot-bar regression:
// the finding must not be dropped to low.
func TestClassify_ExportedGoSymbol_KeptWithRationale(t *testing.T) {
	repo := makeRepoWithFiles(t, map[string]string{
		"internal/api/parse.go": "package api\n\nfunc ParseRequest() error { return nil }\n",
	})
	ts := treesitter.New(repo)
	fi := makeImpactFinding("e1", "internal/api/parse.go", 3, domain.SeverityHigh)
	r := classifyReachability(&fi, repo, ts)

	// MUST NOT be downranked (bugbot-bar regression).
	if r.class == reachKnownDownrank {
		t.Errorf("exported Go symbol must NOT be deterministically downranked; got rationale=%q", r.rationale)
	}
	// Kept with non-empty rationale (not silently kept).
	if r.class != reachKnownKeep {
		t.Errorf("exported Go symbol with zero callers should be reachKnownKeep, got class=%d", r.class)
	}
	if r.severity != domain.SeverityHigh {
		t.Errorf("severity must stay high, got %s", r.severity)
	}
	if r.rationale == "" {
		t.Error("rationale must be non-empty (not silently kept)")
	}
	t.Logf("class=%d rationale=%q", r.class, r.rationale)
}

// TestClassify_HeaderDecl_Ambiguous: C++ header with zero callers → reachAmbiguous
// (routed to LLM so dead header members can be downranked by the adjudicator).
// This covers the named dead cases: Surface ctor, RegisterableFuncType::bind.
func TestClassify_HeaderDecl_Ambiguous(t *testing.T) {
	repo := makeRepoWithFiles(t, map[string]string{
		"include/registry.hpp": "class RegisterableFuncType {\npublic:\n    void bind();\n};\n",
	})
	ts := treesitter.New(repo)
	fi := makeImpactFinding("h1", "include/registry.hpp", 3, domain.SeverityHigh)
	r := classifyReachability(&fi, repo, ts)

	// Must be routed to LLM (not kept, not deterministically downranked).
	if r.class != reachAmbiguous {
		t.Errorf("header decl with zero callers should be reachAmbiguous, got class=%d callerFacts=%q", r.class, r.callerFacts)
	}
	if r.callerFacts == "" {
		t.Error("callerFacts must be non-empty for LLM prompt")
	}
	t.Logf("class=%d callerFacts=%q", r.class, r.callerFacts)
}

// TestClassify_SameNameCollision_Ambiguous: when a bare symbol name has
// callers in >5 distinct files, route to adjudication (collision risk).
func TestClassify_SameNameCollision_Ambiguous(t *testing.T) {
	files := map[string]string{
		"src/dead.cpp": "void write() { /* dead A */ }\n",
		"src/a.cpp":    "void write();\nvoid a() { write(); }\n",
		"src/b.cpp":    "void write();\nvoid b() { write(); }\n",
		"src/c.cpp":    "void write();\nvoid c() { write(); }\n",
		"src/d.cpp":    "void write();\nvoid d() { write(); }\n",
		"src/e.cpp":    "void write();\nvoid e() { write(); }\n",
		"src/f.cpp":    "void write();\nvoid f() { write(); }\n",
	}
	repo := makeRepoWithFiles(t, files)
	ts := treesitter.New(repo)
	fi := makeImpactFinding("sc1", "src/dead.cpp", 1, domain.SeverityHigh)
	r := classifyReachability(&fi, repo, ts)

	// 6 distinct caller files → distinctFiles > 5 → ambiguous.
	if r.class != reachAmbiguous {
		t.Errorf("same-name collision (>5 caller files): want reachAmbiguous, got class=%d rationale=%q", r.class, r.rationale)
	}
}

// TestClassify_NamedDeadCases: the named cases from the bead's corpus.
// JSON::write dead in .cpp → downranked. Surface ctor in .hpp → kept with
// non-empty rationale (not silently kept). renderFrame reachable → kept.
func TestClassify_NamedDeadCases(t *testing.T) {
	repo := makeRepoWithFiles(t, map[string]string{
		"src/json.cpp":        "namespace JSON {\nvoid write() {\n    // dead\n}\n}\n",
		"include/surface.hpp": "class Surface {\npublic:\n    Surface(int w, int h);\n};\n",
		"src/render.cpp":      "void renderFrame() {\n    // called every frame\n}\n",
		"src/main.cpp":        "void renderFrame();\nint main() {\n    while(1) { renderFrame(); }\n}\n",
	})
	ts := treesitter.New(repo)

	// JSON::write in .cpp, zero callers → should be downranked (not kept).
	jsonFi := makeImpactFinding("jw1", "src/json.cpp", 2, domain.SeverityHigh)
	r := classifyReachability(&jsonFi, repo, ts)
	if r.class == reachKnownKeep {
		t.Errorf("JSON::write dead in .cpp: should not be kept; got rationale=%q", r.rationale)
	}
	t.Logf("JSON::write: class=%d rationale=%q", r.class, r.rationale)

	// Surface ctor in .hpp → reachAmbiguous (routed to LLM so dead header members
	// can be downranked). Must NOT be deterministically kept or downranked.
	surfFi := makeImpactFinding("sc1", "include/surface.hpp", 3, domain.SeverityHigh)
	r = classifyReachability(&surfFi, repo, ts)
	if r.class != reachAmbiguous {
		t.Errorf("Surface ctor in .hpp: should be reachAmbiguous, got class=%d callerFacts=%q", r.class, r.callerFacts)
	}
	if r.callerFacts == "" {
		t.Errorf("Surface ctor in .hpp: callerFacts must be non-empty")
	}
	t.Logf("Surface ctor: class=%d callerFacts=%q", r.class, r.callerFacts)

	// renderFrame is reachable → must not be downranked.
	renderFi := makeImpactFinding("rf1", "src/render.cpp", 1, domain.SeverityHigh)
	r = classifyReachability(&renderFi, repo, ts)
	if r.class == reachKnownDownrank {
		t.Errorf("renderFrame is reachable: must not be downranked; got rationale=%q", r.rationale)
	}
	t.Logf("renderFrame: class=%d rationale=%q", r.class, r.rationale)
}

// ---- impactSweep integration tests (real store, real git, count-tracking client) ----

// TestImpactSweep_DeadCodeDownrank: impactSweep persists severity=low and
// verdict_detail for a dead unexported C++ function; zero LLM calls.
func TestImpactSweep_DeadCodeDownrank(t *testing.T) {
	repoDir := makeGitRepo(t, map[string]string{
		"src/dead.cpp": "void deadHelper() {}\n",
		"src/main.cpp": "int main() { return 0; }\n",
	})

	st := openImpactStore(t)
	ctx := context.Background()
	client := newScriptedClient()
	f := makeImpactFunnel(t, st, repoDir, client)

	fi := seedFinding(t, st, makeImpactFinding("dead1", "src/dead.cpp", 1, domain.SeverityHigh))
	findings := []store.Finding{fi}
	result := &Result{}
	f.impactSweep(ctx, findings, repoDir, client, false, result)

	// Zero LLM calls: deadHelper is unexported, non-header, zero callers → deterministic.
	if n := client.callCount(); n != 0 {
		t.Errorf("expected 0 LLM calls, got %d", n)
	}
	// In-memory severity downranked.
	if findings[0].Severity != domain.SeverityLow {
		t.Errorf("in-memory severity=%s, want low", findings[0].Severity)
	}
	if !strings.Contains(findings[0].VerdictDetail, "zero non-test callers") {
		t.Errorf("VerdictDetail=%q, want 'zero non-test callers'", findings[0].VerdictDetail)
	}
	// Persisted to store.
	got, err := st.GetFinding(ctx, fi.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.Severity != domain.SeverityLow {
		t.Errorf("persisted severity=%s, want low", got.Severity)
	}
	if !strings.Contains(got.VerdictDetail, "zero non-test callers") {
		t.Errorf("persisted VerdictDetail=%q", got.VerdictDetail)
	}
}

// TestImpactSweep_ExportedGoKeptWithRationale: exported Go symbol with zero
// in-repo callers → kept with non-empty verdict_detail; zero LLM calls.
func TestImpactSweep_ExportedGoKeptWithRationale(t *testing.T) {
	repoDir := makeGitRepo(t, map[string]string{
		"internal/api/parse.go": "package api\n\nfunc ParseRequest() error { return nil }\n",
	})

	st := openImpactStore(t)
	ctx := context.Background()
	client := newScriptedClient()

	f := makeImpactFunnel(t, st, repoDir, client)

	fi := seedFinding(t, st, makeImpactFinding("exp1", "internal/api/parse.go", 3, domain.SeverityHigh))
	findings := []store.Finding{fi}
	result := &Result{}
	f.impactSweep(ctx, findings, repoDir, client, false, result)

	// Zero LLM calls: exported Go is deterministically kept-with-rationale.
	if n := client.callCount(); n != 0 {
		t.Errorf("expected 0 LLM calls for exported Go (keep-with-rationale), got %d", n)
	}
	// Severity must stay high.
	if findings[0].Severity != domain.SeverityHigh {
		t.Errorf("in-memory severity=%s, want high", findings[0].Severity)
	}
	// VerdictDetail must be non-empty (not silently kept).
	if findings[0].VerdictDetail == "" {
		t.Error("VerdictDetail must be non-empty for exported Go with zero callers")
	}
	got, err := st.GetFinding(ctx, fi.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.Severity != domain.SeverityHigh {
		t.Errorf("persisted severity=%s, want high", got.Severity)
	}
	if got.VerdictDetail == "" {
		t.Error("persisted VerdictDetail should be non-empty")
	}
}

// TestImpactSweep_HeaderMemberDownranked: a dead .hpp member (RegisterableFuncType::bind,
// zero callers) is routed to LLM adjudication → exactly one LLM call →
// persisted as low with verdict_detail. Covers the acceptance-(2) named dead cases.
func TestImpactSweep_HeaderMemberDownranked(t *testing.T) {
	repoDir := makeGitRepo(t, map[string]string{
		"include/registry.hpp": "class RegisterableFuncType {\npublic:\n    void bind();\n};\n",
	})

	st := openImpactStore(t)
	ctx := context.Background()
	client := newScriptedClient()
	// LLM adjudicator says dead application header member → low.
	client.fallback = `{"results":[{"id":"hm1","severity":"low","rationale":"RegisterableFuncType::bind has zero callers; dead application header surface"}]}`

	f := makeImpactFunnel(t, st, repoDir, client)

	fi := seedFinding(t, st, makeImpactFinding("hm1", "include/registry.hpp", 3, domain.SeverityHigh))
	findings := []store.Finding{fi}
	result := &Result{}
	f.impactSweep(ctx, findings, repoDir, client, false, result)

	// Exactly one LLM call (header member is ambiguous → adjudicated).
	if n := client.callCount(); n != 1 {
		t.Errorf("expected 1 LLM call for dead header member, got %d", n)
	}
	// In-memory severity downranked by adjudicator.
	if findings[0].Severity != domain.SeverityLow {
		t.Errorf("in-memory severity=%s, want low (LLM adjudicator downranked)", findings[0].Severity)
	}
	if findings[0].VerdictDetail == "" {
		t.Error("VerdictDetail must be non-empty after LLM adjudication")
	}
	// Persisted to store.
	got, err := st.GetFinding(ctx, fi.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.Severity != domain.SeverityLow {
		t.Errorf("persisted severity=%s, want low", got.Severity)
	}
	if got.VerdictDetail == "" {
		t.Error("persisted VerdictDetail should be non-empty")
	}
}

// TestImpactSweep_HeaderMemberKeptByAdjudicator: a .hpp symbol that looks like
// a public API (adjudicator keeps it high) → persisted at original severity.
func TestImpactSweep_HeaderMemberKeptByAdjudicator(t *testing.T) {
	repoDir := makeGitRepo(t, map[string]string{
		"include/api.hpp": "class PublicAPI {\npublic:\n    void connect();\n};\n",
	})

	st := openImpactStore(t)
	ctx := context.Background()
	client := newScriptedClient()
	// LLM adjudicator decides this is a real public API → keep high.
	client.fallback = `{"results":[{"id":"pk1","severity":"high","rationale":"PublicAPI::connect is a public library interface; external callers cannot be ruled out"}]}`

	f := makeImpactFunnel(t, st, repoDir, client)

	fi := seedFinding(t, st, makeImpactFinding("pk1", "include/api.hpp", 3, domain.SeverityHigh))
	findings := []store.Finding{fi}
	result := &Result{}
	f.impactSweep(ctx, findings, repoDir, client, false, result)

	if n := client.callCount(); n != 1 {
		t.Errorf("expected 1 LLM call, got %d", n)
	}
	// Severity must stay high (adjudicator kept it).
	if findings[0].Severity != domain.SeverityHigh {
		t.Errorf("in-memory severity=%s, want high (adjudicator kept)", findings[0].Severity)
	}
	got, err := st.GetFinding(ctx, fi.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.Severity != domain.SeverityHigh {
		t.Errorf("persisted severity=%s, want high", got.Severity)
	}
}

// TestImpactSweep_NilClientSkipsAmbiguous: when verifierClient is nil and there
// are ambiguous findings (C/C++ header with zero callers), zero LLM calls are
// made and a note is recorded.
func TestImpactSweep_NilClientSkipsAmbiguous(t *testing.T) {
	// A .hpp finding is ambiguous (header → LLM path); nil client triggers the note.
	repoDir := makeGitRepo(t, map[string]string{
		"include/registry.hpp": "class Foo {\npublic:\n    void bar();\n};\n",
	})

	st := openImpactStore(t)
	ctx := context.Background()
	buildClient := newScriptedClient()
	f := makeImpactFunnel(t, st, repoDir, buildClient)

	fi := seedFinding(t, st, makeImpactFinding("nilc1", "include/registry.hpp", 3, domain.SeverityHigh))
	findings := []store.Finding{fi}
	result := &Result{}
	// nil client: header is ambiguous → nil-client guard fires → note recorded.
	f.impactSweep(ctx, findings, repoDir, nil, false, result)

	if n := buildClient.callCount(); n != 0 {
		t.Errorf("expected 0 LLM calls with nil client, got %d", n)
	}
	// Note must be recorded: "no LLM client" degraded message.
	if len(result.Skipped) == 0 {
		t.Error("expected a Skipped note for nil-client degraded path")
	}
	t.Logf("note: %v", result.Skipped)
}

// TestImpactSweep_AtMostOneLLMCall: even with multiple ambiguous findings,
// adjudicateImpact makes exactly one batched LLM call.
func TestImpactSweep_AtMostOneLLMCall(t *testing.T) {
	ctx := context.Background()
	client := newScriptedClient()
	client.fallback = `{"results":[
		{"id":"id-a","severity":"low","rationale":"dead header API"},
		{"id":"id-b","severity":"low","rationale":"dead header API too"}
	]}`

	ambiguous := []ambiguousEntry{
		{idx: 0, fi: store.Finding{ID: "id-a", Title: "finding A", File: "include/a.hpp", Severity: "high"}},
		{idx: 1, fi: store.Finding{ID: "id-b", Title: "finding B", File: "include/b.hpp", Severity: "medium"}},
	}

	var sink captureSink
	results, err := adjudicateImpact(ctx, client, ambiguous, &sink)
	if err != nil {
		t.Fatalf("adjudicateImpact: %v", err)
	}
	// The batched re-assessment must surface as a "severity" agent: one
	// started + one finished bracket, regardless of how many findings it
	// processed in the single call.
	if got := len(sink.byKind(progress.KindAgentStarted)); got != 1 {
		t.Errorf("severity agent_started count = %d, want 1", got)
	}
	fin := sink.byKind(progress.KindAgentFinished)
	if len(fin) != 1 {
		t.Fatalf("severity agent_finished count = %d, want 1", len(fin))
	}
	if fin[0].Role != progress.RoleSeverity {
		t.Errorf("agent_finished role = %q, want %q", fin[0].Role, progress.RoleSeverity)
	}
	if n := client.callCount(); n != 1 {
		t.Errorf("expected exactly 1 LLM call regardless of count, got %d", n)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.severity != "low" {
			t.Errorf("expected low, got %q", r.severity)
		}
		if r.rationale == "" {
			t.Error("expected non-empty rationale")
		}
	}
}

// ---- helper function tests (real functions, not stubs) ----

func TestIsTestFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"internal/foo/foo_test.go", true},
		{"internal/foo/foo.go", false},
		{"test/integration.go", true},
		{"internal/testdata/fixture.go", true},
		{"internal/foo/test_helper.py", true},
		{"internal/foo/module.py", false},
		{"src/__tests__/api.test.ts", true},
		{"src/api.spec.js", true},
		{"src/api.js", false},
	}
	for _, tc := range cases {
		if got := isTestFile(tc.path); got != tc.want {
			t.Errorf("isTestFile(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestIsWindowsOrMacOnly(t *testing.T) {
	dir := t.TempDir()
	winC := "win_impl.c"
	if err := os.WriteFile(filepath.Join(dir, winC), []byte("#ifdef _WIN32\nvoid f() {}\n#endif\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	normC := "normal.c"
	if err := os.WriteFile(filepath.Join(dir, normC), []byte("void f() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		want bool
	}{
		{"internal/os/syscall_windows.go", true},
		{"internal/os/syscall_darwin.go", true},
		{"internal/os/syscall_linux.go", false},
		{"src/win32/util.c", true},
		{"src/macos/util.m", true},
		{"src/linux/util.c", false},
		{winC, true},
		{normC, false},
	}
	for _, tc := range cases {
		if got := isWindowsOrMacOnly(tc.path, dir); got != tc.want {
			t.Errorf("isWindowsOrMacOnly(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// ---- SweepDrain integration tests ----

// TestSweepDrain_SweepsUnswept: seeds an unswept open finding (a dead
// unexported C++ function with zero callers — deterministically classified),
// runs SweepDrain, and asserts swept_at is non-zero and UnsweptOpenFindings
// no longer returns it.
func TestSweepDrain_SweepsUnswept(t *testing.T) {
	repoDir := makeGitRepo(t, map[string]string{
		"src/dead.cpp": "void deadHelper() {}\n",
		"src/main.cpp": "int main() { return 0; }\n",
	})

	st := openImpactStore(t)
	ctx := context.Background()
	client := newScriptedClient()
	f := makeImpactFunnel(t, st, repoDir, client)

	fi := seedFinding(t, st, makeImpactFinding("sd-unswept1", "src/dead.cpp", 1, domain.SeverityHigh))

	// Sanity: unswept before drain.
	before, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("UnsweptOpenFindings (before): %v", err)
	}
	found := false
	for _, x := range before {
		if x.ID == fi.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("seeded finding should appear in UnsweptOpenFindings before drain")
	}

	result, err := f.SweepDrain(ctx)
	if err != nil {
		t.Fatalf("SweepDrain: %v", err)
	}
	if result.ScanRunID == "" {
		t.Error("ScanRunID should be non-empty after a non-empty drain")
	}

	// Zero LLM calls: deadHelper is unexported, non-header, zero callers → deterministic.
	if n := client.callCount(); n != 0 {
		t.Errorf("expected 0 LLM calls, got %d", n)
	}

	// swept_at must be set in the store.
	got, err := st.GetFinding(ctx, fi.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.SweptAt.IsZero() {
		t.Error("swept_at must be non-zero after SweepDrain")
	}
	if got.Severity != domain.SeverityLow {
		t.Errorf("severity = %s after sweep, want low", got.Severity)
	}

	// Finding must be excluded from UnsweptOpenFindings after drain.
	after, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("UnsweptOpenFindings (after): %v", err)
	}
	for _, x := range after {
		if x.ID == fi.ID {
			t.Error("swept finding still appears in UnsweptOpenFindings")
		}
	}
}

// TestSweepDrain_Idempotent: after a first drain, UnsweptOpenFindings is empty
// so a second SweepDrain returns an empty Result with no LLM calls.
func TestSweepDrain_Idempotent(t *testing.T) {
	repoDir := makeGitRepo(t, map[string]string{
		"src/dead.cpp": "void deadHelper() {}\n",
		"src/main.cpp": "int main() { return 0; }\n",
	})

	st := openImpactStore(t)
	ctx := context.Background()
	client := newScriptedClient()
	f := makeImpactFunnel(t, st, repoDir, client)

	_ = seedFinding(t, st, makeImpactFinding("sd-idem1", "src/dead.cpp", 1, domain.SeverityHigh))

	// First drain: sweeps the finding.
	if _, err := f.SweepDrain(ctx); err != nil {
		t.Fatalf("first SweepDrain: %v", err)
	}
	callsAfterFirst := client.callCount()

	// Second drain: nothing unswept → no-op.
	result2, err := f.SweepDrain(ctx)
	if err != nil {
		t.Fatalf("second SweepDrain: %v", err)
	}
	if result2.ScanRunID != "" {
		t.Error("second drain should return empty Result (no scan run opened)")
	}
	if len(result2.Findings) != 0 {
		t.Errorf("second drain returned %d findings, want 0", len(result2.Findings))
	}
	// No additional LLM calls on the second drain.
	if n := client.callCount(); n != callsAfterFirst {
		t.Errorf("second drain made %d additional LLM calls, want 0", n-callsAfterFirst)
	}

	// Store confirms empty.
	unswept, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("UnsweptOpenFindings after second drain: %v", err)
	}
	if len(unswept) != 0 {
		t.Errorf("expected 0 unswept findings, got %d", len(unswept))
	}
}

// TestSweepDrain_AmbiguousDeferredRePickedUp: a .hpp finding routed to LLM
// adjudication with a nil verifier client stays swept_at NULL (ambiguous-
// deferred). A subsequent drain with a real (scripted) client sweeps it.
func TestSweepDrain_AmbiguousDeferredRePickedUp(t *testing.T) {
	repoDir := makeGitRepo(t, map[string]string{
		"include/registry.hpp": "class Foo {\npublic:\n    void bar();\n};\n",
	})

	st := openImpactStore(t)
	ctx := context.Background()

	// Build funnel with nil verifier so ambiguous finding is not adjudicated.
	nilClient := newScriptedClient()
	fNil := makeImpactFunnel(t, st, repoDir, nilClient)

	fi := seedFinding(t, st, makeImpactFinding("sd-defer1", "include/registry.hpp", 3, domain.SeverityHigh))

	// Drain 1: verifier is effectively nil for impactSweep (we pass nil directly);
	// call impactSweep directly to simulate nil-client deferred path.
	result1 := &Result{}
	findings1, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("UnsweptOpenFindings: %v", err)
	}
	fNil.impactSweep(ctx, findings1, repoDir, nil, false, result1)

	// Ambiguous .hpp with nil client → swept_at stays NULL → still in unswept.
	if n := nilClient.callCount(); n != 0 {
		t.Errorf("expected 0 LLM calls with nil client, got %d", n)
	}
	got1, err := st.GetFinding(ctx, fi.ID)
	if err != nil {
		t.Fatalf("GetFinding after first (nil) sweep: %v", err)
	}
	if !got1.SweptAt.IsZero() {
		t.Error("swept_at should be zero (deferred) after nil-client sweep")
	}
	unswept1, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("UnsweptOpenFindings after nil sweep: %v", err)
	}
	found := false
	for _, x := range unswept1 {
		if x.ID == fi.ID {
			found = true
		}
	}
	if !found {
		t.Error("deferred finding should still be in UnsweptOpenFindings")
	}

	// Drain 2: real scripted client adjudicates → swept_at set.
	realClient := newScriptedClient()
	realClient.fallback = `{"results":[{"id":"sd-defer1","severity":"low","rationale":"dead header member, no callers"}]}`
	fReal := makeImpactFunnel(t, st, repoDir, realClient)

	result2, err := fReal.SweepDrain(ctx)
	if err != nil {
		t.Fatalf("second SweepDrain (with real client): %v", err)
	}
	if result2.ScanRunID == "" {
		t.Error("second drain should have opened a scan run (finding was still unswept)")
	}
	if n := realClient.callCount(); n != 1 {
		t.Errorf("expected exactly 1 LLM call for deferred .hpp adjudication, got %d", n)
	}

	got2, err := st.GetFinding(ctx, fi.ID)
	if err != nil {
		t.Fatalf("GetFinding after second sweep: %v", err)
	}
	if got2.SweptAt.IsZero() {
		t.Error("swept_at should be non-zero after budgeted drain")
	}
	if got2.Severity != domain.SeverityLow {
		t.Errorf("severity = %s after adjudication, want low", got2.Severity)
	}

	// No longer in UnsweptOpenFindings.
	unswept2, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("UnsweptOpenFindings after second sweep: %v", err)
	}
	for _, x := range unswept2 {
		if x.ID == fi.ID {
			t.Error("adjudicated finding should be excluded from UnsweptOpenFindings")
		}
	}
}

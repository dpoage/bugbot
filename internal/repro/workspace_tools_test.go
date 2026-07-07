package repro

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// fakeMaterializingSandbox wraps *sandbox.Mock (for scripted Exec responses)
// and adds MaterializeWorkspace so unit tests can exercise the workspace
// tools' iteration-workspace lifecycle without a real container runtime.
// MaterializeWorkspace creates a REAL temp directory (so tests can assert it
// exists, and later that it was removed); Exec itself stays fully scripted
// via the embedded Mock.
type fakeMaterializingSandbox struct {
	*sandbox.Mock

	mu   sync.Mutex
	made []string
}

func newFakeMaterializingSandbox(def sandbox.MockResponse) *fakeMaterializingSandbox {
	return &fakeMaterializingSandbox{Mock: sandbox.NewMock(def)}
}

func (f *fakeMaterializingSandbox) MaterializeWorkspace(repoDir string) (string, error) {
	dir, err := os.MkdirTemp("", "bugbot-test-iterws-")
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	f.made = append(f.made, dir)
	f.mu.Unlock()
	return dir, nil
}

// materialized returns every directory MaterializeWorkspace has returned, in
// call order.
func (f *fakeMaterializingSandbox) materialized() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.made))
	copy(out, f.made)
	return out
}

var (
	_ sandbox.Sandbox       = (*fakeMaterializingSandbox)(nil)
	_ workspaceMaterializer = (*fakeMaterializingSandbox)(nil)
)

// newRunReproTool builds a RunReproTool wired to sb for tests, with no
// dependency-strategy extras and the given per-attempt budget.
func newRunReproTool(sb *fakeMaterializingSandbox, repoDir string, maxExec int, ws *iterationWorkspace) *RunReproTool {
	return NewRunReproTool(sb, repoDir, "", 30*time.Second, nil, nil, nil, sb.MaterializeWorkspace, ws, maxExec)
}

// newWriteTool builds a WriteReproFileTool sharing sb's materializer and ws.
func newWriteTool(sb *fakeMaterializingSandbox, repoDir string, ws *iterationWorkspace) *WriteReproFileTool {
	return NewWriteReproFileTool(repoDir, sb.MaterializeWorkspace, ws)
}

func mustCmdArgs(t *testing.T, cmd []string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(runReproArgs{Cmd: cmd})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return b
}

func mustWriteArgs(t *testing.T, path, contents string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(writeReproFileArgs{Path: path, Contents: contents})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return b
}

// writeFileVia runs the write tool and fails the test on error.
func writeFileVia(t *testing.T, tool *WriteReproFileTool, path, contents string) string {
	t.Helper()
	out, err := tool.Run(context.Background(), mustWriteArgs(t, path, contents))
	if err != nil {
		t.Fatalf("write_repro_file(%s): %v", path, err)
	}
	return out
}

// TestRunReproTool_BudgetExhausted verifies that a call beyond maxExec
// returns a recoverable tool error naming the limit, without reaching the
// sandbox (CallCount stays at maxExec).
func TestRunReproTool_BudgetExhausted(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestX\nFAIL"}})
	repoDir := newRepoDir(t)
	tool := newRunReproTool(sb, repoDir, 2, &iterationWorkspace{})

	args := mustCmdArgs(t, []string{"cat", "x_test.go"})

	for i := 0; i < 2; i++ {
		if _, err := tool.Run(context.Background(), args); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}
	_, err := tool.Run(context.Background(), args)
	if err == nil {
		t.Fatal("3rd call should exceed the budget of 2")
	}
	if !strings.Contains(err.Error(), "run_repro budget exhausted") || !strings.Contains(err.Error(), "2/2") {
		t.Errorf("budget error = %q, want it to name the limit (2/2)", err.Error())
	}
	if sb.CallCount() != 2 {
		t.Errorf("sandbox CallCount = %d, want 2 (budget-exceeded call must not reach the sandbox)", sb.CallCount())
	}
}

// TestRunReproTool_InvalidCallsDoNotConsumeBudget verifies the fix for the
// observed budget-burn failure mode: malformed or invalid calls (bad JSON
// shape, empty cmd, bare shell operator) are rejected BEFORE the budget is
// charged, so a schema stumble never eats the agent's real iteration rounds.
func TestRunReproTool_InvalidCallsDoNotConsumeBudget(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestX\nFAIL"}})
	repoDir := newRepoDir(t)
	tool := newRunReproTool(sb, repoDir, 1, &iterationWorkspace{})

	invalid := []json.RawMessage{
		json.RawMessage(`{"cmd": "go test ./..."}`),           // string, not array
		json.RawMessage(`{"cmd": []}`),                        // empty argv
		json.RawMessage(`{}`),                                 // missing cmd
		mustCmdArgs(t, []string{"make", "&&", "make", "run"}), // bare shell op
	}
	for i, raw := range invalid {
		if _, err := tool.Run(context.Background(), raw); err == nil {
			t.Fatalf("invalid call %d: expected an error", i)
		}
	}
	if got := tool.ExecCount(); got != 0 {
		t.Fatalf("ExecCount after invalid calls = %d, want 0 (invalid calls must be free)", got)
	}

	// The single budget slot is still available for a real run.
	if _, err := tool.Run(context.Background(), mustCmdArgs(t, []string{"cat", "x"})); err != nil {
		t.Fatalf("valid call after invalid ones: %v", err)
	}
	if sb.CallCount() != 1 {
		t.Errorf("sandbox CallCount = %d, want 1", sb.CallCount())
	}
}

// TestRunReproTool_MalformedArgsTeachSchema verifies that a wrong-shaped cmd
// yields a message that teaches the expected JSON shape instead of leaking Go
// unmarshal internals as the only guidance.
func TestRunReproTool_MalformedArgsTeachSchema(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t)
	tool := newRunReproTool(sb, repoDir, 5, &iterationWorkspace{})

	_, err := tool.Run(context.Background(), json.RawMessage(`{"cmd": "go test ./..."}`))
	if err == nil {
		t.Fatal("expected an error for cmd-as-string")
	}
	if !strings.Contains(err.Error(), "ARRAY") || !strings.Contains(err.Error(), `"cmd"`) {
		t.Errorf("error = %q, want it to teach that cmd is a JSON array", err.Error())
	}
}

// TestWriteReproFile_WritesTracksAndOverwrites verifies the write/edit
// affordance: files land on disk in the (lazily materialized, reused)
// workspace, parent directories are created, a same-path rewrite replaces the
// contents, and the registry tracks every live file.
func TestWriteReproFile_WritesTracksAndOverwrites(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t)
	ws := &iterationWorkspace{}
	tool := newWriteTool(sb, repoDir, ws)

	out := writeFileVia(t, tool, "a_test.go", "v1")
	if !strings.Contains(out, "a_test.go") {
		t.Errorf("write response %q does not echo the path", out)
	}
	writeFileVia(t, tool, "sub/dir/b_test.go", "nested")
	writeFileVia(t, tool, "a_test.go", "v2")

	made := sb.materialized()
	if len(made) != 1 {
		t.Fatalf("MaterializeWorkspace called %d times, want 1 (materialize once, reuse thereafter)", len(made))
	}
	got, err := os.ReadFile(filepath.Join(made[0], "a_test.go"))
	if err != nil || string(got) != "v2" {
		t.Errorf("a_test.go on disk = %q, %v; want overwritten contents \"v2\"", got, err)
	}
	if _, err := os.Stat(filepath.Join(made[0], "sub", "dir", "b_test.go")); err != nil {
		t.Errorf("nested file missing: %v", err)
	}
	wantPaths := []string{"a_test.go", "sub/dir/b_test.go"}
	if paths := ws.trackedPaths(); !reflect.DeepEqual(paths, wantPaths) {
		t.Errorf("trackedPaths = %v, want %v", paths, wantPaths)
	}
}

// TestWriteReproFile_ValidationErrors verifies that path violations are
// rejected with the SAME rules validatePlan applies (so a written file is
// guaranteed to clear submission), with teaching messages, and WITHOUT
// materializing a workspace for a rejected write.
func TestWriteReproFile_ValidationErrors(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t) // contains go.mod
	tool := newWriteTool(sb, repoDir, &iterationWorkspace{})

	cases := []struct {
		name string
		args json.RawMessage
		want string
	}{
		{"path escape", mustWriteArgs(t, "../escape.go", "x"), "workspace-relative"},
		{"existing repo file", mustWriteArgs(t, "go.mod", "x"), "already exists"},
		{"missing path", json.RawMessage(`{"contents": "x"}`), `"path"`},
		{"malformed json", json.RawMessage(`{"path": ["a.go"]}`), "invalid arguments"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := tool.Run(context.Background(), c.args)
			if err == nil {
				t.Fatal("expected a validation error, got none")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), c.want)
			}
		})
	}
	if made := sb.materialized(); len(made) != 0 {
		t.Errorf("MaterializeWorkspace called %d times, want 0 (rejected writes must not materialize)", len(made))
	}
}

// TestDeleteReproFile verifies the escape hatch for a poisoned workspace: a
// previously written file is removed from disk AND from the submission
// registry, while repo files (never written this attempt) cannot be deleted.
func TestDeleteReproFile(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t)
	ws := &iterationWorkspace{}
	writeTool := newWriteTool(sb, repoDir, ws)
	delTool := NewDeleteReproFileTool(ws)

	writeFileVia(t, writeTool, "broken_test.go", "package bug\nsyntax error")
	writeFileVia(t, writeTool, "keep_test.go", "package bug\n")

	if _, err := delTool.Run(context.Background(), json.RawMessage(`{"path": "broken_test.go"}`)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if paths := ws.trackedPaths(); !reflect.DeepEqual(paths, []string{"keep_test.go"}) {
		t.Errorf("trackedPaths after delete = %v, want [keep_test.go]", paths)
	}
	if _, err := os.Stat(filepath.Join(sb.materialized()[0], "broken_test.go")); !os.IsNotExist(err) {
		t.Errorf("broken_test.go still on disk (stat err = %v), want removed", err)
	}

	_, err := delTool.Run(context.Background(), json.RawMessage(`{"path": "go.mod"}`))
	if err == nil {
		t.Fatal("deleting a repo file must fail")
	}
	if !strings.Contains(err.Error(), "not a file you wrote") || !strings.Contains(err.Error(), "keep_test.go") {
		t.Errorf("error = %q, want refusal naming the tracked files", err.Error())
	}
}

// TestRunRepro_RunsAgainstSharedWorkspace verifies that run_repro executes in
// the same directory write_repro_file populated (Spec.Workspace pins it, no
// WriteFiles needed — files are already on disk) and the workspace is
// materialized exactly once across both tools.
func TestRunRepro_RunsAgainstSharedWorkspace(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestX\nFAIL"}})
	repoDir := newRepoDir(t)
	ws := &iterationWorkspace{}
	writeTool := newWriteTool(sb, repoDir, ws)
	runTool := newRunReproTool(sb, repoDir, 5, ws)

	writeFileVia(t, writeTool, "x_test.go", "package bug\n")
	if _, err := runTool.Run(context.Background(), mustCmdArgs(t, []string{"cat", "x_test.go"})); err != nil {
		t.Fatalf("run_repro: %v", err)
	}
	if _, err := runTool.Run(context.Background(), mustCmdArgs(t, []string{"cat", "x_test.go"})); err != nil {
		t.Fatalf("run_repro (2nd): %v", err)
	}

	made := sb.materialized()
	if len(made) != 1 {
		t.Fatalf("MaterializeWorkspace called %d times, want 1 (shared across write and run tools)", len(made))
	}
	calls := sb.Calls()
	if len(calls) != 2 {
		t.Fatalf("sandbox calls = %d, want 2", len(calls))
	}
	for i, c := range calls {
		if c.Spec.Workspace != made[0] {
			t.Errorf("call %d Spec.Workspace = %q, want the materialized path %q", i, c.Spec.Workspace, made[0])
		}
		if len(c.Spec.WriteFiles) != 0 {
			t.Errorf("call %d Spec.WriteFiles = %v, want none (files are written at write time)", i, c.Spec.WriteFiles)
		}
		// RepoDir is still threaded through so a non-git fallback works, but
		// Workspace is what Exec actually uses (see sandbox.Spec.Workspace doc).
		if c.Spec.RepoDir != repoDir {
			t.Errorf("call %d Spec.RepoDir = %q, want %q", i, c.Spec.RepoDir, repoDir)
		}
	}
}

// TestIterationWorkspace_MergedFiles verifies the submission merge: the
// registry is the base, overlay entries win on collisions, and the result is
// a fresh map (neither input mutated).
func TestIterationWorkspace_MergedFiles(t *testing.T) {
	ws := &iterationWorkspace{}
	ws.record("a_test.go", "from workspace")
	ws.record("b_test.go", "keep me")

	overlay := map[string]string{"a_test.go": "overlay wins", "c_test.go": "new"}
	merged := ws.mergedFiles(overlay)

	want := map[string]string{
		"a_test.go": "overlay wins",
		"b_test.go": "keep me",
		"c_test.go": "new",
	}
	if !reflect.DeepEqual(merged, want) {
		t.Errorf("mergedFiles = %v, want %v", merged, want)
	}
	// Registry unchanged: a second merge with no overlay returns the originals.
	if again := ws.mergedFiles(nil); again["a_test.go"] != "from workspace" {
		t.Errorf("registry mutated by merge: a_test.go = %q", again["a_test.go"])
	}
}

// TestRunReproTool_RendersClassificationAndTail verifies that Run's textual
// result carries the exit code, the interpret()-style demonstrated/reason
// classification, and a tail excerpt of the combined output.
func TestRunReproTool_RendersClassificationAndTail(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\n    bug_test.go:1: boom\nFAIL",
	}})
	repoDir := newRepoDir(t)
	tool := newRunReproTool(sb, repoDir, 5, &iterationWorkspace{})

	out, err := tool.Run(context.Background(), mustCmdArgs(t, []string{"go", "test", "-timeout", "60s", "-run", "TestBug", "./..."}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "exit_code=1") {
		t.Errorf("output missing exit_code: %q", out)
	}
	if !strings.Contains(out, "demonstrated=true") {
		t.Errorf("output missing demonstrated=true classification: %q", out)
	}
	if !strings.Contains(out, "--- FAIL: TestBug") {
		t.Errorf("output missing tail excerpt of combined output: %q", out)
	}
}

// TestRunReproTool_NotDemonstratedReasonSurfaced verifies a non-demonstrating
// run (e.g. exit 0) reports demonstrated=false with its reason, so the agent
// gets the same signal validatePlan/interpret would give the final plan.
func TestRunReproTool_NotDemonstratedReasonSurfaced(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "PASS"}})
	repoDir := newRepoDir(t)
	tool := newRunReproTool(sb, repoDir, 5, &iterationWorkspace{})

	out, err := tool.Run(context.Background(), mustCmdArgs(t, []string{"go", "test", "-timeout", "60s", "-run", "TestBug", "./..."}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "demonstrated=false") {
		t.Errorf("output missing demonstrated=false: %q", out)
	}
	if !strings.Contains(out, "reason=exit_zero") {
		t.Errorf("output missing reason=exit_zero: %q", out)
	}
}

// TestAttempt_WorkspaceFilesSubmittedAsPlan is the core workspace-as-proof
// contract: an agent that builds its repro via write_repro_file and then
// submits a plan with cmd ONLY (no files field) still promotes — Attempt
// merges the workspace registry into the plan, validatePlan sees the files,
// and the official clean-room execute() re-applies them onto a fresh
// workspace via WriteFiles.
func TestAttempt_WorkspaceFilesSubmittedAsPlan(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)
	artifactDir := t.TempDir()

	const testFile = "package bug\n\nimport \"testing\"\n\nfunc TestBug(t *testing.T){ t.Fatal(\"boom\") }\n"
	client := newToolScriptedClient(
		toolCallStep("c1", "write_repro_file", string(mustWriteArgs(t, "bug_test.go", testFile))),
		toolCallStep("c2", "run_repro", string(mustCmdArgs(t, []string{"go", "test", "-timeout", "60s", "-run", "TestBug", "./..."}))),
		textStep(`{"cmd":["go","test","-timeout","60s","-run","TestBug","./..."],"expect":"assertion fails"}`),
	)

	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\nFAIL",
	}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: artifactDir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	att, err := r.Attempt(ctx, finding)
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if !att.Promoted {
		t.Fatalf("Attempt not promoted (reason=%q): %+v", att.Reason, att)
	}
	if got := att.Plan.Files["bug_test.go"]; got != testFile {
		t.Errorf("promoted plan is missing the workspace-written file; got %q", got)
	}

	// The official verdict is the LAST sandbox call: a clean-room run
	// (Workspace empty) that re-applies the tracked file via WriteFiles.
	calls := sb.Calls()
	final := calls[len(calls)-1].Spec
	if final.Workspace != "" {
		t.Errorf("official run Spec.Workspace = %q, want empty (fresh clean-room copy)", final.Workspace)
	}
	if got := string(final.WriteFiles["bug_test.go"]); got != testFile {
		t.Errorf("official run WriteFiles[bug_test.go] = %q, want the workspace-written contents", got)
	}
}

// TestAttempt_IterationWorkspaceRemovedAfterReturn verifies that an Attempt
// that uses the workspace tools leaves the iteration workspace materialized
// during the run but removes it (via the deferred cleanup) before Attempt
// returns — the official clean-room verdict must never be able to observe it
// afterward.
func TestAttempt_IterationWorkspaceRemovedAfterReturn(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)
	artifactDir := t.TempDir()

	// The scripted client writes its repro, runs it once, then submits the
	// final plan.
	client := newToolScriptedClient(
		toolCallStep("c1", "write_repro_file", string(mustWriteArgs(t, "bug_test.go", goodPlan().Files["bug_test.go"]))),
		toolCallStep("c2", "run_repro", string(mustCmdArgs(t, goodPlan().Cmd))),
		textStep(planBody(t, goodPlan())),
	)

	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\nFAIL",
	}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: artifactDir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	att, err := r.Attempt(ctx, finding)
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if !att.Promoted {
		t.Fatalf("Attempt not promoted: %+v", att)
	}

	made := sb.materialized()
	if len(made) != 1 {
		t.Fatalf("MaterializeWorkspace called %d times, want exactly 1", len(made))
	}
	if _, statErr := os.Stat(made[0]); !os.IsNotExist(statErr) {
		t.Errorf("iteration workspace %q still exists after Attempt returned (stat err = %v), want removed", made[0], statErr)
	}
}

// TestAttempt_NoWorkspaceToolCallLeavesWorkspaceUnmaterialized verifies that
// an Attempt whose agent never touches the workspace tools never materializes
// an iteration workspace at all (zero extra disk cost for the common
// single-shot case).
func TestAttempt_NoWorkspaceToolCallLeavesWorkspaceUnmaterialized(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)
	artifactDir := t.TempDir()

	client := newScriptedClient(planBody(t, goodPlan()))
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\nFAIL",
	}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: artifactDir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	att, err := r.Attempt(ctx, finding)
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if !att.Promoted {
		t.Fatalf("Attempt not promoted: %+v", att)
	}
	if made := sb.materialized(); len(made) != 0 {
		t.Errorf("MaterializeWorkspace called %d times, want 0 (no workspace tool was ever invoked)", len(made))
	}
}

package repro

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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

// newWorkspaceTool builds a WorkspaceTool wired to sb for tests, with no
// dependency-strategy extras and the given per-attempt budget.
func newWorkspaceTool(sb *fakeMaterializingSandbox, repoDir string, maxExec int, ws *iterationWorkspace) *WorkspaceTool {
	return NewWorkspaceTool(sb, repoDir, "", 30*time.Second, nil, nil, nil, nil, sb.MaterializeWorkspace, ws, maxExec)
}

// newWriteTool builds a WriteReproFileTool sharing sb's materializer and ws.
func newWriteTool(sb *fakeMaterializingSandbox, repoDir string, ws *iterationWorkspace) *WriteReproFileTool {
	return NewWriteReproFileTool(repoDir, sb.MaterializeWorkspace, ws)
}

func mustCmdArgs(t *testing.T, cmd []string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(workspaceArgs{Argv: append([]string{"exec"}, cmd...)})
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

// TestWorkspaceTool_BudgetExhausted verifies that a call beyond maxExec
// returns a recoverable tool error naming the limit, without reaching the
// sandbox (CallCount stays at maxExec).
func TestWorkspaceTool_BudgetExhausted(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestX\nFAIL"}})
	repoDir := newRepoDir(t)
	tool := newWorkspaceTool(sb, repoDir, 2, &iterationWorkspace{})

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
	if !strings.Contains(err.Error(), "workspace exec budget exhausted") || !strings.Contains(err.Error(), "2/2") {
		t.Errorf("budget error = %q, want it to name the limit (2/2)", err.Error())
	}
	if sb.CallCount() != 2 {
		t.Errorf("sandbox CallCount = %d, want 2 (budget-exceeded call must not reach the sandbox)", sb.CallCount())
	}
}

// TestWorkspaceTool_InvalidCallsDoNotConsumeBudget verifies the fix for the
// observed budget-burn failure mode: malformed args, unknown applets, and
// invalid exec commands (bad JSON shape, empty argv, unknown applet, bare
// shell operator) are rejected BEFORE the budget is charged, so a schema
// stumble never eats the agent's real iteration rounds.
func TestWorkspaceTool_InvalidCallsDoNotConsumeBudget(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestX\nFAIL"}})
	repoDir := newRepoDir(t)
	tool := newWorkspaceTool(sb, repoDir, 1, &iterationWorkspace{})

	invalid := []json.RawMessage{
		json.RawMessage(`{"argv": "exec go test ./..."}`),     // string, not array
		json.RawMessage(`{"argv": []}`),                       // empty argv
		json.RawMessage(`{}`),                                 // missing argv
		json.RawMessage(`{"argv": ["frobnicate"]}`),           // unknown applet
		json.RawMessage(`{"argv": ["exec"]}`),                 // exec with no command
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

// TestWorkspaceTool_MalformedArgsTeachSchema verifies that a wrong-shaped argv
// yields a message that teaches the expected JSON shape instead of leaking Go
// unmarshal internals as the only guidance.
func TestWorkspaceTool_MalformedArgsTeachSchema(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t)
	tool := newWorkspaceTool(sb, repoDir, 5, &iterationWorkspace{})

	_, err := tool.Run(context.Background(), json.RawMessage(`{"argv": "exec go test ./..."}`))
	if err == nil {
		t.Fatal("expected an error for argv-as-string")
	}
	if !strings.Contains(err.Error(), "ARRAY") || !strings.Contains(err.Error(), `"argv"`) {
		t.Errorf("error = %q, want it to teach that argv is a JSON array", err.Error())
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

// TestWorkspaceExec_RunsAgainstSharedWorkspace verifies that `workspace exec` executes in
// the same directory write_repro_file populated (Spec.Workspace pins it, no
// WriteFiles needed — files are already on disk) and the workspace is
// materialized exactly once across both tools.
func TestWorkspaceExec_RunsAgainstSharedWorkspace(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestX\nFAIL"}})
	repoDir := newRepoDir(t)
	ws := &iterationWorkspace{}
	writeTool := newWriteTool(sb, repoDir, ws)
	runTool := newWorkspaceTool(sb, repoDir, 5, ws)

	writeFileVia(t, writeTool, "x_test.go", "package bug\n")
	if _, err := runTool.Run(context.Background(), mustCmdArgs(t, []string{"cat", "x_test.go"})); err != nil {
		t.Fatalf("workspace exec: %v", err)
	}
	if _, err := runTool.Run(context.Background(), mustCmdArgs(t, []string{"cat", "x_test.go"})); err != nil {
		t.Fatalf("workspace exec (2nd): %v", err)
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

// TestWorkspaceTool_RendersClassificationAndTail verifies that Run's textual
// result carries the exit code, the interpret()-style demonstrated/reason
// classification, and a tail excerpt of the combined output.
func TestWorkspaceTool_RendersClassificationAndTail(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\n    bug_test.go:1: boom\nFAIL",
	}})
	repoDir := newRepoDir(t)
	tool := newWorkspaceTool(sb, repoDir, 5, &iterationWorkspace{})

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

// TestWorkspaceTool_NotDemonstratedReasonSurfaced verifies a non-demonstrating
// run (e.g. exit 0) reports demonstrated=false with its reason, so the agent
// gets the same signal validatePlan/interpret would give the final plan.
func TestWorkspaceTool_NotDemonstratedReasonSurfaced(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "PASS"}})
	repoDir := newRepoDir(t)
	tool := newWorkspaceTool(sb, repoDir, 5, &iterationWorkspace{})

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
		toolCallStep("c2", "workspace", string(mustCmdArgs(t, []string{"go", "test", "-timeout", "60s", "-run", "TestBug", "./..."}))),
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
		toolCallStep("c2", "workspace", string(mustCmdArgs(t, goodPlan().Cmd))),
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

// runApplet is a small helper to invoke a WorkspaceTool applet by argv.
func runApplet(t *testing.T, tool *WorkspaceTool, argv ...string) (string, error) {
	t.Helper()
	raw, err := json.Marshal(workspaceArgs{Argv: argv})
	if err != nil {
		t.Fatalf("marshal argv: %v", err)
	}
	return tool.Run(context.Background(), raw)
}

// TestWorkspaceTool_FreeAppletsOnUnmaterializedWorkspace verifies that
// ls/cat/grep/find, called before any write_repro_file/exec, return the
// pristine-repo hint WITHOUT materializing a workspace — materializing on
// a free call would silently copy the whole repo.
func TestWorkspaceTool_FreeAppletsOnUnmaterializedWorkspace(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t)
	ws := &iterationWorkspace{}
	tool := newWorkspaceTool(sb, repoDir, 5, ws)

	for _, argv := range [][]string{{"ls"}, {"cat", "x.go"}, {"grep", "foo"}, {"find", "x.go"}} {
		out, err := runApplet(t, tool, argv...)
		if err != nil {
			t.Fatalf("%v: unexpected error: %v", argv, err)
		}
		if !strings.Contains(out, "not yet materialized") {
			t.Errorf("%v output = %q, want the unmaterialized hint", argv, out)
		}
	}
	if made := sb.materialized(); len(made) != 0 {
		t.Errorf("MaterializeWorkspace called %d times, want 0 (free applets must not materialize)", len(made))
	}
	if got := tool.ExecCount(); got != 0 {
		t.Errorf("ExecCount = %d, want 0 (free applets never charge the budget)", got)
	}
}

// TestWorkspaceTool_LsAndCat verifies the ls/cat applets against a
// materialized workspace: ls lists entries with a trailing slash on
// directories, and cat returns file contents (tail-capped, with a
// truncation marker when clipped). Neither consumes the exec budget.
func TestWorkspaceTool_LsAndCat(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t)
	ws := &iterationWorkspace{}
	writeTool := newWriteTool(sb, repoDir, ws)
	tool := newWorkspaceTool(sb, repoDir, 5, ws)

	writeFileVia(t, writeTool, "sub/nested_test.go", "package bug\n")

	out, err := runApplet(t, tool, "ls", "sub")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(out, "nested_test.go") {
		t.Errorf("ls output = %q, want it to list nested_test.go", out)
	}

	out, err = runApplet(t, tool, "cat", "sub/nested_test.go")
	if err != nil {
		t.Fatalf("cat: %v", err)
	}
	if !strings.Contains(out, "package bug") {
		t.Errorf("cat output = %q, want the file contents", out)
	}

	// A big file must be tail-capped with a truncation marker.
	big := strings.Repeat("x", runReproOutputTailBytes+1024)
	writeFileVia(t, writeTool, "big.txt", big)
	out, err = runApplet(t, tool, "cat", "big.txt")
	if err != nil {
		t.Fatalf("cat big: %v", err)
	}
	if len(out) >= len(big) {
		t.Errorf("cat big.txt returned %d bytes, want it capped below the %d-byte input", len(out), len(big))
	}
	if !strings.Contains(out, "[head elided]") {
		t.Errorf("cat big.txt output missing truncation marker: %q", out[:80])
	}

	if got := tool.ExecCount(); got != 0 {
		t.Errorf("ExecCount = %d, want 0 (ls/cat never charge the budget)", got)
	}
}

// TestWorkspaceTool_LsCatConfinement verifies ls/cat reject a lexical
// escape ("..") and a symlink planted inside the workspace that points
// outside it — the same containment agent.FSRoot already guarantees for the
// host-repo tools.
func TestWorkspaceTool_LsCatConfinement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t)
	ws := &iterationWorkspace{}
	writeTool := newWriteTool(sb, repoDir, ws)
	tool := newWorkspaceTool(sb, repoDir, 5, ws)
	writeFileVia(t, writeTool, "keep_test.go", "package bug\n")

	wsPath := sb.materialized()[0]
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(wsPath, "escape")); err != nil {
		t.Fatal(err)
	}

	if _, err := runApplet(t, tool, "cat", "../escape.go"); err == nil {
		t.Error("cat with \"..\" traversal should fail")
	}
	if _, err := runApplet(t, tool, "cat", "escape"); err == nil {
		t.Error("cat through a symlink escaping the workspace should fail")
	}
	if _, err := runApplet(t, tool, "ls", ".."); err == nil {
		t.Error("ls with \"..\" traversal should fail")
	}
}

// TestWorkspaceTool_Status verifies the status applet reports materialization,
// tracked files, and the exec budget used/max — the direct fix for the
// observed keep-calling-after-exhaustion failure mode.
func TestWorkspaceTool_Status(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0}})
	repoDir := newRepoDir(t)
	ws := &iterationWorkspace{}
	tool := newWorkspaceTool(sb, repoDir, 3, ws)

	out, err := runApplet(t, tool, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "materialized: false") {
		t.Errorf("status before any write/exec = %q, want materialized: false", out)
	}
	if !strings.Contains(out, "0/3 used") {
		t.Errorf("status output = %q, want exec budget 0/3", out)
	}

	writeTool := newWriteTool(sb, repoDir, ws)
	writeFileVia(t, writeTool, "a_test.go", "package bug\n")
	if _, err := runApplet(t, tool, "exec", "cat", "a_test.go"); err != nil {
		t.Fatalf("exec: %v", err)
	}

	out, err = runApplet(t, tool, "status")
	if err != nil {
		t.Fatalf("status (2nd): %v", err)
	}
	if !strings.Contains(out, "materialized: true") {
		t.Errorf("status after write = %q, want materialized: true", out)
	}
	if !strings.Contains(out, "a_test.go") {
		t.Errorf("status output = %q, want it to list the tracked file", out)
	}
	if !strings.Contains(out, "1/3 used") {
		t.Errorf("status output = %q, want exec budget 1/3 after one exec", out)
	}
}

// TestWorkspaceTool_UnknownAppletIsFreeAndEnumerates verifies that an
// unrecognized applet is rejected for free and the error names the valid
// applets, so a schema stumble teaches the fix instead of burning budget.
func TestWorkspaceTool_UnknownAppletIsFreeAndEnumerates(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t)
	tool := newWorkspaceTool(sb, repoDir, 5, &iterationWorkspace{})

	_, err := runApplet(t, tool, "rm", "foo")
	if err == nil {
		t.Fatal("expected an error for an unknown applet")
	}
	for _, applet := range []string{"ls", "cat", "status", "grep", "find", "exec"} {
		if !strings.Contains(err.Error(), applet) {
			t.Errorf("unknown-applet error = %q, want it to name applet %q", err.Error(), applet)
		}
	}
	if got := tool.ExecCount(); got != 0 {
		t.Errorf("ExecCount = %d, want 0 (unknown applet must be free)", got)
	}
}

// TestWorkspaceTool_Grep verifies that `grep` finds content in a
// materialized workspace file, formats matches as 'path:line:text', and
// never charges the exec budget.
func TestWorkspaceTool_Grep(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t)
	ws := &iterationWorkspace{}
	writeTool := newWriteTool(sb, repoDir, ws)
	tool := newWorkspaceTool(sb, repoDir, 5, ws)

	writeFileVia(t, writeTool, "sub/nested_test.go", "package bug\n\nfunc TestNeedle(t *testing.T) {}\n")
	writeFileVia(t, writeTool, "other_test.go", "package bug\n\nfunc TestOther(t *testing.T) {}\n")

	out, err := runApplet(t, tool, "grep", "TestNeedle")
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(out, "sub/nested_test.go:3:func TestNeedle") {
		t.Errorf("grep output = %q, want a sub/nested_test.go:3:... match", out)
	}
	if strings.Contains(out, "other_test.go") {
		t.Errorf("grep output = %q, must not match other_test.go", out)
	}

	// A dir argument scopes the search.
	out, err = runApplet(t, tool, "grep", "func Test", "sub")
	if err != nil {
		t.Fatalf("grep sub: %v", err)
	}
	if !strings.Contains(out, "nested_test.go") || strings.Contains(out, "other_test.go") {
		t.Errorf("grep sub output = %q, want only sub/nested_test.go", out)
	}

	// No matches is reported explicitly, not as an empty string.
	out, err = runApplet(t, tool, "grep", "NoSuchSymbolAnywhere")
	if err != nil {
		t.Fatalf("grep no match: %v", err)
	}
	if !strings.Contains(out, "no matches") {
		t.Errorf("grep no-match output = %q, want it to say so explicitly", out)
	}

	// An invalid regexp is rejected without touching the sandbox.
	if _, err := runApplet(t, tool, "grep", "("); err == nil {
		t.Error("grep with an invalid regexp should fail")
	}

	if got := tool.ExecCount(); got != 0 {
		t.Errorf("ExecCount = %d, want 0 (grep never charges the budget)", got)
	}
}

// TestWorkspaceTool_Find verifies that `find` matches filenames by glob and
// by substring under a materialized workspace, and never charges the exec
// budget.
func TestWorkspaceTool_Find(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t)
	ws := &iterationWorkspace{}
	writeTool := newWriteTool(sb, repoDir, ws)
	tool := newWorkspaceTool(sb, repoDir, 5, ws)

	writeFileVia(t, writeTool, "sub/nested_test.go", "package bug\n")
	writeFileVia(t, writeTool, "sub/helper.go", "package bug\n")
	writeFileVia(t, writeTool, "other.txt", "not go\n")

	out, err := runApplet(t, tool, "find", "*_test.go")
	if err != nil {
		t.Fatalf("find glob: %v", err)
	}
	if !strings.Contains(out, "sub/nested_test.go") {
		t.Errorf("find glob output = %q, want sub/nested_test.go", out)
	}
	if strings.Contains(out, "helper.go") || strings.Contains(out, "other.txt") {
		t.Errorf("find glob output = %q, want only *_test.go matches", out)
	}

	out, err = runApplet(t, tool, "find", "helper")
	if err != nil {
		t.Fatalf("find substring: %v", err)
	}
	if !strings.Contains(out, "sub/helper.go") {
		t.Errorf("find substring output = %q, want sub/helper.go", out)
	}

	out, err = runApplet(t, tool, "find", "nothing-matches-this")
	if err != nil {
		t.Fatalf("find no match: %v", err)
	}
	if !strings.Contains(out, "no matches") {
		t.Errorf("find no-match output = %q, want it to say so explicitly", out)
	}

	if got := tool.ExecCount(); got != 0 {
		t.Errorf("ExecCount = %d, want 0 (find never charges the budget)", got)
	}
}

// TestWorkspaceTool_GrepFindConfinement verifies grep/find reject a lexical
// "../" escape and an absolute path outside the workspace, matching the
// containment ls/cat already guarantee via agent.FSRoot.
func TestWorkspaceTool_GrepFindConfinement(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t)
	ws := &iterationWorkspace{}
	writeTool := newWriteTool(sb, repoDir, ws)
	tool := newWorkspaceTool(sb, repoDir, 5, ws)
	writeFileVia(t, writeTool, "keep_test.go", "package bug\n")

	outside := t.TempDir()

	if _, err := runApplet(t, tool, "grep", "package", "../escape"); err == nil {
		t.Error("grep with \"..\" traversal should fail")
	}
	if _, err := runApplet(t, tool, "grep", "package", outside); err == nil {
		t.Error("grep with an absolute path outside the workspace should fail")
	}
	if _, err := runApplet(t, tool, "find", "keep", "../escape"); err == nil {
		t.Error("find with \"..\" traversal should fail")
	}
	if _, err := runApplet(t, tool, "find", "keep", outside); err == nil {
		t.Error("find with an absolute path outside the workspace should fail")
	}
}

// TestWorkspaceTool_GrepFindSymlinkEscape verifies grep/find, which walk
// the whole workspace tree (unlike ls/cat's single-path lookup), never
// follow a symlink planted inside the workspace out to the host: neither
// applet reads through a symlinked file it encounters during the walk, nor
// descends into a symlinked directory — mirroring
// TestWorkspaceTool_LsCatConfinement's symlink fixture but exercised
// against the default whole-tree scan instead of a direct path argument.
func TestWorkspaceTool_GrepFindSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t)
	ws := &iterationWorkspace{}
	writeTool := newWriteTool(sb, repoDir, ws)
	tool := newWorkspaceTool(sb, repoDir, 5, ws)
	writeFileVia(t, writeTool, "keep_test.go", "package bug\n")

	wsPath := sb.materialized()[0]
	outside := t.TempDir()

	// A symlinked FILE inside the workspace pointing at an outside secret.
	secretFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("needle-in-secret-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretFile, filepath.Join(wsPath, "escape_file.txt")); err != nil {
		t.Fatal(err)
	}

	// A symlinked DIRECTORY inside the workspace pointing at an outside
	// directory containing its own matchable file.
	outsideDir := filepath.Join(outside, "vault")
	if err := os.Mkdir(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	leaked := filepath.Join(outsideDir, "leaked_test.go")
	if err := os.WriteFile(leaked, []byte("needle-in-leaked-dir\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(wsPath, "escape_dir")); err != nil {
		t.Fatal(err)
	}

	grepOut, err := runApplet(t, tool, "grep", "needle-in-")
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if strings.Contains(grepOut, "needle-in-secret-file") || strings.Contains(grepOut, "escape_file.txt") {
		t.Errorf("grep output = %q, must not read through the symlinked file", grepOut)
	}
	if strings.Contains(grepOut, "needle-in-leaked-dir") || strings.Contains(grepOut, "leaked_test.go") {
		t.Errorf("grep output = %q, must not descend the symlinked directory", grepOut)
	}

	findLeaked, err := runApplet(t, tool, "find", "leaked")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if strings.Contains(findLeaked, "leaked_test.go") {
		t.Errorf("find output = %q, must not descend the symlinked directory", findLeaked)
	}

	// The symlink entries themselves must not surface as matches either —
	// the walk skips symlinked files/dirs outright rather than resolving
	// and matching their target.
	findEscape, err := runApplet(t, tool, "find", "escape")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if strings.Contains(findEscape, "escape_file") || strings.Contains(findEscape, "escape_dir") {
		t.Errorf("find output = %q, must not list symlinked entries", findEscape)
	}
}

// TestWorkspaceTool_GrepOutputCaps verifies the grep applet enforces both
// its match-count and output-byte caps: a file with far more than
// workspaceGrepMaxMatches matching lines is truncated and flagged, and the
// output never exceeds workspaceGrepMaxOutputBytes by more than one
// rendered line.
func TestWorkspaceTool_GrepOutputCaps(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t)
	ws := &iterationWorkspace{}
	writeTool := newWriteTool(sb, repoDir, ws)
	tool := newWorkspaceTool(sb, repoDir, 5, ws)

	// One matching line per row, far more rows than workspaceGrepMaxMatches,
	// so the match-count cap (not EOF) ends the scan.
	var contents strings.Builder
	for i := 0; i < workspaceGrepMaxMatches*3; i++ {
		contents.WriteString("needle line\n")
	}
	writeFileVia(t, writeTool, "haystack.txt", contents.String())

	out, err := runApplet(t, tool, "grep", "needle")
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if got := strings.Count(out, "haystack.txt:"); got != workspaceGrepMaxMatches {
		t.Errorf("grep returned %d matches, want the cap of %d", got, workspaceGrepMaxMatches)
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("grep output = %q, want a truncation marker", out[:min(len(out), 200)])
	}
	if len(out) > workspaceGrepMaxOutputBytes+256 {
		t.Errorf("grep output is %d bytes, want it bounded near the %d-byte cap", len(out), workspaceGrepMaxOutputBytes)
	}
}

// TestWorkspaceTool_FindOutputCap verifies the find applet enforces its
// match-count cap: a directory with far more than workspaceFindMaxMatches
// matching filenames is truncated and flagged.
func TestWorkspaceTool_FindOutputCap(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t)
	ws := &iterationWorkspace{}
	writeTool := newWriteTool(sb, repoDir, ws)
	tool := newWorkspaceTool(sb, repoDir, 5, ws)

	for i := 0; i < workspaceFindMaxMatches+50; i++ {
		writeFileVia(t, writeTool, fmt.Sprintf("gen/needle_%03d.txt", i), "x")
	}

	out, err := runApplet(t, tool, "find", "needle_")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	matches := strings.Count(out, "gen/needle_")
	if matches != workspaceFindMaxMatches {
		t.Errorf("find returned %d matches, want the cap of %d", matches, workspaceFindMaxMatches)
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("find output = %q, want a truncation marker", out[:min(len(out), 200)])
	}
}

// TestWorkspaceExec_StructuredOutputParity is the core bugbot-0zay
// regression test: it scripts a mock sandbox whose response depends on
// whether the incoming Spec.Cmd carries the -json rewrite
// normalizeCmdForStructuredOutput applies, so the unnormalized (marker-only)
// and normalized (structured go test -json) branches DISAGREE on
// demonstrated — the marker-only branch reports plain, marker-less error
// text (not_demonstrated) while the -json branch reports a dispositive
// per-test failure event (demonstrated). Before the fix, runExec sent cmd
// unnormalized and would have landed on the marker-only branch and
// misclassified a genuine demonstration as not_demonstrated. This asserts
// `workspace exec`'s preview classification matches what execute()'s
// official buildSpec path would classify for the identical cmd.
func TestWorkspaceExec_StructuredOutputParity(t *testing.T) {
	const jsonFailStdout = `{"Action":"run","Test":"TestBug"}
{"Action":"fail","Test":"TestBug"}
{"Action":"fail","Package":"pkg"}
`
	const plainStdout = "unexpected error: something went wrong\n"

	responseFunc := func(_ int, spec sandbox.Spec) (sandbox.Result, error) {
		if hasCmdFlag(spec.Cmd, "-json") {
			return sandbox.Result{ExitCode: 1, Stdout: jsonFailStdout}, nil
		}
		return sandbox.Result{ExitCode: 1, Stdout: plainStdout}, nil
	}

	cmd := []string{"go", "test", "-timeout", "60s", "-run", "TestBug", "./..."}

	// Sanity check: prove the two branches actually disagree on their OWN
	// terms — the marker-only branch must NOT demonstrate, so this test
	// genuinely exercises the structured-path parity fix rather than two
	// branches that happen to agree anyway.
	rawVerdict := interpret(sandbox.Result{ExitCode: 1, Stdout: plainStdout}, cmd)
	if rawVerdict.demonstrated {
		t.Fatalf("test setup: the marker-only branch must not demonstrate (got demonstrated=true), " +
			"or this test cannot exercise the marker-vs-structured divergence it is meant to catch")
	}

	// Official path: buildSpec is execute()'s own spec-assembly helper,
	// exercised directly against the same scripted mock and cmd.
	sbOfficial := sandbox.NewMock(sandbox.MockResponse{})
	sbOfficial.ResponseFunc = responseFunc
	plan := &Plan{Cmd: cmd, Files: map[string]string{"bug_test.go": "package bug"}}
	officialSpec := buildSpec(t.TempDir(), plan, "", "", 30*time.Second, sandbox.Resolution{})
	officialRes, err := sbOfficial.Exec(context.Background(), officialSpec)
	if err != nil {
		t.Fatalf("official exec: %v", err)
	}
	officialVerdict := interpret(officialRes, cmd)
	if !officialVerdict.demonstrated {
		t.Fatalf("test setup: official path did not demonstrate; check the -json branch's fixture")
	}

	// Preview path: `workspace exec` against an identically scripted mock
	// and the SAME cmd.
	sbPreview := newFakeMaterializingSandbox(sandbox.MockResponse{})
	sbPreview.ResponseFunc = responseFunc
	repoDir := newRepoDir(t)
	tool := newWorkspaceTool(sbPreview, repoDir, 5, &iterationWorkspace{})

	out, err := tool.Run(context.Background(), mustCmdArgs(t, cmd))
	if err != nil {
		t.Fatalf("workspace exec: %v", err)
	}
	if !strings.Contains(out, "demonstrated=true") {
		t.Errorf("preview output = %q, want demonstrated=true to match the official verdict (demonstrated=%t)",
			out, officialVerdict.demonstrated)
	}

	// The mock must actually have seen a normalized (-json) cmd for the
	// preview call — otherwise the assertion above could pass for the wrong
	// reason (e.g. a marker cascade fluke).
	calls := sbPreview.Calls()
	if len(calls) != 1 {
		t.Fatalf("sandbox calls = %d, want 1", len(calls))
	}
	if !hasCmdFlag(calls[0].Spec.Cmd, "-json") {
		t.Errorf("preview Spec.Cmd = %v, want the -json rewrite applied (parity with buildSpec)", calls[0].Spec.Cmd)
	}
	if len(calls[0].Spec.CaptureFiles) != 0 {
		t.Errorf("preview Spec.CaptureFiles = %v, want none for a go test cmd (CaptureFiles is the pytest junit case)", calls[0].Spec.CaptureFiles)
	}
}

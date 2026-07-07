package repro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// runReproOutputTailBytes bounds the combined-output excerpt returned by the
// workspace tool's cat/exec applets. It is deliberately more generous than
// the 16 KiB cap on sandbox_exec/run_tests results (tools_sandbox_exec.go's
// sandboxOutputCap): the workspace tool is the agent's primary diagnostic
// loop for its OWN candidate, so truncating too aggressively would hide the
// exact compiler error or assertion failure it needs to see to fix the next
// iteration.
const runReproOutputTailBytes = 4 * 1024

// workspaceMaterializer is implemented by sandbox backends that can
// pre-materialize a caller-owned workspace outside of Exec (see
// sandbox.Spec.Workspace). *sandbox.CLI implements it via the pristine-
// workspace cache. The workspace tool set (write_repro_file,
// delete_repro_file, workspace) is wired only when the configured sandbox
// implements this interface (checked once in newRunner); a backend that
// doesn't (e.g. a bare sandbox.Mock in a test that never scripts iteration)
// simply omits the tools, mirroring how run_tests is omitted when no build
// system is detectable.
type workspaceMaterializer interface {
	MaterializeWorkspace(repoDir string) (string, error)
}

// iterationWorkspace is the lazily-materialized, per-Attempt workspace the
// reproducer agent builds its candidate in. It starts empty (path == "") and
// is materialized on the first tool call that needs it (write_repro_file or
// `workspace exec`) within one Attempt; every later call in that same
// Attempt reuses the same directory, so files written by one call remain
// visible (and can be overwritten) by the next — the interactive
// write/run/observe/fix loop the design is built around.
//
// Beyond the directory itself, the holder tracks every repro file the agent
// wrote (path → contents). That registry IS the submission: Attempt merges it
// into the final plan's Files before validation, so the workspace the agent
// iterated in is what gets re-run for the official verdict — no blind
// retranscription of file contents into the plan JSON.
//
// Attempt owns the holder's lifecycle: it constructs one per finding and
// defers cleanup, so the directory (and anything left in it) is gone before
// Attempt returns — the official clean-room verdict in execute() always runs
// against a completely fresh workspace and never observes iteration leftovers
// beyond the tracked file contents it re-applies.
type iterationWorkspace struct {
	mu    sync.Mutex
	path  string
	files map[string]string
}

// ensure returns the iteration workspace's path, materializing it via
// materialize on the first call and reusing the cached path thereafter.
func (w *iterationWorkspace) ensure(repoDir string, materialize func(string) (string, error)) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.path != "" {
		return w.path, nil
	}
	ws, err := materialize(repoDir)
	if err != nil {
		return "", err
	}
	w.path = ws
	return ws, nil
}

// record tracks contents as the current version of the repro file at path.
func (w *iterationWorkspace) record(path, contents string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.files == nil {
		w.files = make(map[string]string)
	}
	w.files[path] = contents
}

// forget drops path from the registry, reporting whether it was tracked.
func (w *iterationWorkspace) forget(path string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.files[path]
	delete(w.files, path)
	return ok
}

// trackedPaths returns the sorted paths of every repro file currently
// tracked, for echoing workspace state back to the agent.
func (w *iterationWorkspace) trackedPaths() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	paths := make([]string, 0, len(w.files))
	for p := range w.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// materializedPath returns the iteration workspace's path and whether it has
// been materialized yet, WITHOUT materializing it. Used by the workspace
// tool's free ls/cat/status applets so an inspection call never triggers a
// full repo copy.
func (w *iterationWorkspace) materializedPath() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.path, w.path != ""
}

// mergedFiles returns the tracked registry with overlay applied on top
// (overlay entries win on path collisions). The result is always a fresh map;
// neither input is mutated. Attempt calls this to fold the workspace into the
// submitted plan: the workspace is the proof, and the plan's own files field
// is only an optional overlay.
func (w *iterationWorkspace) mergedFiles(overlay map[string]string) map[string]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	merged := make(map[string]string, len(w.files)+len(overlay))
	for p, c := range w.files {
		merged[p] = c
	}
	for p, c := range overlay {
		merged[p] = c
	}
	return merged
}

// cleanup removes the iteration workspace, if one was ever materialized, and
// resets the holder so a stale path or registry can never be reused. Safe to
// call multiple times (e.g. an Attempt that returns before any workspace tool
// call).
func (w *iterationWorkspace) cleanup() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.files = nil
	if w.path == "" {
		return nil
	}
	err := os.RemoveAll(w.path)
	w.path = ""
	return err
}

// WriteReproFileTool exposes the workspace's write/edit affordance:
// write_repro_file writes ONE NEW file into the per-Attempt iteration
// workspace and tracks it in the registry that Attempt later submits as the
// plan's files. Writing is free — it is not gated by the workspace exec
// budget —
// so the agent can edit (overwrite a file it wrote earlier) as many times as
// it needs between runs.
type WriteReproFileTool struct {
	repoDir     string
	materialize func(repoDir string) (string, error)
	ws          *iterationWorkspace
}

// NewWriteReproFileTool builds a write_repro_file tool bound to one Attempt's
// iteration workspace holder. repoDir is the host repo path used for the
// must-not-overwrite-existing-repo-file check; materialize lazily creates the
// workspace (normally sb.(workspaceMaterializer).MaterializeWorkspace).
func NewWriteReproFileTool(repoDir string, materialize func(repoDir string) (string, error), ws *iterationWorkspace) *WriteReproFileTool {
	return &WriteReproFileTool{repoDir: repoDir, materialize: materialize, ws: ws}
}

// writeReproFileArgs is the JSON schema for write_repro_file's arguments.
type writeReproFileArgs struct {
	// Path is the repo-root-relative destination of the file.
	Path string `json:"path"`
	// Contents is the full file contents to write.
	Contents string `json:"contents"`
}

// Def implements agent.Tool.
func (t *WriteReproFileTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "write_repro_file",
		Description: "Write ONE NEW repro/test file into your persistent attempt workspace. Calling it " +
			"again with the same path replaces the file — that is how you edit. Writing is free (it does " +
			"not consume the workspace exec budget). Every file you write (and do not later delete) is " +
			"automatically included in your final submitted plan: the workspace is the proof. You cannot " +
			"overwrite a file that already exists in the repository — write NEW files only.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Destination path relative to the repo root (e.g. \"pkg/repro_test.go\"). Must NOT be an existing repository file, must not be absolute, and must not escape the workspace with \"..\"."
    },
    "contents": {
      "type": "string",
      "description": "The FULL contents of the file."
    }
  },
  "required": ["path", "contents"],
  "additionalProperties": false
}`),
	}
}

// Run implements agent.Tool. It validates path with the SAME rule validatePlan
// applies to plan file keys (workspace-relative, must not shadow an existing
// repo file) so a file that clears this gate is guaranteed to clear final
// submission, lazily materializes the attempt's workspace on first use, writes
// the file, and records it in the registry Attempt submits with the plan.
func (t *WriteReproFileTool) Run(_ context.Context, raw json.RawMessage) (string, error) {
	var args writeReproFileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf(`invalid arguments (%v): write_repro_file takes {"path": "<repo-relative path>", "contents": "<full file contents>"} — both strings`, err)
	}
	if args.Path == "" {
		return "", errors.New(`missing "path": give the file's destination relative to the repo root, e.g. "pkg/repro_test.go"`)
	}
	if err := validateReproFilePath(args.Path, t.repoDir); err != nil {
		return "", err
	}
	ws, err := t.ws.ensure(t.repoDir, t.materialize)
	if err != nil {
		return "", fmt.Errorf("write_repro_file: materialize iteration workspace: %w", err)
	}
	dst := filepath.Join(ws, filepath.FromSlash(args.Path))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("write_repro_file: create parent directory: %w", err)
	}
	if err := os.WriteFile(dst, []byte(args.Contents), 0o644); err != nil {
		return "", fmt.Errorf("write_repro_file: write %s: %w", args.Path, err)
	}
	t.ws.record(args.Path, args.Contents)
	return fmt.Sprintf("wrote %s (%d bytes)\nWorkspace repro files (submitted with your final plan): %s",
		args.Path, len(args.Contents), strings.Join(t.ws.trackedPaths(), ", ")), nil
}

// DeleteReproFileTool removes a file the agent previously wrote with
// write_repro_file, from both the workspace and the submission registry. It
// exists to escape an otherwise-dead end: a broken helper file left in the
// workspace becomes part of the submitted proof and can poison the final
// build (e.g. a stray Go _test.go with a syntax error fails compilation of
// the whole package even when cmd never names it).
type DeleteReproFileTool struct {
	ws *iterationWorkspace
}

// NewDeleteReproFileTool builds a delete_repro_file tool bound to one
// Attempt's iteration workspace holder.
func NewDeleteReproFileTool(ws *iterationWorkspace) *DeleteReproFileTool {
	return &DeleteReproFileTool{ws: ws}
}

// deleteReproFileArgs is the JSON schema for delete_repro_file's arguments.
type deleteReproFileArgs struct {
	// Path is the repo-root-relative path of a previously written repro file.
	Path string `json:"path"`
}

// Def implements agent.Tool.
func (t *DeleteReproFileTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "delete_repro_file",
		Description: "Delete a file you previously wrote with write_repro_file, removing it from the " +
			"workspace AND from the files submitted with your final plan. Only files you wrote this " +
			"attempt can be deleted — repository files are read-only.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Repo-root-relative path of a file previously written via write_repro_file."
    }
  },
  "required": ["path"],
  "additionalProperties": false
}`),
	}
}

// Run implements agent.Tool.
func (t *DeleteReproFileTool) Run(_ context.Context, raw json.RawMessage) (string, error) {
	var args deleteReproFileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf(`invalid arguments (%v): delete_repro_file takes {"path": "<repo-relative path>"}`, err)
	}
	tracked := t.ws.trackedPaths()
	if !t.ws.forget(args.Path) {
		return "", fmt.Errorf("%q is not a file you wrote this attempt (workspace repro files: %s); repository files cannot be deleted",
			args.Path, strings.Join(tracked, ", "))
	}
	// Best-effort disk removal: the registry is authoritative for submission,
	// and the workspace copy only matters for subsequent `workspace exec`
	// calls. A file already absent on disk is fine.
	t.ws.mu.Lock()
	ws := t.ws.path
	t.ws.mu.Unlock()
	if ws != "" {
		if err := os.Remove(filepath.Join(ws, filepath.FromSlash(args.Path))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("delete_repro_file: remove %s: %w", args.Path, err)
		}
	}
	return fmt.Sprintf("deleted %s\nWorkspace repro files (submitted with your final plan): %s",
		args.Path, strings.Join(t.ws.trackedPaths(), ", ")), nil
}

// workspaceApplets enumerates the valid workspace tool applets, used both to
// dispatch a call and to render the "unknown applet" teaching message.
var workspaceApplets = []string{"ls", "cat", "status", "exec"}

// workspaceLSMaxEntries bounds a single `workspace ls` listing so a huge
// directory (e.g. a populated module cache) cannot flood the agent's
// context; entries beyond the cap are summarized with a count.
const workspaceLSMaxEntries = 200

// workspaceUnmaterializedHint is returned by the free ls/cat applets when no
// iteration workspace has been materialized yet. It deliberately does NOT
// materialize one — that would silently copy the whole repo just to answer
// an inspection query — and instead points the agent at what to do next:
// use the (already free) read_file/list_dir for the pristine repo, or take
// an action that materializes the workspace for it.
const workspaceUnmaterializedHint = "workspace not yet materialized: it is created on your first " +
	"write_repro_file call or `workspace exec` call. To inspect the pristine repository before then, " +
	"use read_file/list_dir/grep (rooted at the repo)."

// WorkspaceTool is the busybox-style multiplexer over the agent's
// per-Attempt iteration workspace: argv[0] selects an applet, exactly like a
// shell command. ls/cat/status are FREE, host-side inspection applets — they
// never spin a container and never consume the exec budget, even when
// called before the workspace exists. exec is the single, BUDGETED escape
// path into the sandbox; it behaves exactly like the tool it replaces,
// running the given argv against the persistent iteration workspace and
// rendering the same interpret()-style classification the final plan
// verdict uses (see renderRunReproResult).
//
// bugbot-jto7: replaces RunReproTool. Live dogfood runs showed the agent
// burning its ENTIRE exec budget on environment probes (`which bazel`,
// `ls /vendor/...`) before ever writing a candidate, because
// read_file/list_dir/grep are rooted at the HOST repo and could not see the
// sandbox or the iteration workspace — executing a command was the agent's
// ONLY way to look. The free ls/cat/status applets remove that pressure
// without adding a second command-running tool, which would just recreate
// the same "drain whichever budget remains" failure mode.
type WorkspaceTool struct {
	sb      sandbox.Sandbox
	repoDir string
	image   string
	timeout time.Duration

	roMounts  []sandbox.ROMount
	depEnv    []string
	setupCmds [][]string

	materialize func(repoDir string) (string, error)
	ws          *iterationWorkspace

	maxExec int
	used    atomic.Int32
}

// NewWorkspaceTool builds a workspace tool bound to one Attempt's iteration
// workspace holder. Parameters mirror the removed NewRunReproTool exactly:
// sb executes the sandboxed command for the exec applet; repoDir/image/
// timeout mirror execute()'s Spec policy so an exec run sees the same
// network/dep/timeout/image environment the final plan will; roMounts/
// depEnv/setupCmds carry the resolved dependency strategy; materialize
// lazily creates the iteration workspace (normally sb.(workspaceMaterializer).
// MaterializeWorkspace); ws is the shared holder Attempt cleans up on
// return; maxExec is the per-attempt SANDBOX budget — only exec calls that
// actually reach the sandbox consume it; the free ls/cat/status applets
// never do.
func NewWorkspaceTool(
	sb sandbox.Sandbox,
	repoDir, image string,
	timeout time.Duration,
	roMounts []sandbox.ROMount,
	depEnv []string,
	setupCmds [][]string,
	materialize func(repoDir string) (string, error),
	ws *iterationWorkspace,
	maxExec int,
) *WorkspaceTool {
	return &WorkspaceTool{
		sb:          sb,
		repoDir:     repoDir,
		image:       image,
		timeout:     timeout,
		roMounts:    roMounts,
		depEnv:      depEnv,
		setupCmds:   setupCmds,
		materialize: materialize,
		ws:          ws,
		maxExec:     maxExec,
	}
}

// workspaceArgs is the JSON schema for the tool arguments: a single argv
// array, busybox-style — argv[0] selects the applet, the remainder are its
// arguments (e.g. the file to cat, or the command to exec).
type workspaceArgs struct {
	Argv []string `json:"argv"`
}

// Def implements agent.Tool.
func (t *WorkspaceTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "workspace",
		Description: "Busybox-style multiplexer over your persistent attempt workspace (the repo plus " +
			"every file you wrote via write_repro_file). argv[0] selects the applet:\n" +
			"  [\"ls\", \"<dir>\"]      list a workspace-relative directory (dir defaults to \".\"). FREE.\n" +
			"  [\"cat\", \"<file>\"]    show a workspace-relative file's tail (last 4 KiB). FREE.\n" +
			"  [\"status\"]            report whether the workspace is materialized, your tracked " +
			"repro files, and your exec budget used/remaining. FREE.\n" +
			"  [\"exec\", \"<argv...>\"] run a command against the workspace and see exactly how the " +
			"sandbox classifies it. BUDGETED: consumes the exec budget ONLY when it reaches the sandbox " +
			"— malformed calls, unknown applets, and invalid commands are free.\n" +
			"exec runs in your PERSISTENT attempt workspace, discarded when the attempt ends. It is NEVER " +
			"what the official verdict runs against: that re-runs your submitted plan cmd in a completely " +
			"FRESH workspace containing the repo plus exactly the files you wrote with write_repro_file — " +
			"no build artifacts or other exec side effects carry over, so cmd must perform any build steps " +
			"itself. A file created as a SIDE EFFECT of an exec command (e.g. shell redirection) is NOT " +
			"tracked or submitted — only files written via write_repro_file are.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "argv": {
      "type": "array",
      "description": "Applet and its arguments as a single argv ARRAY of strings, e.g. [\"ls\",\"sub/dir\"], [\"cat\",\"build.log\"], [\"status\"], or [\"exec\",\"go\",\"test\",\"-timeout\",\"60s\",\"-run\",\"TestX\",\"./pkg\"]. Wrap a multi-step shell exec command as [\"exec\",\"bash\",\"-c\",\"<full command>\"].",
      "items": {"type": "string"},
      "minItems": 1
    }
  },
  "required": ["argv"],
  "additionalProperties": false
}`),
	}
}

// Run implements agent.Tool. It dispatches on argv[0]. Unknown applets and
// malformed arguments are rejected for free, mirroring the removed
// RunReproTool's schema-stumble-must-not-cost-budget contract.
func (t *WorkspaceTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args workspaceArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf(`invalid arguments (%v): workspace takes {"argv": ["<applet>", "..."]} — argv is a JSON ARRAY of strings, not a single string`, err)
	}
	if len(args.Argv) == 0 {
		return "", errors.New(`missing "argv": pass the applet and its arguments as an argv array, e.g. {"argv": ["status"]} or {"argv": ["exec","go","test","./..."]}`)
	}

	applet, rest := args.Argv[0], args.Argv[1:]
	switch applet {
	case "ls":
		return t.runLS(rest)
	case "cat":
		return t.runCat(rest)
	case "status":
		return t.runStatus(), nil
	case "exec":
		return t.runExec(ctx, rest)
	default:
		return "", fmt.Errorf("unknown workspace applet %q; valid applets are: %s", applet, strings.Join(workspaceApplets, ", "))
	}
}

// runLS implements the free `ls [dir]` applet: it lists a workspace-relative
// directory (default the workspace root), confined via agent.FSRoot so a
// symlink planted by a build step cannot walk the listing outside the
// workspace. It never materializes an unmaterialized workspace.
func (t *WorkspaceTool) runLS(argv []string) (string, error) {
	dir := "."
	if len(argv) > 0 {
		dir = argv[0]
	}
	wsPath, ok := t.ws.materializedPath()
	if !ok {
		return workspaceUnmaterializedHint, nil
	}
	root, err := agent.NewFSRoot(wsPath)
	if err != nil {
		return "", fmt.Errorf("workspace ls: %w", err)
	}
	abs, err := root.Resolve(dir)
	if err != nil {
		return "", fmt.Errorf("workspace ls %q: %w", dir, err)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", fmt.Errorf("workspace ls %q: %w", dir, err)
	}
	if len(entries) == 0 {
		return "(empty directory)", nil
	}
	total := len(entries)
	truncated := total > workspaceLSMaxEntries
	if truncated {
		entries = entries[:workspaceLSMaxEntries]
	}
	var b strings.Builder
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		b.WriteString(name)
		b.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&b, "... (%d more entries, listing truncated at %d)\n", total-workspaceLSMaxEntries, workspaceLSMaxEntries)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// runCat implements the free `cat <file>` applet: it returns the tail of a
// workspace-relative file capped at runReproOutputTailBytes (build logs are
// the primary use case), with the same tailExcerpt truncation marker
// exec/interpret feedback already uses. Confined via agent.FSRoot. It never
// materializes an unmaterialized workspace.
func (t *WorkspaceTool) runCat(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New(`workspace cat: missing "<file>"; usage: {"argv": ["cat", "<workspace-relative file>"]}`)
	}
	file := argv[0]
	wsPath, ok := t.ws.materializedPath()
	if !ok {
		return workspaceUnmaterializedHint, nil
	}
	root, err := agent.NewFSRoot(wsPath)
	if err != nil {
		return "", fmt.Errorf("workspace cat: %w", err)
	}
	abs, err := root.Resolve(file)
	if err != nil {
		return "", fmt.Errorf("workspace cat %q: %w", file, err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("workspace cat %q: %w", file, err)
	}
	return tailExcerpt(string(data), runReproOutputTailBytes), nil
}

// runStatus implements the free `status` applet: it reports whether the
// iteration workspace is materialized, the sorted list of tracked repro
// files, and the exec budget used/max — the direct fix for the observed
// keep-calling-after-exhaustion failure mode (an agent that hit 5/4 and kept
// retrying), since the agent can now check its remaining budget for free
// instead of guessing.
func (t *WorkspaceTool) runStatus() string {
	wsPath, materialized := t.ws.materializedPath()
	tracked := t.ws.trackedPaths()
	used := int(t.used.Load())

	var b strings.Builder
	if materialized {
		fmt.Fprintf(&b, "materialized: true (%s)\n", wsPath)
	} else {
		b.WriteString("materialized: false (created on your first write_repro_file or `workspace exec` call)\n")
	}
	if len(tracked) == 0 {
		b.WriteString("tracked files: (none)\n")
	} else {
		fmt.Fprintf(&b, "tracked files (%d, submitted with your final plan): %s\n", len(tracked), strings.Join(tracked, ", "))
	}
	fmt.Fprintf(&b, "exec budget: %d/%d used", used, t.maxExec)
	return b.String()
}

// runExec implements the budgeted `exec <argv...>` applet: exactly the
// removed RunReproTool.Run semantics and ordering — validate cmd with the
// SAME rules validatePlan applies to the final plan (so a command that
// clears exec is guaranteed to clear submission too), THEN charge the
// per-attempt sandbox budget — malformed or invalid calls are rejected for
// free, so a schema stumble never eats the agent's real iteration rounds.
// The workspace is lazily materialized on first use and cmd runs against it
// with the stage's network/dep/timeout/image policy. The result is rendered
// with an interpret()-style classification so the agent gets the same
// signal validatePlan/interpret would give the final plan — without
// spending a revision round to learn it.
func (t *WorkspaceTool) runExec(ctx context.Context, cmd []string) (string, error) {
	if len(cmd) == 0 {
		return "", errors.New(`workspace exec: missing command; pass the argv to run, e.g. {"argv": ["exec","go","test","-timeout","60s","-run","TestX","./pkg"]}`)
	}
	if err := validateReproCmd(cmd); err != nil {
		return "", err
	}
	// Budget is charged only after validation: it bounds sandbox capacity, and
	// a call rejected above never reaches the sandbox.
	if err := checkWorkspaceExecBudget(&t.used, t.maxExec); err != nil {
		return "", err
	}

	ws, err := t.ws.ensure(t.repoDir, t.materialize)
	if err != nil {
		return "", fmt.Errorf("workspace exec: materialize iteration workspace: %w", err)
	}

	// Note: unlike execute(), Cmd is passed through UNNORMALIZED — no
	// -json/--junitxml rewrite (see normalizeCmdForStructuredOutput) and no
	// CaptureFiles. This is intentional, not an oversight: exec's result is
	// advisory (the agent's own diagnostic loop), and execute()'s structured,
	// dispositive classification of the SAME cmd on the final plan is what
	// actually gates promotion — so skipping it here costs nothing but a
	// slightly coarser (marker-based) classification during iteration, in
	// exchange for keeping this applet's Spec construction simple and
	// matching exactly what the agent asked to run. No WriteFiles either:
	// write_repro_file already put the tracked files on disk in the
	// workspace.
	spec := sandbox.Spec{
		RepoDir: t.repoDir,
		// Workspace pins this run to the attempt's persistent iteration
		// directory instead of a fresh copy of RepoDir: sb.Exec neither
		// creates a new copy nor removes it (see sandbox.Spec.Workspace).
		// Lifecycle is owned by ws, cleaned up by Attempt's defer, never by
		// this tool.
		Workspace: ws,
		Cmd:       cmd,
		Image:     t.image,
		Timeout:   t.timeout,
		ROMounts:  t.roMounts,
		Env:       t.depEnv,
		SetupCmds: t.setupCmds,
	}
	// res, err below: a sandbox launch failure here is a plain tool error
	// (matching run_tests/RunTestsTool), not an agent.ToolHealthError — the
	// official verdict (execute()) still re-runs the final plan
	// authoritatively, so a transient infra hiccup during iteration need not
	// be escalated as a harness health signal the way a run_tests/sandbox_exec
	// failure would be.
	res, err := t.sb.Exec(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("workspace exec: sandbox execution failed: %w", err)
	}

	return renderRunReproResult(res, cmd), nil
}

// ExecCount returns the number of budget-charged exec calls made so far
// (calls that passed validation, including budget-exceeded attempts
// rejected before reaching the sandbox). Unlike RunTestsTool/
// SandboxExecTool's ExecCount, malformed calls, unknown applets, and
// invalid commands are NOT counted — they are rejected before the budget is
// charged.
func (t *WorkspaceTool) ExecCount() int {
	return int(t.used.Load())
}

// checkWorkspaceExecBudget atomically increments used and returns a
// recoverable tool error once the new count exceeds maxExec. Mirrors the
// shape of agent's unexported checkExecBudget (used by run_tests/
// sandbox_exec) — duplicated here rather than exported cross-package
// because it is three lines and the two packages' tool budgets are
// otherwise unrelated. The message directs the agent at `workspace status`
// (free) instead of guessing at its remaining budget, and at submission —
// the direct fix for the observed keep-calling-after-exhaustion failure
// mode.
func checkWorkspaceExecBudget(used *atomic.Int32, maxExec int) error {
	n := used.Add(1)
	if int(n) > maxExec {
		return fmt.Errorf("workspace exec budget exhausted (%d/%d calls used); run `workspace status` to review "+
			"your tracked files and remaining budget, then submit your final repro plan",
			int(n)-1, maxExec)
	}
	return nil
}

// renderRunReproResult formats a sandbox.Result into the compact text handed
// back to the agent: the raw outcome (exit code, timeout, duration) plus the
// SAME positive-evidence classification interpret() applies to the final
// plan, so an agent iterating via `workspace exec` learns whether its
// candidate would actually be promoted — without spending a revision round
// to find out — followed by a generous tail excerpt of the combined output.
func renderRunReproResult(res sandbox.Result, cmd []string) string {
	v := interpret(res, cmd)
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code=%d timed_out=%t demonstrated=%t duration=%dms",
		res.ExitCode, res.TimedOut, v.demonstrated, res.Duration.Milliseconds())
	if !v.demonstrated {
		fmt.Fprintf(&b, " reason=%s", v.reason)
	}
	b.WriteString("\n\nOutput (tail):\n")
	b.WriteString(tailExcerpt(combinedOutput(res), runReproOutputTailBytes))
	return b.String()
}

var (
	_ agent.Tool = (*WriteReproFileTool)(nil)
	_ agent.Tool = (*DeleteReproFileTool)(nil)
	_ agent.Tool = (*WorkspaceTool)(nil)
)

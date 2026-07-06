package repro

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// tryReproOutputTailBytes bounds the combined-output excerpt returned by
// try_repro. It is deliberately more generous than the 16 KiB cap on
// sandbox_exec/run_tests results (tools_sandbox_exec.go's sandboxOutputCap):
// try_repro is the agent's primary diagnostic loop for its OWN candidate, so
// truncating too aggressively would hide the exact compiler error or
// assertion failure it needs to see to fix the next iteration.
const tryReproOutputTailBytes = 4 * 1024

// workspaceMaterializer is implemented by sandbox backends that can
// pre-materialize a caller-owned workspace outside of Exec (see
// sandbox.Spec.Workspace). *sandbox.CLI implements it via the pristine-
// workspace cache. try_repro is wired only when the configured sandbox
// implements this interface (checked once in newRunner); a backend that
// doesn't (e.g. a bare sandbox.Mock in a test that never scripts iteration)
// simply omits the tool, mirroring how run_tests is omitted when no build
// system is detectable.
type workspaceMaterializer interface {
	MaterializeWorkspace(repoDir string) (string, error)
}

// iterationWorkspace is the lazily-materialized, per-Attempt workspace that
// try_repro writes into and runs commands against. It starts empty (path ==
// "") and is materialized on the FIRST try_repro call within one Attempt;
// every later call in that same Attempt reuses the same directory, so files
// written by one call remain visible (and can be overwritten) by the next —
// the interactive write/run/observe/fix loop the design is built around.
//
// Attempt owns the holder's lifecycle: it constructs one per finding and
// defers cleanup, so the directory (and anything left in it) is gone before
// Attempt returns — the official clean-room verdict in execute() always runs
// against a completely fresh workspace and never observes iteration leftovers.
type iterationWorkspace struct {
	mu   sync.Mutex
	path string
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

// cleanup removes the iteration workspace, if one was ever materialized, and
// resets the holder so a stale path can never be reused. Safe to call
// multiple times (e.g. an Attempt that returns before any try_repro call).
func (w *iterationWorkspace) cleanup() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.path == "" {
		return nil
	}
	err := os.RemoveAll(w.path)
	w.path = ""
	return err
}

// TryReproTool exposes an interactive write/run/observe loop to the
// reproducer agent: unlike the final Plan (submitted once, blind, and re-run
// independently for the promotion verdict), try_repro lets the agent inject
// candidate files and a command, see exactly how the sandbox classifies the
// result, and revise — all within ONE persistent iteration workspace scoped
// to the current Attempt.
//
// The workspace is intentionally NOT the one the official verdict runs
// against: execute() always materializes its own fresh workspace from
// RepoDir (the default Spec path, Workspace left empty), so nothing
// discovered or left behind here can substitute for actually demonstrating
// the bug in the final plan.
type TryReproTool struct {
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

// NewTryReproTool builds a try_repro tool bound to one Attempt's iteration
// workspace holder. sb executes the sandboxed command; repoDir/image/timeout
// mirror execute()'s Spec policy so an iteration run sees the same
// network/dep/timeout/image environment the final plan will; roMounts/depEnv/
// setupCmds carry the resolved dependency strategy; materialize lazily
// creates the iteration workspace (normally sb.(workspaceMaterializer).
// MaterializeWorkspace); ws is the shared holder Attempt cleans up on return;
// maxExec is the per-attempt call budget.
func NewTryReproTool(
	sb sandbox.Sandbox,
	repoDir, image string,
	timeout time.Duration,
	roMounts []sandbox.ROMount,
	depEnv []string,
	setupCmds [][]string,
	materialize func(repoDir string) (string, error),
	ws *iterationWorkspace,
	maxExec int,
) *TryReproTool {
	return &TryReproTool{
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

// tryReproArgs is the JSON schema for the tool arguments — the same files/cmd
// vocabulary as Plan (repro.go), so a candidate that works via try_repro can
// be submitted as the final plan with no reshaping.
type tryReproArgs struct {
	// Files are repro/test files to write into the iteration workspace, keyed
	// by path relative to the repo root. A later call overwrites files at the
	// same path; files from a prior call that are not overwritten persist.
	Files map[string]string `json:"files"`
	// Cmd is the argv to run against the current iteration workspace.
	Cmd []string `json:"cmd"`
}

// Def implements agent.Tool.
func (t *TryReproTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "try_repro",
		Description: "Interactively write candidate repro files and run a command against them in a " +
			"PERSISTENT sandbox workspace scoped to this attempt, so you can write, run, observe the " +
			"output, fix, and re-run BEFORE committing to your final repro plan. Files accumulate across " +
			"calls within this attempt: a later call overwrites files at the same path, and files from an " +
			"earlier call that are not overwritten remain. This workspace is discarded when the attempt " +
			"ends and is NEVER what the final verdict runs against — your submitted plan is independently " +
			"re-run in a completely fresh workspace, so nothing you leave here substitutes for the plan " +
			"actually demonstrating the bug.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "files": {
      "type": "object",
      "description": "Repro/test files to write into the iteration workspace, keyed by path relative to the repo root. Values are the file contents.",
      "additionalProperties": {"type": "string"}
    },
    "cmd": {
      "type": "array",
      "description": "Command (argv) to run against the current iteration workspace.",
      "items": {"type": "string"}
    }
  },
  "required": ["files", "cmd"],
  "additionalProperties": false
}`),
	}
}

// Run implements agent.Tool. It enforces the per-attempt execution budget,
// validates files/cmd with the SAME rules validatePlan applies to the final
// plan (so a candidate that clears try_repro is guaranteed to clear
// submission too), lazily materializes the attempt's iteration workspace on
// the first call, and runs cmd against it with the stage's network/dep/
// timeout/image policy. The result is rendered with an interpret()-style
// classification so the agent gets the same signal validatePlan/interpret
// would give the final plan — without spending a revision round to learn it.
func (t *TryReproTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := checkTryReproBudget(&t.used, t.maxExec); err != nil {
		return "", err
	}

	var args tryReproArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if err := validateFilesCmd(args.Files, args.Cmd, t.repoDir); err != nil {
		return "", err
	}

	ws, err := t.ws.ensure(t.repoDir, t.materialize)
	if err != nil {
		return "", fmt.Errorf("try_repro: materialize iteration workspace: %w", err)
	}

	files := make(map[string][]byte, len(args.Files))
	for path, content := range args.Files {
		files[path] = []byte(content)
	}
	// Note: unlike execute(), Cmd is passed through UNNORMALIZED — no
	// -json/--junitxml rewrite (see normalizeCmdForStructuredOutput) and no
	// CaptureFiles. This is intentional, not an oversight: try_repro's result
	// is advisory (the agent's own diagnostic loop), and execute()'s
	// structured, dispositive classification of the SAME cmd on the final
	// plan is what actually gates promotion — so skipping it here costs
	// nothing but a slightly coarser (marker-based) classification during
	// iteration, in exchange for keeping this tool's Spec construction simple
	// and matching exactly what the agent asked to run.
	spec := sandbox.Spec{
		RepoDir: t.repoDir,
		// Workspace pins this run to the attempt's persistent iteration
		// directory instead of a fresh copy of RepoDir: sb.Exec applies
		// WriteFiles onto it and neither creates a new copy nor removes it
		// (see sandbox.Spec.Workspace). Lifecycle is owned by ws, cleaned up
		// by Attempt's defer, never by this tool.
		Workspace:  ws,
		Cmd:        args.Cmd,
		Image:      t.image,
		Timeout:    t.timeout,
		WriteFiles: files,
		ROMounts:   t.roMounts,
		Env:        t.depEnv,
		SetupCmds:  t.setupCmds,
	}
	// res, err below: a sandbox launch failure here is a plain tool error
	// (matching run_tests/RunTestsTool), not an agent.ToolHealthError — the
	// official verdict (execute()) still re-runs the final plan
	// authoritatively, so a transient infra hiccup during iteration need not
	// be escalated as a harness health signal the way a run_tests/sandbox_exec
	// failure would be.
	res, err := t.sb.Exec(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("try_repro: sandbox execution failed: %w", err)
	}

	return renderTryReproResult(res, args.Cmd), nil
}

// ExecCount returns the number of Run calls made so far (including budget-
// exceeded attempts rejected before reaching the sandbox), mirroring
// RunTestsTool/SandboxExecTool's ExecCount.
func (t *TryReproTool) ExecCount() int {
	return int(t.used.Load())
}

// checkTryReproBudget atomically increments used and returns a recoverable
// tool error once the new count exceeds maxExec. Mirrors the shape of
// agent's unexported checkExecBudget (used by run_tests/sandbox_exec) —
// duplicated here rather than exported cross-package because it is three
// lines and the two packages' tool budgets are otherwise unrelated.
func checkTryReproBudget(used *atomic.Int32, maxExec int) error {
	n := used.Add(1)
	if int(n) > maxExec {
		return fmt.Errorf("try_repro budget exhausted (%d/%d calls used); stop iterating and submit your final repro plan",
			int(n)-1, maxExec)
	}
	return nil
}

// renderTryReproResult formats a sandbox.Result into the compact text handed
// back to the agent: the raw outcome (exit code, timeout, duration) plus the
// SAME positive-evidence classification interpret() applies to the final
// plan, so an agent iterating via try_repro learns whether its candidate
// would actually be promoted — without spending a revision round to find out
// — followed by a generous tail excerpt of the combined output.
func renderTryReproResult(res sandbox.Result, cmd []string) string {
	v := interpret(res, cmd)
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code=%d timed_out=%t demonstrated=%t duration=%dms",
		res.ExitCode, res.TimedOut, v.demonstrated, res.Duration.Milliseconds())
	if !v.demonstrated {
		fmt.Fprintf(&b, " reason=%s", v.reason)
	}
	b.WriteString("\n\nOutput (tail):\n")
	b.WriteString(tailExcerpt(combinedOutput(res), tryReproOutputTailBytes))
	return b.String()
}

var _ agent.Tool = (*TryReproTool)(nil)

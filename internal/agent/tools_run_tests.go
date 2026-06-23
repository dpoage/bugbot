package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// RunTestsTool exposes a constrained, sandboxed test runner to a finder or
// refuter agent. Unlike sandbox_exec (which runs arbitrary commands), this
// tool only runs the repository's test suite — the command is derived from
// the repo's build system and is not caller-supplied. The caller MAY supply
// an optional package path and/or run-filter flag to narrow the test scope.
//
// The tool is budget-gated: calls beyond maxExec return a tool error. It
// shares the same sandbox.Sandbox + dep-strategy machinery as SandboxExecTool;
// no exec path is duplicated.
type RunTestsTool struct {
	sb      sandbox.Sandbox
	repoDir string
	// baseCmd is the suite-level test command (e.g. ["go","test","./..."]) as
	// derived from the repo's build system at construction time. Immutable.
	baseCmd []string
	maxExec int
	used    atomic.Int32

	roMounts  []sandbox.ROMount
	depEnv    []string
	setupCmds [][]string

	// onExec, when non-nil, is called after each run with the wall duration.
	onExec func(time.Duration)
}

// NewRunTestsTool builds a run_tests tool for one agent panel. sb is the
// sandbox backend; repoDir is the repository root; baseCmd is the full-suite
// test argv derived from the build system (e.g. ["go","test","./..."]); maxExec
// is the per-panel execution budget; roMounts/depEnv/setupCmds carry the
// dependency strategy; onExec is an optional hook for stats aggregation.
func NewRunTestsTool(
	sb sandbox.Sandbox,
	repoDir string,
	baseCmd []string,
	maxExec int,
	roMounts []sandbox.ROMount,
	depEnv []string,
	setupCmds [][]string,
	onExec func(time.Duration),
) *RunTestsTool {
	return &RunTestsTool{
		sb:        sb,
		repoDir:   repoDir,
		baseCmd:   baseCmd,
		maxExec:   maxExec,
		roMounts:  roMounts,
		depEnv:    depEnv,
		setupCmds: setupCmds,
		onExec:    onExec,
	}
}

// runTestsArgs is the JSON schema for the tool arguments.
type runTestsArgs struct {
	// Pkg, if non-empty, replaces the default package glob (e.g. "./pkg/...")
	// so the test scope is narrowed to a specific sub-tree.
	Pkg string `json:"pkg,omitempty"`
	// Run, if non-empty, is passed as -run <filter> (Go) or equivalent for
	// other build systems to select a subset of tests.
	Run string `json:"run,omitempty"`
}

// Def implements agent.Tool.
func (t *RunTestsTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "run_tests",
		Description: "Run the repository's test suite in an isolated sandbox to empirically" +
			" confirm or refute a hypothesis. Use this to check whether a bug is reproducible" +
			" via existing tests, or whether a fix makes the tests pass." +
			" This tool ONLY runs tests — it is NOT a place to report findings." +
			" Network is disabled. Narrow the scope with pkg and run when the full suite is slow.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pkg": {
      "type": "string",
      "description": "Optional package path or glob to test (e.g. \"./internal/store/...\"). Omit to run the full suite."
    },
    "run": {
      "type": "string",
      "description": "Optional test name filter (-run for Go, -k for pytest, etc.). Omit to run all tests in the selected package."
    }
  },
  "additionalProperties": false
}`),
	}
}

// Run implements agent.Tool. It enforces the per-panel execution budget,
// builds the test argv, runs it in the sandbox, and returns a compact summary.
// Non-zero exit codes are reported as normal results; only infrastructure
// errors and argument errors are returned as tool errors.
func (t *RunTestsTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	n := t.used.Add(1)
	if int(n) > t.maxExec {
		return "", fmt.Errorf("run_tests budget exhausted (%d/%d calls used); cannot run more test executions for this candidate", int(n)-1, t.maxExec)
	}

	var args runTestsArgs
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", err
	}
	if len(t.baseCmd) == 0 {
		return "", fmt.Errorf("run_tests: no test command could be determined for this repository (unknown build system)")
	}

	cmd := t.buildArgv(args)

	spec := sandbox.Spec{
		RepoDir:   t.repoDir,
		Cmd:       cmd,
		Network:   "none",
		ROMounts:  t.roMounts,
		Env:       t.depEnv,
		SetupCmds: t.setupCmds,
	}

	res, err := t.sb.Exec(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("sandbox execution failed: %w", err)
	}

	if t.onExec != nil {
		t.onExec(res.Duration)
	}

	return renderSandboxResult(res), nil
}

// buildArgv constructs the test command argv from the base command and the
// caller-supplied filters. It understands Go, cargo, pytest, npm/yarn/pnpm,
// ctest, and bazel command shapes; unknown base commands receive no filter
// injection so the tool degrades gracefully rather than producing broken
// invocations.
func (t *RunTestsTool) buildArgv(args runTestsArgs) []string {
	if len(t.baseCmd) == 0 {
		return nil
	}
	runner := t.baseCmd[0]
	switch runner {
	case "go":
		// baseCmd: ["go", "test", "./..."]
		// With pkg: ["go", "test", <pkg>]
		// With run: ["go", "test", <pkg_or_./...>, "-run", <filter>]
		cmd := make([]string, 0, len(t.baseCmd)+3)
		cmd = append(cmd, "go", "test")
		pkg := "./..."
		if args.Pkg != "" {
			pkg = args.Pkg
		}
		cmd = append(cmd, pkg)
		if args.Run != "" {
			cmd = append(cmd, "-run", args.Run)
		}
		return cmd

	case "cargo":
		// baseCmd: ["cargo", "test"]
		// With run filter: ["cargo", "test", <filter>]
		// With pkg (feature): not natively supported; ignore pkg and use run only.
		cmd := make([]string, 0, len(t.baseCmd)+2)
		cmd = append(cmd, "cargo", "test")
		if args.Run != "" {
			cmd = append(cmd, args.Run)
		}
		return cmd

	case "python":
		// baseCmd: ["python", "-m", "pytest"]
		// With pkg: appended as path. With run: "-k <filter>".
		cmd := make([]string, 0, len(t.baseCmd)+3)
		cmd = append(cmd, "python", "-m", "pytest")
		if args.Pkg != "" {
			cmd = append(cmd, args.Pkg)
		}
		if args.Run != "" {
			cmd = append(cmd, "-k", args.Run)
		}
		return cmd

	case "pytest":
		cmd := make([]string, 0, 4)
		cmd = append(cmd, "pytest")
		if args.Pkg != "" {
			cmd = append(cmd, args.Pkg)
		}
		if args.Run != "" {
			cmd = append(cmd, "-k", args.Run)
		}
		return cmd

	case "npm", "yarn", "pnpm", "npx":
		// These don't support inline filters; return the base command unchanged.
		out := make([]string, len(t.baseCmd))
		copy(out, t.baseCmd)
		return out

	case "bazel":
		// baseCmd: ["bazel", "test", "--build_tests_only", "--test_output=errors", "//..."]
		// Narrowing via pkg: preserve every base-command token and replace
		// only the final target token (the "//..." pattern) with args.Pkg.
		// This keeps the canonical flags (--build_tests_only, --test_output)
		// intact instead of rebuilding a bare ["bazel", "test", target].
		out := make([]string, len(t.baseCmd))
		copy(out, t.baseCmd)
		if args.Pkg != "" {
			out[len(out)-1] = args.Pkg
		}
		return out

	default:
		// Unknown runner: return baseCmd verbatim. No filter injection.
		out := make([]string, len(t.baseCmd))
		copy(out, t.baseCmd)
		return out
	}
}

// ExecCount returns the number of Run calls made so far (including
// budget-exceeded attempts rejected before reaching the sandbox).
func (t *RunTestsTool) ExecCount() int {
	return int(t.used.Load())
}

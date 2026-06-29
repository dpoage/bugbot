package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
// caller-supplied filters. It understands Go, cargo, python/pytest,
// npm/yarn/pnpm/npx (verbatim), bazel, and bash-wrapped ctest/meson command
// shapes; unknown base commands receive no filter injection so the tool
// degrades gracefully rather than producing broken invocations.
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

	case "bash":
		// Compound baseCmd shape: ["bash", "-c", script]. Some build systems
		// (cmake/ctest, meson) are invoked through a single shell string; we
		// narrow a runner-anchored filter into the script.
		//
		// The script is shell-parsed by bash, so args.Run MUST be safely
		// single-quoted before splicing — see shellSingleQuote. A malicious
		// or careless value can otherwise break out of the quoted region
		// and inject arbitrary shell.
		if len(t.baseCmd) != 3 || t.baseCmd[1] != "-c" || args.Run == "" {
			// Unexpected shape or no filter to apply: graceful degradation.
			break
		}
		if rewritten, ok := injectBashRunnerFilter(t.baseCmd[2], shellSingleQuote(args.Run)); ok {
			return []string{"bash", "-c", rewritten}
		}
		// Recognizable ctest/meson invocation not found in the script: fall
		// through to the default verbatim behavior so we never produce a
		// broken argv.
		break

	default:
		// Unknown runner: return baseCmd verbatim. No filter injection.
		out := make([]string, len(t.baseCmd))
		copy(out, t.baseCmd)
		return out
	}
	// `case "bash":` reaches here when the shape is unexpected or the script
	// lacks a recognizable ctest/meson invocation. Return baseCmd verbatim
	// (graceful degradation, matching the default path).
	out := make([]string, len(t.baseCmd))
	copy(out, t.baseCmd)
	return out
}

// shellSingleQuote returns s wrapped in POSIX single quotes with any
// embedded single quote escaped via the standard `'\”` sequence. The
// result is safe to splice into a `bash -c` script string: the shell
// will parse it as a single literal token regardless of the contents.
//
// This MUST be used whenever caller-supplied data is concatenated into
// a shell string — relying on bare interpolation lets a value like
// `'; rm -rf / #` break out of the quoted region.
func shellSingleQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			b.WriteString(`'\''`)
		} else {
			b.WriteByte(s[i])
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// isIdentByte reports whether b may appear in a shell identifier
// (alphanumeric or underscore). Used to require word boundaries when
// looking for command names like `ctest` inside a script.
func isIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') || b == '_'
}

// injectBashRunnerFilter rewrites script so that the first ctest or
// meson-test invocation it finds is narrowed by quoted (which is assumed
// to be a pre-quoted filter value, e.g. from shellSingleQuote). Returns
// (rewritten, true) on success; (script, false) when no recognizable
// ctest/meson invocation is present.
//
// Rewriting rules:
//   - `ctest`      -> `ctest -R <quoted>`
//   - `meson test` -> `meson test <quoted>`
//
// Matching is word-boundary aware so identifiers like `myctest_helper`
// or `ctest_does_not_exist` are not falsely rewritten — we keep scanning
// past them until we find a real command boundary.
//
// Matching is purely lexical: it does not parse shell quoting, so it assumes
// the script does not contain `ctest`/`meson test` inside a quoted string or
// comment (build-system-derived baseCmd scripts do not). Even if it did, the
// spliced value is always single-quoted, so this can never inject a command —
// at worst the filter lands in the wrong place and the suite runs unfiltered.
func injectBashRunnerFilter(script, quoted string) (string, bool) {
	const ctest = "ctest"
	const mesonTest = "meson test"

	// Walk the script looking for the first ctest token at a word
	// boundary. A naive strings.Index may land inside an identifier
	// (e.g. "ctest_helper" or "ctest_does_not_exist"), which we must
	// skip past to reach the real ctest command later in the script.
	for i := 0; i < len(script); {
		j := strings.Index(script[i:], ctest)
		if j < 0 {
			break
		}
		i += j
		prevOK := i == 0 || !isIdentByte(script[i-1])
		nextOK := i+len(ctest) == len(script) || !isIdentByte(script[i+len(ctest)])
		if prevOK && nextOK {
			// Insert " -R <quoted>" right after the ctest token.
			return script[:i+len(ctest)] + " -R " + quoted + script[i+len(ctest):], true
		}
		i += len(ctest) // not a boundary match — keep searching past it
	}
	// Mirror the ctest scan for `meson test`: require word boundaries on BOTH
	// sides so "my_meson test" / "meson testing" do not match, and keep
	// scanning past a non-boundary hit to reach a real later invocation.
	for i := 0; i < len(script); {
		j := strings.Index(script[i:], mesonTest)
		if j < 0 {
			break
		}
		i += j
		prevOK := i == 0 || !isIdentByte(script[i-1])
		nextOK := i+len(mesonTest) == len(script) || !isIdentByte(script[i+len(mesonTest)])
		if prevOK && nextOK {
			return script[:i+len(mesonTest)] + " " + quoted + script[i+len(mesonTest):], true
		}
		i += len(mesonTest)
	}
	return script, false
}

// ExecCount returns the number of Run calls made so far (including
// budget-exceeded attempts rejected before reaching the sandbox).
func (t *RunTestsTool) ExecCount() int {
	return int(t.used.Load())
}

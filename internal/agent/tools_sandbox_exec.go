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

// sandboxOutputCap is the per-stream cap applied to sandbox_exec tool results
// before they are returned to the model. The sandbox backend already caps at
// 1 MiB, but that is prompt-hostile; 16 KiB is readable and still carries
// most relevant output.
const sandboxOutputCap = 16 * 1024

// SandboxExecTool exposes a sandboxed command execution to a refuter agent.
// Each instance is bound to one candidate's refuter panel and holds a
// per-candidate execution counter. Calls beyond the configured budget return
// a tool error string telling the agent the budget is exhausted; the counter
// is atomic so the tool is safe to share across the sequentially-run refuters
// within a single candidate's panel.
//
// The tool is NEVER shared across candidates: create a new instance for each
// candidate via NewSandboxExecTool.
type SandboxExecTool struct {
	sb      sandbox.Sandbox
	repoDir string
	maxExec int
	used    atomic.Int32

	// roMounts / depEnv / setupCmds carry the resolved dependency strategy (a
	// read-only module-cache mount and/or GOFLAGS and/or pre-Cmd setup commands)
	// so a refuter's network-none probe can resolve external modules. Empty for
	// vendored/off repos. The one-time online prefetch (DepStrategyFetch) is
	// performed by the caller before any tool runs.
	roMounts  []sandbox.ROMount
	depEnv    []string
	setupCmds [][]string

	// onExec, when non-nil, is called after each successful Exec with the
	// duration. Used by the funnel to aggregate Stats.SandboxExecMillis.
	onExec func(d time.Duration)
}

// NewSandboxExecTool builds a sandbox_exec tool instance for one candidate's
// refuter panel. sb is the sandbox backend; repoDir is the repository root
// passed as Spec.RepoDir; maxExec is the per-candidate execution budget;
// roMounts, depEnv, and setupCmds carry the dependency strategy (read-only
// module-cache mount, env, and pre-Cmd setup commands) so module-dependent
// probes work under --network=none.
func NewSandboxExecTool(sb sandbox.Sandbox, repoDir string, maxExec int, roMounts []sandbox.ROMount, depEnv []string, setupCmds [][]string, onExec func(time.Duration)) *SandboxExecTool {
	return &SandboxExecTool{sb: sb, repoDir: repoDir, maxExec: maxExec, roMounts: roMounts, depEnv: depEnv, setupCmds: setupCmds, onExec: onExec}
}

// sandboxExecArgs is the JSON schema for the tool arguments.
type sandboxExecArgs struct {
	// Cmd is the command to run (argv). Required and non-empty.
	Cmd []string `json:"cmd"`
	// Files are workspace-relative files to inject before execution, keyed by
	// path. The values are the UTF-8 file contents. Optional.
	Files map[string]string `json:"files,omitempty"`
}

// Def implements agent.Tool.
func (t *SandboxExecTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "sandbox_exec",
		Description: "Execute a command in an isolated sandbox against the repository to" +
			" empirically demonstrate or disprove a bug report. Use this to run the guard" +
			" path, an existing test, or a small probe test you inject via `files`." +
			" A clean exit that exercises the claimed bug path is strong refutation evidence;" +
			" an exit confirming the bug means do NOT refute. Network is disabled." +
			" cmd is required; files is optional (workspace-relative paths to inject before running).",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "cmd": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "description": "Command to execute (argv). Example: [\"go\",\"test\",\"-run\",\"TestFoo\",\"./...\"]"
    },
    "files": {
      "type": "object",
      "additionalProperties": {"type": "string"},
      "description": "Optional files to write into the workspace before execution, keyed by workspace-relative path. Example: {\"pkg/probe_test.go\": \"package pkg ...\"}"
    }
  },
  "required": ["cmd"],
  "additionalProperties": false
}`),
	}
}

// Run implements agent.Tool. It enforces the per-candidate execution budget,
// validates arguments, runs the sandbox, and returns a compact text summary.
// Non-zero exit codes are reported as normal results, not tool errors. Only
// infrastructure failures (sandbox launch errors) and argument validation
// errors are returned as tool errors.
func (t *SandboxExecTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	// Check the budget before doing anything else.
	n := t.used.Add(1)
	if int(n) > t.maxExec {
		return "", fmt.Errorf("sandbox execution budget exhausted (%d/%d calls used); cannot run more executions for this candidate", int(n)-1, t.maxExec)
	}

	var args sandboxExecArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if len(args.Cmd) == 0 {
		return "", fmt.Errorf("cmd is required and must be non-empty")
	}

	// Build WriteFiles, converting string values to []byte. Path validation
	// happens inside the sandbox backend (sandbox.Spec documents that paths
	// escaping the workspace via absolute paths or ".." are rejected).
	var writeFiles map[string][]byte
	if len(args.Files) > 0 {
		writeFiles = make(map[string][]byte, len(args.Files))
		for path, content := range args.Files {
			writeFiles[path] = []byte(content)
		}
	}

	spec := sandbox.Spec{
		RepoDir:    t.repoDir,
		Cmd:        args.Cmd,
		Network:    "none",
		WriteFiles: writeFiles,
		ROMounts:   t.roMounts,
		Env:        t.depEnv,
		SetupCmds:  t.setupCmds,
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

// ExecCount returns the number of Exec calls made so far (including budget-
// exceeded attempts that were rejected before reaching the sandbox).
func (t *SandboxExecTool) ExecCount() int {
	return int(t.used.Load())
}

// renderSandboxResult formats a sandbox.Result into a compact text summary
// suitable for the model's context. Stdout and stderr are each capped at
// sandboxOutputCap (16 KiB) with an explicit truncation marker.
func renderSandboxResult(r sandbox.Result) string {
	var b []byte

	durationMS := r.Duration.Milliseconds()

	if r.TimedOut {
		b = fmt.Appendf(b, "exit_code=-1 timed_out=true duration=%dms\n", durationMS)
	} else {
		b = fmt.Appendf(b, "exit_code=%d timed_out=false duration=%dms\n", r.ExitCode, durationMS)
	}

	b = append(b, "\nSTDOUT:\n"...)
	if r.Stdout == "" {
		b = append(b, "(empty)\n"...)
	} else {
		stdout := r.Stdout
		truncated := false
		if len(stdout) > sandboxOutputCap {
			stdout = stdout[:sandboxOutputCap]
			truncated = true
		}
		b = append(b, stdout...)
		if truncated {
			b = append(b, "\n[stdout truncated at 16KiB]\n"...)
		}
	}

	b = append(b, "\nSTDERR:\n"...)
	if r.Stderr == "" {
		b = append(b, "(empty)\n"...)
	} else {
		stderr := r.Stderr
		truncated := false
		if len(stderr) > sandboxOutputCap {
			stderr = stderr[:sandboxOutputCap]
			truncated = true
		}
		b = append(b, stderr...)
		if truncated {
			b = append(b, "\n[stderr truncated at 16KiB]\n"...)
		}
	}

	return string(b)
}

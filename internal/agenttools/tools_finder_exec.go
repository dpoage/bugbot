package agenttools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/agent"
	"github.com/dpoage/llmkit/sandbox"
)

// FinderExecConfig parameterizes one FinderExecTool. One instance is created
// per finder unit; MaxExecs is that unit's entire run_code budget.
type FinderExecConfig struct {
	Sandbox   sandbox.Sandbox
	RepoDir   string              // the backend copies it per Exec
	Network   sandbox.NetworkMode // set verbatim in every Spec; never empty
	MaxExecs  int                 // >= 1
	Timeout   time.Duration       // > 0
	OutputCap int                 // bytes per stream in the rendered result; > 0
	ROMounts  []sandbox.ROMount
	Env       []string
	SetupCmds [][]string
	OnExec    func(time.Duration) // optional; called after each successful Exec with res.Duration
}

// FinderExecTool is the finder's run_code tool: the model authors one shell
// command string, executed via bash -c in a fresh copy of the repository per
// Exec. Never share an instance across finder units — the budget is per unit.
type FinderExecTool struct {
	cfg  FinderExecConfig
	used atomic.Int32
}

// NewFinderExecTool builds the run_code tool for one finder unit.
func NewFinderExecTool(cfg FinderExecConfig) *FinderExecTool {
	return &FinderExecTool{cfg: cfg}
}

type finderExecArgs struct {
	Command string            `json:"command"`
	Files   map[string]string `json:"files,omitempty"`
}

// Def implements agent.Tool.
func (t *FinderExecTool) Def() llmkit.ToolDef {
	network := "Do not rely on network access."
	if t.cfg.Network == sandbox.NetworkNone {
		network = "The network is disabled."
	}
	return llmkit.ToolDef{
		Name: "run_code",
		Description: fmt.Sprintf(
			"Run a shell command (bash -c) in a fresh copy of the repository with the project's environment; "+
				"returns the exit code, stdout, and stderr. Budget %d runs, timeout %gs each. %s",
			t.cfg.MaxExecs, t.cfg.Timeout.Seconds(), network),
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "minLength": 1,
      "description": "Shell command to execute via bash -c. Example: \"python -c 'import astropy.constants as c; print(c.c)'\""
    },
    "files": {
      "type": "object",
      "additionalProperties": {"type": "string"},
      "description": "Optional files to write into the workspace copy before execution, keyed by workspace-relative path. Example: {\"probe.py\": \"import numpy as np\\nprint(np.__version__)\"}"
    }
  },
  "required": ["command"],
  "additionalProperties": false
}`),
	}
}

// Run implements agent.Tool: budget check first, then argument validation,
// then one sandbox Exec. Non-zero exit codes are normal results; argument
// and infrastructure failures are tool errors.
func (t *FinderExecTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := checkExecBudget(&t.used, t.cfg.MaxExecs, "run_code budget exhausted (%d/%d calls used); cannot run more code executions for this finder"); err != nil {
		return "", err
	}

	var args finderExecArgs
	if err := agent.UnmarshalArgs(raw, &args); err != nil {
		return "", err
	}
	if args.Command == "" {
		return "", fmt.Errorf("command is required and must be non-empty")
	}

	var writeFiles map[string][]byte
	if len(args.Files) > 0 {
		writeFiles = make(map[string][]byte, len(args.Files))
		for path, content := range args.Files {
			// Pre-validate so an escaping path is a retryable argument error, not a ToolHealthError from Exec.
			if err := sandbox.ValidateWorkspacePath(path); err != nil {
				return "", fmt.Errorf("invalid files path %q: %w", path, err)
			}
			writeFiles[path] = []byte(content)
		}
	}

	spec := sandbox.Spec{
		RepoDir:    t.cfg.RepoDir,
		Cmd:        []string{"bash", "-c", args.Command},
		Network:    t.cfg.Network,
		Timeout:    t.cfg.Timeout,
		WriteFiles: writeFiles,
		ROMounts:   t.cfg.ROMounts,
		Env:        t.cfg.Env,
		SetupCmds:  t.cfg.SetupCmds,
	}
	res, err := t.cfg.Sandbox.Exec(ctx, spec)
	if err != nil {
		return "", &agent.ToolHealthError{
			Reason: "sandbox runtime unavailable",
			Err:    fmt.Errorf("sandbox execution failed: %w", err),
		}
	}

	if t.cfg.OnExec != nil {
		t.cfg.OnExec(res.Duration)
	}

	return renderSandboxResult(res, t.cfg.OutputCap), nil
}

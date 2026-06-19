// Package repro implements the Reproduce stage of the Bugbot pipeline: the
// promotion of a Tier-2 ("verified") finding to Tier-1 ("reproduced") by
// generating a minimal failing test and demonstrating the bug inside the
// sandbox.
//
// The contract for Tier-1 is strong: a finding is promoted only when a
// sandboxed command exits non-zero in a way that is consistent with the bug
// the finding describes — not merely because some process crashed. A repro
// that fails to compile, fails to resolve dependencies, or exits cleanly does
// NOT demonstrate the bug; those outcomes feed corrective feedback back to the
// reproducer agent for one or more revision rounds (bounded by MaxAttempts)
// and, if never demonstrated, leave the finding at Tier-2 untouched. Failure
// demotes nothing.
//
// On success the stage writes a self-contained artifact bundle to disk under
// ArtifactDir/<finding-id>/: the injected repro files, a run.sh capturing the
// command, and a README.md describing the finding and how to run it. The
// finding's store row is updated to tier 1 with repro_path pointing at that
// bundle.
//
// The stage drives an [agent.Runner] with read-only tools (read_file,
// list_dir, grep) rooted at the target repo, so the reproducer can investigate
// the finding's file/line/reasoning before proposing a repro plan. Execution
// goes through the [sandbox.Sandbox] interface, so unit tests use
// sandbox.NewMock and a scripted llm.Client with no real container runtime.
package repro

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// Defaults for Options. Reproduction is deliberately conservative: each
// sandbox run copies the whole repo workspace (disk-heavy), so parallelism and
// attempt counts are kept low.
const (
	// DefaultMaxAttempts is the number of repro plans tried per finding before
	// giving up. The first attempt plus revision rounds count toward this.
	DefaultMaxAttempts = 2
	// DefaultMaxParallel bounds concurrent findings under PromoteAll. Sandbox
	// workspace copies are disk-heavy, so the default is intentionally small.
	DefaultMaxParallel = 2
	// DefaultTimeout bounds a single sandbox execution when Options.Timeout is
	// unset.
	DefaultTimeout = 90 * time.Second
	// DefaultArtifactDir is the host directory under which per-finding repro
	// bundles are written when Options.ArtifactDir is unset.
	DefaultArtifactDir = ".bugbot/repro"
)

// Options configures a Reproducer.
type Options struct {
	// MaxAttempts is the maximum number of repro plans tried per finding
	// (initial plan + revisions). Zero uses DefaultMaxAttempts; negative is
	// treated as 1.
	MaxAttempts int
	// Timeout bounds a single sandbox execution. Zero uses DefaultTimeout.
	Timeout time.Duration
	// Image overrides the sandbox's default container image for repro runs.
	// Empty uses the sandbox backend's configured default.
	Image string
	// ArtifactDir is the host directory under which per-finding repro bundles
	// are written. Empty uses DefaultArtifactDir.
	ArtifactDir string
	// MaxParallel bounds concurrent findings processed by PromoteAll. Zero uses
	// DefaultMaxParallel; negative is treated as 1.
	MaxParallel int
	// AgentLimits bounds each reproducer agent run (iterations / token budget).
	// Zero-value fields resolve to agent defaults.
	AgentLimits agent.Limits
	// TranscriptDir, when non-empty, makes each reproducer agent auto-save its
	// transcript there.
	TranscriptDir string
	// PatchProver enables the patch-prover stage: after a successful repro,
	// attempt to produce a minimal fix and prove it with a sandboxed suite run.
	PatchProver bool
	// PatchMaxAttempts is the maximum number of fix plans tried per finding
	// before flagging it needs-human. Zero uses the default (3); negative is
	// treated as 1.
	PatchMaxAttempts int
	// PatchSuiteCmd is the full-suite test command for the suite-green half of
	// the fix witness. Empty means detect from repo marker files (go.mod,
	// Cargo.toml, package.json, pyproject.toml/setup.py); if detection also
	// fails the patch-prover skips for that finding.
	PatchSuiteCmd []string
	// DepStrategy selects how external module dependencies are made available to
	// the network-none sandbox (see sandbox.DepStrategy). Empty/"off" keeps the
	// current behavior (only vendored repos build offline). "host" mounts the
	// host module cache read-only; "fetch" warms a bugbot-managed cache with one
	// online download then mounts it read-only. Vendored repos are always
	// detected regardless of this value.
	DepStrategy sandbox.DepStrategy
	// SetupCmds are operator-supplied commands to run inside the sandbox BEFORE
	// the main command AND BEFORE any per-ecosystem offline-install setup (e.g.
	// pip install from cache). Use to install system-level dependencies
	// (apt packages, shared libraries) that must be present before ecosystem
	// tools run. Commands share the same network-none run and must not need the
	// network. Each entry is a non-empty argv; a failed setup exits with code
	// 125 (env_error, never a bug demonstration).
	SetupCmds [][]string
	// LocalMounts are read-only host directories bind-mounted into the sandbox,
	// independent of dep_strategy. Use to expose monorepo siblings or
	// locally-checked-out path dependencies that fall outside the scanned repo.
	// Mounts are read-only with Shared=true (no SELinux :Z relabel).
	LocalMounts []sandbox.ROMount
	// Capabilities is the pre-probed CapabilitySet for the sandbox image.
	// When non-nil, the reproducer prompt enumerates available invocation
	// modes and instructs the agent to avoid unavailable ones (e.g. -race
	// when cgo is absent). A nil CapabilitySet is treated as "all unknown"
	// and the prompt omits capability guidance.
	Capabilities sandbox.CapabilitySet
}

// resolve returns a copy of o with defaults applied; it does not mutate the
// caller's value.
func (o Options) resolve() Options {
	if o.MaxAttempts == 0 {
		o.MaxAttempts = DefaultMaxAttempts
	}
	if o.MaxAttempts < 0 {
		o.MaxAttempts = 1
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.ArtifactDir == "" {
		o.ArtifactDir = DefaultArtifactDir
	}
	if o.MaxParallel == 0 {
		o.MaxParallel = DefaultMaxParallel
	}
	if o.MaxParallel < 0 {
		o.MaxParallel = 1
	}
	return o
}

// Reproducer promotes verified findings to Tier-1 by generating and executing
// minimal failing tests in the sandbox. Construct one with New and reuse it
// across findings; it holds no per-finding mutable state and Attempt is safe
// for concurrent use (the sandbox and store are concurrency-safe, and each
// Attempt builds its own agent.Runner).
type Reproducer struct {
	client  llm.Client
	sb      sandbox.Sandbox
	repoDir string
	opts    Options
	// deps is the resolved dependency strategy for repoDir: the read-only mounts
	// and extra env every sandbox run carries, plus an optional one-time
	// Prefetch hook (run once from PromoteAll). Resolved once in New.
	deps sandbox.Resolution
	// buildSystems is the set of build systems detected in repoDir, resolved
	// once in New and threaded into the system prompt so C/C++ findings get
	// build-system-specific repro guidance without re-detecting per attempt.
	buildSystems []ingest.BuildSystem
	// capabilities is the probed CapabilitySet threaded from Options.Capabilities,
	// passed to systemPrompt to constrain available invocation modes.
	capabilities sandbox.CapabilitySet
	// nav is the shared code-navigation tool bundle (find_definition,
	// find_references, find_implementations, read_symbol, find_usages, outline)
	// rooted at repoDir. Constructed eagerly in New; no language-server process
	// is started until the first query. Closed by Close.
	nav *agent.CodeNav
}

// New constructs a Reproducer. client is the reproducer-role LLM client, sb is
// the sandbox used to execute repro plans, and repoDir is the host path to the
// target repository the agent investigates and the sandbox runs against. All
// three are required.
func New(client llm.Client, sb sandbox.Sandbox, repoDir string, opts Options) (*Reproducer, error) {
	if client == nil {
		return nil, errors.New("repro: nil llm client")
	}
	if sb == nil {
		return nil, errors.New("repro: nil sandbox")
	}
	if repoDir == "" {
		return nil, errors.New("repro: empty repoDir")
	}
	resolved := opts.resolve()
	deps, err := sandbox.ResolveDeps(repoDir, sandbox.DepOptions{
		Strategy:     resolved.DepStrategy,
		FetchSandbox: sb,
		FetchImage:   resolved.Image,
		LocalMounts:  resolved.LocalMounts,
	})
	if err != nil {
		return nil, fmt.Errorf("repro: resolve dependency strategy: %w", err)
	}
	// Prepend operator setup_cmds BEFORE ecosystem-derived setup commands so
	// system-level dependencies (apt packages, shared libraries) are present
	// when the ecosystem installer (e.g. pip install from wheelhouse) runs.
	// Both run in the same network-none container; operator cmds must not need
	// the network. See config.Sandbox.SetupCmds.
	if len(resolved.SetupCmds) > 0 {
		deps.SetupCmds = append(resolved.SetupCmds, deps.SetupCmds...)
	}
	nav, err := agent.NewCodeNav(repoDir)
	if err != nil {
		return nil, fmt.Errorf("repro: init code-nav: %w", err)
	}
	return &Reproducer{
		client:       client,
		sb:           sb,
		repoDir:      repoDir,
		opts:         resolved,
		deps:         deps,
		buildSystems: ingest.DetectBuildSystems(repoDir),
		capabilities: resolved.Capabilities,
		nav:          nav,
	}, nil
}

// Attempt is the outcome of attempting to reproduce a single finding.
type Attempt struct {
	// FindingID is the id of the finding this attempt targeted.
	FindingID string
	// Promoted reports whether the bug was demonstrated (and the finding is
	// eligible for Tier-1 promotion).
	Promoted bool
	// ArtifactPath is the host directory containing the repro bundle, set only
	// when Promoted is true.
	ArtifactPath string
	// Attempts is the number of repro plans executed (1..MaxAttempts).
	Attempts int
	// Output is a short human-readable summary of the demonstrating (or final)
	// sandbox run's output, for display in summaries.
	Output string
	// Plan is the repro plan that demonstrated the bug, set only when Promoted.
	Plan *Plan
	// Reason explains a non-promotion (the last failure category) for display.
	Reason string
}

// Plan is the reproducer agent's proposal for demonstrating a bug. It is the
// JSON contract the agent returns via RunJSON, and is also the input a caller
// can construct by hand (bypassing the LLM) for integration tests.
type Plan struct {
	// Files are repro/test files to inject into the workspace before running,
	// keyed by workspace-relative path. For Go, this is typically a single
	// _test.go file.
	Files map[string]string `json:"files"`
	// Cmd is the argv used to run the repro (e.g. ["go","test","-run","TestX"]).
	Cmd []string `json:"cmd"`
	// Expect is a short description of the expected failure, used in the
	// artifact README and for human review.
	Expect string `json:"expect"`
}

// Attempt tries to reproduce finding: it asks the reproducer agent for a repro
// plan, executes it in the sandbox, and interprets the result, revising up to
// MaxAttempts times. On a demonstrated failure it writes the artifact bundle
// and returns Attempt{Promoted: true}. It returns a non-nil error only for
// infrastructure failures (agent/LLM error, sandbox launch failure, artifact
// write failure) — a finding that simply could not be reproduced is reported
// via Attempt{Promoted: false}, not an error.
//
// Attempt does NOT update the store; PromoteAll owns persistence so that the
// promotion (tier + repro_path) and the Summary stay consistent. Callers using
// Attempt directly are responsible for any store updates.
func (r *Reproducer) Attempt(ctx context.Context, finding store.Finding) (*Attempt, error) {
	runner, err := r.newRunner(ingest.DetectLanguage(finding.File), r.buildSystems)
	if err != nil {
		return nil, fmt.Errorf("repro: build agent runner: %w", err)
	}

	att := &Attempt{FindingID: finding.ID}

	// feedback carries the prior run's output back into the next plan request so
	// the agent can correct a non-demonstrating repro. Empty on the first pass.
	var feedback string

	for i := 0; i < r.opts.MaxAttempts; i++ {
		att.Attempts = i + 1

		plan, perr := r.planFor(ctx, runner, finding, feedback)
		if perr != nil {
			return nil, fmt.Errorf("repro: plan finding %s: %w", finding.ID, perr)
		}
		if verr := validatePlan(plan); verr != nil {
			// A structurally invalid plan is treated like a non-demonstrating
			// attempt: feed the problem back and try again.
			feedback = fmt.Sprintf("Your previous plan was invalid: %s. Provide files, a cmd, and an expect description.", verr)
			att.Reason = "invalid plan: " + verr.Error()
			continue
		}

		res, serr := r.execute(ctx, plan)
		if serr != nil {
			return nil, fmt.Errorf("repro: execute finding %s: %w", finding.ID, serr)
		}

		verdict := interpret(res, plan.Cmd)
		att.Output = verdict.summary

		if verdict.demonstrated {
			path, werr := writeArtifacts(r.opts.ArtifactDir, finding, plan, res)
			if werr != nil {
				return nil, fmt.Errorf("repro: write artifacts for finding %s: %w", finding.ID, werr)
			}
			att.Promoted = true
			att.ArtifactPath = path
			att.Plan = plan
			att.Reason = ""
			return att, nil
		}

		att.Reason = string(verdict.reason)
		feedback = verdict.feedback(plan)
	}

	return att, nil
}

// newRunner builds a read-only agent runner rooted at the repo for the
// reproducer role. lang is the finding's file language, used to seed the
// language-specific test-framework guidance in the system prompt. systems
// refines that guidance for C/C++ (cmake > meson > generic fallback).
func (r *Reproducer) newRunner(lang ingest.Language, systems []ingest.BuildSystem) (*agent.Runner, error) {
	tools, err := readOnlyTools(r.repoDir)
	if err != nil {
		return nil, err
	}
	tools = append(tools, r.nav.Tools()...)
	var opts []agent.Option
	opts = append(opts, agent.WithLimits(r.opts.AgentLimits))
	if r.opts.TranscriptDir != "" {
		opts = append(opts, agent.WithTranscriptDir(r.opts.TranscriptDir))
	}
	return agent.NewRunner(r.client, tools, systemPrompt(lang, systems, r.capabilities), opts...), nil
}

// Close shuts down any language servers the code-navigation tools spawned.
// Safe to call multiple times and on a nil receiver (so deferred Close calls
// on a partially-initialised Reproducer never panic).
func (r *Reproducer) Close() error {
	if r == nil || r.nav == nil {
		return nil
	}
	return r.nav.Close()
}

// readOnlyTools builds the read-only investigation tool set rooted at dir.
func readOnlyTools(dir string) ([]agent.Tool, error) {
	read, err := agent.NewReadFile(dir)
	if err != nil {
		return nil, err
	}
	list, err := agent.NewListDir(dir)
	if err != nil {
		return nil, err
	}
	grep, err := agent.NewGrep(dir)
	if err != nil {
		return nil, err
	}
	return []agent.Tool{read, list, grep}, nil
}

// execute runs a plan in the sandbox with the stage's network/timeout/image
// policy.
func (r *Reproducer) execute(ctx context.Context, plan *Plan) (sandbox.Result, error) {
	files := make(map[string][]byte, len(plan.Files))
	for path, content := range plan.Files {
		files[path] = []byte(content)
	}
	spec := sandbox.Spec{
		RepoDir:    r.repoDir,
		Cmd:        plan.Cmd,
		Image:      r.opts.Image,
		Network:    "none",
		Timeout:    r.opts.Timeout,
		WriteFiles: files,
		// Dependency strategy: mount a module cache read-only and/or set GOFLAGS
		// so the network-none run can resolve external modules. SetupCmds installs
		// non-Go ecosystem packages from the mounted cache before Cmd runs.
		ROMounts:  r.deps.ROMounts,
		Env:       r.deps.Env,
		SetupCmds: r.deps.SetupCmds,
	}
	return r.sb.Exec(ctx, spec)
}

// validatePlan rejects structurally unusable plans before spending a sandbox
// run on them. A failure here is recoverable: Attempt feeds the message back to
// the agent and revises, so the checks below double as corrective guidance.
func validatePlan(p *Plan) error {
	if len(p.Files) == 0 {
		return errors.New("no repro files")
	}
	if len(p.Cmd) == 0 {
		return errors.New("no command")
	}
	for path := range p.Files {
		// Injected file keys must be workspace-relative: an absolute or
		// escaping path (e.g. "/tmp/repro_test.cpp") is rejected by the sandbox
		// at write time, which would otherwise abort the whole attempt with a
		// hard infrastructure error instead of a recoverable revision. Catch it
		// here using the sandbox's own rule so the agent is told to fix it.
		if err := sandbox.ValidateWorkspacePath(path); err != nil {
			return fmt.Errorf("file %q must be a workspace-relative path inside the repo "+
				"(no leading %q, no %q), e.g. %q: %w", path, "/", "..", "repro_test.cpp", err)
		}
	}
	return nil
}

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
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ecosystem"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/sandbox"
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
	// DefaultMaxIterations bounds the reproducer agent's investigation turns per
	// attempt. The agent package's default (20) is too tight for large repos: the
	// agent spends every turn orienting (build system, test layout) and is forced
	// to emit a blind plan before it can iterate. A higher cap, paired with the
	// cartographer package summaries fed into the task, gives it room to plan and
	// revise. Applied in resolve when AgentLimits.MaxIterations is zero.
	DefaultMaxIterations = 40
	// DefaultSandboxMaxExecs is the per-attempt budget of run_tests calls the
	// reproducer agent may make. A value of 3 lets the agent verify the
	// toolchain, inspect output, and confirm the suite layout without burning
	// unreasonable sandbox capacity.
	DefaultSandboxMaxExecs = 3
	// DefaultTryMaxExecs is the per-attempt budget of `workspace exec` calls
	// the reproducer agent may make. Unlike run_tests (read-only orientation
	// against the repo's existing suite), workspace exec lets the agent run
	// and observe its OWN candidate repro in the iteration workspace before
	// committing to a final plan. This is a single shared pool for BOTH
	// probing the sandbox environment (toolchain, layout) and rehearsing the
	// candidate — the workspace tool's free ls/cat/status applets cover most
	// probe-only needs without spending this budget, so raising it to 10
	// (from 4) buys real write/run/observe/fix looping capacity rather than
	// re-adding the probe pressure the free applets exist to remove. Only
	// calls that reach the sandbox consume it; writes (write_repro_file) and
	// the free applets are free.
	DefaultTryMaxExecs = 10
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
	// Network records the sandbox network policy applied to repro runs
	// (config.Sandbox.Network), purely for manifest.json bookkeeping (see
	// writeArtifacts/buildManifest) — it does NOT affect execute()'s Spec,
	// which deliberately leaves Spec.Network unset so the run inherits the
	// sandbox backend's own configured default (see execute's doc comment).
	// Empty resolves to "none", the package's documented hardened default.
	Network string
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
	// SandboxMaxExecs is the per-attempt execution budget: the reproducer agent
	// may call run_tests at most this many times per attempt to orient itself
	// before proposing its repro plan. Zero uses DefaultSandboxMaxExecs.
	SandboxMaxExecs int
	// TryMaxExecs is the per-attempt execution budget for `workspace exec`:
	// the reproducer agent may run/observe its candidate repro at most this
	// many times per attempt before committing to its final plan. Zero uses
	// DefaultTryMaxExecs. The workspace tool set (write_repro_file,
	// delete_repro_file, workspace) is only wired when the sandbox backend
	// supports workspace materialization (see newRunner); Mock-backed tests
	// that do not implement it never see the tools regardless of this budget.
	TryMaxExecs int
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
	// HostToolchains are host toolchain names (resolved from the host PATH) or
	// explicit host directories to bind-mount read-only into the sandbox and
	// prepend to its PATH — see sandbox.ResolveHostToolchains. Independent of
	// LocalMounts and DepStrategy; the CLI wires it to config.Sandbox.HostToolchains.
	HostToolchains []string
	// Capabilities is the pre-probed CapabilitySet for the sandbox image.
	// When non-nil, the reproducer prompt enumerates available invocation
	// modes and instructs the agent to avoid unavailable ones (e.g. -race
	// when cgo is absent). A nil CapabilitySet is treated as "all unknown"
	// and the prompt omits capability guidance.
	Capabilities sandbox.CapabilitySet
	// Progress, when non-nil, receives agent observability events: each repro
	// (and patch-prover) run is bracketed with KindAgentStarted/Finished and
	// its per-call tool activity is emitted as KindToolCall events, so a running
	// reproduce stage surfaces in `bugbot status` and the live pane via the
	// shared progress.AgentScope seam. Nil disables emission (no-op).
	Progress progress.EventSink
	// StatusNotes, when true, offers the reproducer and patch-prover agents the
	// status_note tool so the model can record an explicit working note in
	// addition to the automatic tool-call activity. Mirrors the funnel's
	// Scan.StatusNotes gate; off by default.
	StatusNotes bool
	// PackageSummary, when non-nil, returns the cached cartographer summary for a
	// repo-relative package directory. The reproducer pushes the finding's own
	// package summary into the task prompt AND exposes get_package_context, so the
	// agent can pull other packages' summaries (e.g. the repo's test package) to
	// learn the build/test layout without spending its tool budget rediscovering
	// it. nil disables both. The CLI wires it to store.GetPackageSummaries.
	PackageSummary func(ctx context.Context, pkg string) (summary string, found bool)
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
	if o.Network == "" {
		o.Network = "none"
	}
	if o.MaxParallel == 0 {
		o.MaxParallel = DefaultMaxParallel
	}
	if o.MaxParallel < 0 {
		o.MaxParallel = 1
	}
	if o.AgentLimits.MaxIterations == 0 {
		o.AgentLimits.MaxIterations = DefaultMaxIterations
	}
	if o.SandboxMaxExecs <= 0 {
		o.SandboxMaxExecs = DefaultSandboxMaxExecs
	}
	if o.TryMaxExecs <= 0 {
		o.TryMaxExecs = DefaultTryMaxExecs
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
	// pkgSummary returns the cached cartographer summary for a package directory,
	// or ok=false on a miss. Set from Options.PackageSummary; nil disables the
	// task-prompt summary push and the get_package_context tool.
	pkgSummary func(ctx context.Context, pkg string) (string, bool)
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
		Strategy:       resolved.DepStrategy,
		FetchSandbox:   sb,
		FetchImage:     resolved.Image,
		LocalMounts:    resolved.LocalMounts,
		HostToolchains: resolved.HostToolchains,
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
		pkgSummary:   resolved.PackageSummary,
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
	// WitnessOnly is true when Promoted is true but the detected ecosystem
	// could not provide an execution witness for the finding's target file
	// (see ecosystem.WitnessTable / witnessDemonstration): the finding was
	// genuinely demonstrated, but callers must record it via the existing
	// witness-only path (bugbot-w1bh) instead of a full Tier-1 promotion.
	WitnessOnly bool
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
func (r *Reproducer) Attempt(ctx context.Context, finding domain.Finding) (_ *Attempt, retErr error) {
	// Bracket the whole per-finding reproduce run (all revision attempts) as a
	// single "reproducer" agent in `bugbot status` / the live pane. The scope's
	// activity sink is wired into the runner so each turn's tool call (and any
	// status_note) shows what the reproducer is doing; the deferred Finish
	// settles the bracket with the accumulated token usage and final error.
	scope := progress.NewAgentScope(r.opts.Progress, progress.RoleReproducer, finding.Title).Start()
	start := time.Now()
	var usage llm.Usage
	defer func() {
		scope.Finish(usage.InputTokens+usage.OutputTokens, time.Since(start), retErr)
	}()

	// iterWS is the per-Attempt iteration workspace the agent builds its
	// candidate in (see iterationWorkspace doc): write_repro_file writes into
	// it, `workspace exec` executes against it, and its tracked files are
	// merged into the submitted plan below. It stays unmaterialized
	// (path == "") if the agent never uses the workspace tools, so an
	// Attempt that never iterates pays zero extra disk cost. Removed
	// unconditionally when Attempt returns,
	// so the official clean-room verdict below (execute()) never runs against
	// anything the iteration left behind.
	iterWS := &iterationWorkspace{}
	defer func() { _ = iterWS.cleanup() }()

	runner, err := r.newRunner(ctx, ingest.DetectLanguage(finding.File), r.buildSystems, scope, iterWS)
	if err != nil {
		return nil, fmt.Errorf("repro: build agent runner: %w", err)
	}

	att := &Attempt{FindingID: finding.ID}

	// feedback carries the prior run's output back into the next plan request so
	// the agent can correct a non-demonstrating repro. Empty on the first pass.
	var feedback string

	// Look up the finding's own package summary once and push it into every plan
	// request so the agent starts oriented on the buggy code's package instead of
	// rediscovering it from files. A miss (no cached summary, or a repo-root file)
	// yields an empty string, which buildTask omits.
	var pkgSummary string
	if r.pkgSummary != nil && finding.File != "" {
		if s, ok := r.pkgSummary(ctx, path.Dir(finding.File)); ok {
			pkgSummary = s
		}
	}

	for i := 0; i < r.opts.MaxAttempts; i++ {
		roundStart := time.Now()
		att.Attempts = i + 1

		plan, u, perr := r.planFor(ctx, runner, finding, pkgSummary, feedback)
		usage.InputTokens += u.InputTokens
		usage.OutputTokens += u.OutputTokens
		if perr != nil {
			// An unparseable or schema-violating plan from the agent is a
			// recoverable model hiccup, not an infrastructure failure: treat it
			// like a structurally invalid plan (feed the error back and revise)
			// so a bad revision round never discards a prior round's honest
			// verdict by aborting the whole finding. Genuine infra failures (LLM
			// transport, context cancellation, sandbox launch) are returned
			// unwrapped by RunJSON and still abort here.
			if errors.Is(perr, agent.ErrUnparseableOutput) {
				feedback = fmt.Sprintf("Your previous response was not a usable repro plan: %s. "+
					"Return ONLY a JSON object with the repro files and the cmd to run them.", perr)
				att.Reason = "unparseable plan: " + perr.Error()
				scope.EmitEvent(progress.Event{
					Kind:    progress.KindReproAttempt,
					Attempt: att.Attempts, MaxAttempts: r.opts.MaxAttempts,
					Verdict: "unparseable_plan", Duration: time.Since(roundStart),
				})
				continue
			}
			// Genuine infra failure (not recoverable): abort without a
			// KindReproAttempt — the round never reached a verdict, so there is
			// nothing to report as a round outcome.
			return nil, fmt.Errorf("repro: plan finding %s: %w", finding.ID, perr)
		}
		// The workspace is the proof: every file the agent wrote via
		// write_repro_file (and did not delete) is folded into the submitted
		// plan, with any inline plan.Files entries applied on top. This makes
		// self-containment structural — the clean-room verdict re-runs exactly
		// what iteration ran — instead of relying on the agent to retranscribe
		// file contents into the plan JSON verbatim.
		plan.Files = iterWS.mergedFiles(plan.Files)
		if verr := validatePlan(plan, r.repoDir); verr != nil {
			// A structurally invalid plan is treated like a non-demonstrating
			// attempt: feed the problem back and try again.
			feedback = fmt.Sprintf("Your previous plan was invalid: %s. Provide files and a cmd (expect is optional but recommended).", verr)
			att.Reason = "invalid plan: " + verr.Error()
			scope.EmitEvent(progress.Event{
				Kind:    progress.KindReproAttempt,
				Attempt: att.Attempts, MaxAttempts: r.opts.MaxAttempts,
				Verdict: "invalid_plan", Duration: time.Since(roundStart),
			})
			continue
		}
		// Pre-launch capability gate (bugbot-14g0 fix B, acceptance 3): reject
		// a plan whose cmd requires an ecosystem the probed CapabilitySet
		// reports unavailable, BEFORE any sandbox launch. Feedback names the
		// missing toolchain and the available alternatives so the agent
		// revises toward something this image can actually run, instead of
		// receiving a bare environment_error for a gap it already knows
		// about (bugbot-qb4r: an agent that hits environment_error blind
		// routes around it with a non-behavioral substitute).
		if msg := rejectUnavailableEcosystemPlan(plan, r.capabilities); msg != "" {
			feedback = msg
			att.Reason = "blocked_toolchain: " + msg
			scope.EmitEvent(progress.Event{
				Kind:    progress.KindReproAttempt,
				Attempt: att.Attempts, MaxAttempts: r.opts.MaxAttempts,
				Verdict: "blocked_toolchain", Duration: time.Since(roundStart),
			})
			continue
		}

		// bugbot-qb4r layer (a): the cheap static plan gate. Runs BEFORE any
		// sandbox execution — a plan whose submitted test files never reach
		// finding.File through an executable edge (import/require/#include/
		// use, or same-package colocation) is rejected here, without paying
		// for a sandbox run that could never be a genuine demonstration
		// (grep-tests, import-absence lint checks, and transliterations all
		// stop here). Ecosystems without a static rule (bazel, unknown) are
		// permissive; layer (b) below (witnessDemonstration) still applies
		// to whatever DOES execute.
		ecoName := detectEcosystem(plan.Cmd).name
		if reason, detail := ClassifyTargetExecution(plan.Files, finding.File, ecoName); reason != "" {
			gateVerdict := verdict{reason: reason, summary: detail, ecosystem: ecoName}
			att.Reason = string(reason)
			scope.EmitEvent(progress.Event{
				Kind:    progress.KindReproAttempt,
				Attempt: att.Attempts, MaxAttempts: r.opts.MaxAttempts,
				Verdict: string(reason), Duration: time.Since(roundStart),
			})
			feedback = gateVerdict.feedback(plan)
			continue
		}

		res, serr := r.execute(ctx, plan)
		if serr != nil {
			// Same rule as the planFor infra-failure above: a sandbox launch
			// failure aborts the whole attempt with no KindReproAttempt for
			// this round.
			return nil, fmt.Errorf("repro: execute finding %s: %w", finding.ID, serr)
		}

		verdict := interpret(res, plan.Cmd)
		if verdict.demonstrated {
			// bugbot-qb4r layer (b): the execution witness. Only meaningful
			// once interpret() has already found ran-evidence; this either
			// leaves the verdict untouched (witness found), downgrades it to
			// the non-promoting target_not_executed reason (ecosystem can
			// witness but didn't), or marks witnessOnly (ecosystem cannot
			// witness at all — repro-as-evidence, not repro-as-promotion).
			verdict = witnessDemonstration(verdict, combinedOutput(res), finding.File)
		}
		att.Output = verdict.summary

		roundVerdict := string(verdict.reason)
		if verdict.demonstrated {
			roundVerdict = "demonstrated"
		}
		// Emitted before writeArtifacts by design: the round's verdict is
		// final at this point (interpret() already ran), and artifact writing
		// is a side effect of a "demonstrated" verdict, not part of it — a
		// failure below aborts the whole Attempt with an error, independent of
		// whether this event was observed.
		scope.EmitEvent(progress.Event{
			Kind:    progress.KindReproAttempt,
			Attempt: att.Attempts, MaxAttempts: r.opts.MaxAttempts,
			Verdict: roundVerdict, Duration: time.Since(roundStart),
		})

		if verdict.demonstrated {
			path, werr := writeArtifacts(r.opts.ArtifactDir, finding, plan, res, r.opts.Image, r.opts.Network)
			if werr != nil {
				return nil, fmt.Errorf("repro: write artifacts for finding %s: %w", finding.ID, werr)
			}
			att.Promoted = true
			att.ArtifactPath = path
			att.Plan = plan
			att.Reason = ""
			att.WitnessOnly = verdict.witnessOnly
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
// refines that guidance for C/C++ (cmake > meson > generic fallback). scope is
// the run's observability handle: its activity sink is wired so the agent's
// per-turn tool calls surface live, and when StatusNotes is enabled the agent
// also gets the status_note tool routed to the same sink.
func (r *Reproducer) newRunner(ctx context.Context, lang ingest.Language, systems []ingest.BuildSystem, scope progress.AgentScope, iterWS *iterationWorkspace) (*agent.Runner, error) {
	tools, err := readOnlyTools(r.repoDir)
	if err != nil {
		return nil, err
	}
	tools = append(tools, r.nav.Tools()...)
	if r.opts.StatusNotes {
		tools = append(tools, agent.NewStatusNoteTool(toolActivitySink(scope)))
	}
	// get_package_context lets the agent pull any package's cartographer summary
	// (e.g. the repo's test package) to learn the build/test layout cheaply,
	// mirroring the finder. ctx is the per-attempt context — the runner lives only
	// within this Attempt. Omitted when no summary provider is wired.
	if r.pkgSummary != nil {
		tools = append(tools, agent.NewPackageContextTool(func(pkg string) (string, bool, error) {
			s, ok := r.pkgSummary(ctx, pkg)
			return s, ok, nil
		}))
	}
	// run_tests lets the agent exercise the repo's EXISTING test suite in the
	// sandbox to confirm the toolchain and learn the test layout before writing
	// its repro plan. Omitted when no build system is detectable.
	baseCmd := detectSuiteCmdFor(r.repoDir, r.buildSystems)
	if len(baseCmd) > 0 {
		// onExec is nil: per-turn run_tests activity already surfaces via the
		// runner's activity sink; the reproducer keeps no aggregate sandbox-exec
		// counters (unlike the funnel), so there is nothing to accumulate.
		tools = append(tools, agent.NewRunTestsTool(r.sb, r.repoDir, baseCmd, r.opts.SandboxMaxExecs, r.deps.ROMounts, r.deps.Env, r.deps.SetupCmds, nil))
	}
	// The workspace tool set (write_repro_file, delete_repro_file, workspace)
	// lets the agent build, run, and observe a candidate repro interactively
	// in a persistent per-attempt workspace before committing to its final
	// plan (bugbot-bkz1, bugbot-hu59, bugbot-jto7); the files it writes are
	// submitted with the plan automatically. Only wired when the sandbox
	// backend can pre-materialize a caller-owned workspace
	// (workspaceMaterializer): *sandbox.CLI always can; a bare sandbox.Mock in
	// a test that doesn't script iteration simply omits the tools, matching
	// run_tests' no-build-system omission above.
	workspaceWired := false
	if mat, ok := r.sb.(workspaceMaterializer); ok {
		workspaceWired = true
		tools = append(tools,
			NewWriteReproFileTool(r.repoDir, mat.MaterializeWorkspace, iterWS),
			NewDeleteReproFileTool(iterWS),
			NewWorkspaceTool(r.sb, r.repoDir, r.opts.Image, r.opts.Timeout,
				r.deps.ROMounts, r.deps.Env, r.deps.SetupCmds, mat.MaterializeWorkspace, iterWS, r.opts.TryMaxExecs))
	}
	var opts []agent.Option
	opts = append(opts, agent.WithLimits(r.opts.AgentLimits))
	if r.opts.TranscriptDir != "" {
		opts = append(opts, agent.WithTranscriptDir(r.opts.TranscriptDir))
	}
	opts = append(opts, agent.WithActivitySink(toolActivitySink(scope)))
	prompt := systemPrompt(lang, systems, r.capabilities)
	if r.pkgSummary != nil {
		prompt += pkgContextGuidance
	}
	if len(baseCmd) > 0 {
		prompt += runTestsGuidance(r.opts.SandboxMaxExecs)
	}
	if workspaceWired {
		prompt += workspaceGuidance(r.opts.TryMaxExecs)
	}
	if slices.Contains(r.buildSystems, ingest.BuildSystemBazel) {
		prompt += bazelGuidance()
	}
	prompt += reproSandboxGuidance(r.deps.ROMounts)
	return agent.NewRunner(r.client, tools, prompt, opts...), nil
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
// policy. network is intentionally left unset so the run inherits the
// sandbox's configured default (sandbox.network in bugbot.yaml, applied via
// the CLI's sandboxRunOpts) — see buildSpec's doc comment. Replay (see
// replay.go) calls buildSpec directly with an explicit "none" instead, since
// it re-runs a saved bundle rather than an agent-proposed plan.
func (r *Reproducer) execute(ctx context.Context, plan *Plan) (sandbox.Result, error) {
	return r.sb.Exec(ctx, buildSpec(r.repoDir, plan, r.opts.Image, "", r.opts.Timeout, r.deps))
}

// buildSpec renders plan into a sandbox.Spec against repoDir, applying the
// same dependency-mount plumbing (ROMounts/Env/SetupCmds) and structured-
// output rewrite every repro sandbox run needs, regardless of whether the
// plan came from a live reproducer agent (execute, network deliberately
// left "" to inherit the backend's configured default) or a saved bundle
// being replayed (Replay, network forced to "none" — see replay.go). This is
// the ONE workspace-reconstruction path both callers share, so a replay
// genuinely re-runs what the official Attempt path would have run, not a
// parallel reimplementation of it.
func buildSpec(repoDir string, plan *Plan, image, network string, timeout time.Duration, deps sandbox.Resolution) sandbox.Spec {
	files := make(map[string][]byte, len(plan.Files))
	for path, content := range plan.Files {
		files[path] = []byte(content)
	}
	// normalizeCmdForStructuredOutput rewrites a direct `go test`/`pytest`
	// invocation to ask the runner for machine-readable output (see
	// runnerevents.go), so interpret() can classify off positive test-level
	// evidence instead of scanning free-form text. plan.Cmd itself is left
	// untouched: it is still what ecosystem detection and agent-facing
	// feedback use, and the rewrite is a harness-owned implementation detail
	// the agent never needs to see.
	cmd, captures := normalizeCmdForStructuredOutput(plan.Cmd)
	return sandbox.Spec{
		RepoDir:    repoDir,
		Cmd:        cmd,
		Image:      image,
		Network:    network,
		Timeout:    timeout,
		WriteFiles: files,
		// Dependency strategy: mount a module cache read-only and/or set GOFLAGS
		// so the network-none run can resolve external modules. SetupCmds installs
		// non-Go ecosystem packages from the mounted cache before Cmd runs.
		ROMounts:     deps.ROMounts,
		Env:          deps.Env,
		SetupCmds:    deps.SetupCmds,
		CaptureFiles: captures,
	}
}

// bareShellOps is the set of shell control tokens that mean nothing to a
// sandbox that runs Cmd as raw argv. If any element of p.Cmd is exactly one of
// these strings, the agent emitted shell syntax the sandbox cannot interpret
// (e.g. ["cmake", "...", "&&", "cmake", "--build", "..."] passes "&&" as a
// literal argument to the first cmake). The fix is to wrap the whole command
// in `bash -c "..."` so the shell parses the operators — and a correctly
// wrapped plan keeps these tokens INSIDE one quoted argv element, so it must
// not be flagged. Match whole argv elements only; do not substring-scan.
var bareShellOps = map[string]struct{}{
	"&&":   {},
	"||":   {},
	"|":    {},
	";":    {},
	"2>&1": {},
	">":    {},
	">>":   {},
	"<":    {},
	"&":    {},
	"cd":   {},
}

// isBareShellOp reports whether arg is a shell control token that must not
// appear as a standalone element of plan.Cmd. Matching is exact-string only:
// an arg like "&&&&" or "echo &&" is fine because the operator is not the whole
// element. This keeps a properly bash-wrapped plan (with the operators inside
// one quoted string) untouched.
func isBareShellOp(arg string) bool {
	_, ok := bareShellOps[arg]
	return ok
}

// validatePlan rejects structurally unusable plans before spending a sandbox
// run on them. A failure here is recoverable: Attempt feeds the message back to
// the agent and revises, so the checks below double as corrective guidance.
//
// The per-file and per-cmd rules are shared verbatim with the workspace tools'
// per-call validation (validateReproFilePath in write_repro_file,
// validateReproCmd in the workspace tool's exec applet) so iteration teaches
// the agent the exact same
// contract its final plan must satisfy — a file or command that cleared the
// tools' gate is guaranteed to clear submission too. Attempt merges the
// workspace registry into p.Files before calling this, so a cmd-only plan
// after write_repro_file calls carries those files here.
func validatePlan(p *Plan, repoDir string) error {
	if len(p.Files) == 0 {
		return errors.New(`no repro files: the plan must inject at least one NEW file that demonstrates the bug. ` +
			`Either write your repro into the workspace with write_repro_file (every file you write is ` +
			`submitted with the plan automatically), or set the plan's "files" to a JSON object mapping ` +
			`repo-relative paths to FULL file contents, e.g. {"repro_test.go": "package ..."}`)
	}
	if len(p.Cmd) == 0 {
		return errors.New(`no command: set "cmd" to the argv array that builds and runs your repro, ` +
			`e.g. ["go","test","-timeout","60s","-run","TestX","./pkg"]`)
	}
	for fpath := range p.Files {
		if err := validateReproFilePath(fpath, repoDir); err != nil {
			return err
		}
	}
	return validateReproCmd(p.Cmd)
}

// gatedEcosystems is the ordered set of ecosystem.BaseMode-gated ecosystems
// (see ecosystem.InferFromCmd/BaseMode) consulted for both the "available
// alternatives" list in rejectUnavailableEcosystemPlan and any future
// diagnostic that wants to enumerate what a probed image can run.
var gatedEcosystems = []ecosystem.Ecosystem{ecosystem.EcosystemJS, ecosystem.EcosystemPython, ecosystem.EcosystemRust}

// rejectUnavailableEcosystemPlan checks plan.Cmd against caps and returns
// revision feedback naming the missing toolchain and the available
// alternatives, or "" when the plan should proceed to a sandbox launch —
// either ecosystem.InferFromCmd found no gated ecosystem requirement (e.g.
// "go test", "make", or any command this function does not recognize), caps
// is nil (no probe available), or the required ecosystem IS available.
//
// This is the pre-launch half of bugbot-14g0's capability gate (fix B,
// acceptance 3): promote.go's gateEcosystem gates on the FINDING's inferred
// ecosystem before a claim even happens; this gates on the PLAN's actual cmd
// right before the sandbox would launch it, catching a plan that (for
// whatever reason) targets a different toolchain than the finding's file
// extension implied.
func rejectUnavailableEcosystemPlan(p *Plan, caps sandbox.CapabilitySet) string {
	if caps == nil {
		return ""
	}
	eco := ecosystem.InferFromCmd(p.Cmd)
	if eco == "" {
		return ""
	}
	mode := ecosystem.BaseMode(eco)
	if mode == "" || caps.Available(eco, mode) {
		return ""
	}

	var alts []string
	for _, alt := range gatedEcosystems {
		if alt == eco {
			continue
		}
		if m := ecosystem.BaseMode(alt); m != "" && caps.Available(alt, m) {
			alts = append(alts, alt)
		}
	}

	if len(alts) == 0 {
		return fmt.Sprintf(
			"Your plan's command requires %s, which this sandbox does not have, and no alternative "+
				"toolchain (js/python/rust) is available either. Do NOT substitute a non-behavioral test in a "+
				"different language or grep for the pattern — that does not demonstrate the bug. Set cmd to a "+
				"command this sandbox can actually run, or omit a cmd change and report the environment gap in expect.",
			eco,
		)
	}
	return fmt.Sprintf(
		"Your plan's command requires %s, which this sandbox does not have. Available alternative "+
			"toolchains in this sandbox: %s. If the bug is only observable via %s, do NOT substitute a "+
			"non-behavioral test in another language — revise cmd to use one of the available toolchains only "+
			"if the bug is genuinely reproducible that way; otherwise report the environment gap in expect.",
		eco, strings.Join(alts, ", "), eco,
	)
}

// validateReproFilePath is the per-file structural gate shared by validatePlan
// (every key of the final submitted plan's files) and the write_repro_file
// tool (each interactive write). Holding the actual rules in one place keeps
// the two callers from drifting: a path accepted during iteration is accepted
// at submission.
//
// repoDir is the host path to the target repository. When non-empty, fpath is
// stat-checked against it: a file that would overwrite an existing repo file
// is rejected as a recoverable revision before any sandbox run (bugbot-ndlw).
// New files — paths that do not yet exist on disk — are allowed anywhere
// workspace-relative.
func validateReproFilePath(fpath, repoDir string) error {
	// Injected file keys must be workspace-relative: an absolute or
	// escaping path (e.g. "/tmp/repro_test.cpp") is rejected by the sandbox
	// at write time, which would otherwise abort the whole attempt with a
	// hard infrastructure error instead of a recoverable revision. Catch it
	// here using the sandbox's own rule so the agent is told to fix it.
	if err := sandbox.ValidateWorkspacePath(fpath); err != nil {
		return fmt.Errorf("file %q must be a workspace-relative path inside the repo "+
			"(no leading %q, no %q), e.g. %q: %w", fpath, "/", "..", "repro_test.cpp", err)
	}
	// Reject writes that would overwrite an existing repo file. The agent
	// must only write NEW files; overwriting production sources, go.mod,
	// fixtures, or existing tests causes the subsequent run to demonstrate
	// a self-inflicted change rather than the claimed bug (bugbot-ndlw).
	if repoDir != "" {
		if _, err := os.Stat(filepath.Join(repoDir, fpath)); err == nil {
			return fmt.Errorf("file %q already exists in the repository; "+
				"write NEW files only; modify nothing that exists in the repo", fpath)
		}
	}
	return nil
}

// validateReproCmd is the per-command structural gate shared by validatePlan
// (the final submitted plan's cmd) and the workspace tool's exec applet
// (each interactive run). Holding the actual rules in one place keeps the two callers from
// drifting: a command accepted during iteration is accepted at submission.
func validateReproCmd(cmd []string) error {
	for _, arg := range cmd {
		// Bare shell control operators are meaningless to a sandbox that runs
		// Cmd as raw argv. A reproducer that emits, say,
		// ["cmake", "...", "&&", "cmake", "--build", "..."] passes "&&" as a
		// literal argument to the first cmake, which errors out without ever
		// reaching the second command and wastes a sandbox run. The fix is
		// structural: wrap the whole command in `bash -c "..."` so the shell
		// parses the operators. A correctly wrapped plan
		// ["bash","-c","cmake ... && cmake --build ..."] keeps the operators
		// inside ONE quoted string, so it must NOT be flagged — we match whole
		// argv elements only, never substrings.
		if isBareShellOp(arg) {
			return fmt.Errorf("cmd contains a bare shell operator %q as a separate argv element; "+
				"the sandbox runs Cmd as raw argv (no shell), so %q is passed as a literal argument to the preceding command. "+
				"Wrap the entire command in bash: set cmd to [\"bash\",\"-c\",\"<full command as a single string>\"] "+
				"(e.g. [\"bash\",\"-c\",\"cmake ... && cmake --build ... && cd build && ./tests/x\"])", arg, arg)
		}
	}
	// A go test repro must self-terminate: without -timeout a hung test blocks
	// until the sandbox idle watchdog kills it, wasting the attempt on a useless
	// "timeout" verdict (bugbot-opq). Require the flag so the test binary kills
	// itself first. Scoped to "go test" (the demonstrated failure class);
	// unwrapShell handles a bash -c wrapper.
	if eff := unwrapShell(cmd); len(eff) >= 2 &&
		strings.ToLower(eff[0]) == "go" && strings.ToLower(eff[1]) == "test" &&
		!hasCmdFlag(eff, "-timeout") {
		return fmt.Errorf("go test repro must include a -timeout flag so a hung test self-terminates " +
			"before the sandbox idle watchdog kills it (which yields a useless timeout verdict); " +
			"add e.g. -timeout 60s: [\"go\",\"test\",\"-timeout\",\"60s\",\"-run\",\"TestX\",\"./...\"]")
	}
	return nil
}

// hasCmdFlag reports whether argv contains the flag name in either the
// "-flag" / "-flag value" form or the "-flag=value" form.
func hasCmdFlag(argv []string, name string) bool {
	for _, a := range argv {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

// toolActivitySink builds the func(agent.ToolActivity) callback for
// agent.WithActivitySink and agent.NewStatusNoteTool, routing each structured
// ToolActivity through scope.EmitToolCall so it surfaces as a KindToolCall
// progress event without coupling the repro package to agent's types at the
// call sites.
func toolActivitySink(scope progress.AgentScope) func(agent.ToolActivity) {
	return func(act agent.ToolActivity) {
		scope.EmitToolCall(act.Phase, act.Tool, act.File, act.Line, act.EndLine, act.Symbol, act.Pattern, act.Count, act.Err)
	}
}

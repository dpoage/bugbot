package repro

// patch.go implements the Patch-prover stage of the Bugbot pipeline.
//
// After a bug is demonstrated by a failing sandboxed test (Tier-1 repro), the
// patch-prover attempts to go one step further: it asks an agent to produce a
// MINIMAL candidate fix, then proves (a) the failing repro test now PASSES with
// the fix applied, and (b) the full suite stays green.
//
// On success the finding is promoted to Tier-0 ("fix-witnessed") and the diff
// is stored as FixPatch — a witness / starting point, NOT a reviewed fix.
//
// On exhaustion (all attempts failed to produce a plausible minimal fix) the
// finding is flagged NeedsHuman: a fix-refusing bug is often a misdiagnosed
// one.
//
// Exit-code semantics here are INVERTED relative to the repro stage: the repro
// stage expects exit != 0 (test failure proves the bug); the patch-prover
// expects exit == 0 (test pass proves the fix works).  The helper patchVerdict
// encodes this explicitly and must not reuse interpret().
//
// Validation seam — a PatchPlan is rejected when any patched path:
//   (a) collides with a repro-plan file (protecting the repro witness),
//   (b) matches *_test.go (fix must not modify tests),
//   (c) escapes the repo root (path traversal guard),
//   (d) does not exist in the repo (minimal fix edits existing code only;
//       adding new source files is out of scope because it changes the module
//       surface area in ways that are hard to review automatically).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// patchMaxDiffBytes is the maximum size of the unified diff text stored in
// FixPatch.  Diffs beyond this are truncated with a marker.
const patchMaxDiffBytes = 32 * 1024 // 32 KB

// patchDefaultMaxAttempts is the default number of fix plans tried when
// PatchProver.maxAttempts is zero.
const patchDefaultMaxAttempts = 3

// isTestPath reports whether a cleaned repo-relative path looks like a test
// file in any mainstream language, or lives under a conventional test
// directory. This is defense-in-depth: the load-bearing invariant is the
// repro-file collision guard (language-independent); these patterns stop a
// proposed "fix" from rewriting the broader test surface.
func isTestPath(clean string) bool {
	slashed := filepath.ToSlash(clean)
	for _, seg := range strings.Split(slashed, "/") {
		switch seg {
		case "test", "tests", "__tests__", "spec", "testdata":
			return true
		}
	}
	base := strings.ToLower(filepath.Base(slashed))
	switch {
	case strings.HasSuffix(base, "_test.go"),
		strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"),
		strings.HasSuffix(base, "_test.py"),
		strings.HasSuffix(base, "_spec.rb"):
		return true
	}
	// foo.test.ts / foo.spec.jsx style (JS/TS ecosystems).
	if parts := strings.Split(base, "."); len(parts) >= 3 {
		switch parts[len(parts)-2] {
		case "test", "spec":
			return true
		}
	}
	// JVM/.NET conventions: FooTest.java, FooTests.cs.
	switch {
	case strings.HasSuffix(base, "test.java"), strings.HasSuffix(base, "tests.java"),
		strings.HasSuffix(base, "test.cs"), strings.HasSuffix(base, "tests.cs"):
		return true
	}
	return false
}

// patchSystemPrompt instructs the patch-prover agent.
const patchSystemPrompt = `You are Bugbot's patch-prover agent. A bug has been demonstrated by a
failing sandbox test. Your job is to produce a MINIMAL candidate fix that makes
the failing repro test pass while keeping the full test suite green.

You have read-only tools (read_file, list_dir, grep) rooted at the target
repository. Investigate the bug location and the repro test thoroughly before
proposing a fix.

Hard requirements for the fix:
- Produce the SMALLEST change that makes the repro test pass.
- Provide the COMPLETE new contents of each file you change.
- Do NOT modify or delete any test file (in any language or test directory).
- Do NOT add new files — only edit files that already exist in the repository.
  (New files change the module surface area and are out of scope for automated
  witnessing.)
- Do NOT add new external dependencies.
- Do NOT refactor unrelated code.

Return a patch plan with the new file contents and a short human-readable
summary of what was changed and why.`

// patchSchema is the JSON schema for the patch-prover agent's plan output.
var patchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "files": {
      "type": "object",
      "description": "Files to replace, keyed by workspace-relative path. Must contain at least one entry. Each value is the COMPLETE new content of that file.",
      "additionalProperties": {"type": "string"},
      "minProperties": 1
    },
    "summary": {
      "type": "string",
      "minLength": 1,
      "description": "Short human-readable description of what was changed and why."
    }
  },
  "required": ["files", "summary"],
  "additionalProperties": false
}`)

// PatchPlan is the patch-prover agent's proposal.
type PatchPlan struct {
	// Files are the files to replace, keyed by workspace-relative path.
	// Values are the complete new content of each file.
	Files map[string]string `json:"files"`
	// Summary is a short description of the change.
	Summary string `json:"summary"`
}

// PatchProver runs the patch-prover stage for a single finding.
type PatchProver struct {
	client      llm.Client
	sb          sandbox.Sandbox
	repoDir     string
	maxAttempts int
	timeout     time.Duration
	image       string
	artifactDir string
	agentLimits agent.Limits
	// transcriptDir, when non-empty, makes each patch-prover agent auto-save
	// its run transcript there. Mirrors Reproducer.opts.TranscriptDir so the
	// fix-witness step is observable end-to-end on the same artifact path.
	transcriptDir string
	// suiteCmd runs the full test suite for the suite-green witness. Empty
	// means "detect from repo markers"; if detection also fails the prover
	// skips rather than guessing — a wrong suite command would silently
	// weaken the witness.
	suiteCmd []string
	// depMounts / depEnv / setupCmds carry the resolved dependency strategy
	// (read-only module-cache mount and/or GOFLAGS and/or pre-Cmd setup
	// commands) so the patch-prover's network-none runs resolve external modules
	// identically to the repro run. The one-time online prefetch is already done
	// by PromoteAll before the prover runs.
	depMounts []sandbox.ROMount
	depEnv    []string
	setupCmds [][]string
}

// detectSuiteCmd infers the full-suite test command from well-known repo
// marker files. Returns nil when the toolchain cannot be identified — callers
// must skip rather than guess when nil is returned.
//
// Priority order matches ingest.DetectBuildSystems:
//
//  1. Bazel → ["bazel", "test", "//..."]
//  2. GoWorkspace → ["go", "test", "./..."] only when a root go.mod also
//     exists; without go.mod the workspace spans multiple modules and a single
//     ./... invocation at the root is wrong (per-module invocations are out of
//     scope).
//  3. JSWorkspace (pnpm-workspace.yaml) → ["pnpm", "test"]; turbo/nx →
//     ["npm", "test"] (closest sensible default; project-specific config can
//     override via suite_cmd).
//  4. GoModule → ["go", "test", "./..."]
//  5. Cargo → ["cargo", "test"]
//  6. NPM → ["npm", "test"]
//  7. Python (pyproject.toml / setup.py) → ["python", "-m", "pytest"]
//
// The existing single-marker behaviour (go.mod, Cargo.toml, package.json,
// pyproject.toml, setup.py) is preserved exactly for backward compatibility.
func detectSuiteCmd(repoDir string) []string {
	systems := ingest.DetectBuildSystems(repoDir)
	for _, sys := range systems {
		switch sys {
		case ingest.BuildSystemBazel:
			return []string{"bazel", "test", "//..."}

		case ingest.BuildSystemGoWorkspace:
			// A go.work-only repo spans multiple modules; `go test ./...` at
			// the workspace root only works when there is also a root go.mod
			// (i.e. there is a package in the root module). Without a root
			// go.mod the correct approach is per-module invocations, which is
			// out of scope — fall through to let a lower-priority system match.
			if _, err := os.Stat(filepath.Join(repoDir, "go.mod")); err == nil {
				return []string{"go", "test", "./..."}
			}
			// No root go.mod: skip; lower-priority systems may still match.

		case ingest.BuildSystemJSWorkspace:
			// pnpm workspaces have a canonical `pnpm test` command.
			if _, err := os.Stat(filepath.Join(repoDir, "pnpm-workspace.yaml")); err == nil {
				return []string{"pnpm", "test"}
			}
			// turbo.json / nx.json: fall back to npm test as the closest
			// portable default; projects that need `turbo run test` or
			// `nx run-many` should configure suite_cmd explicitly.
			return []string{"npm", "test"}

		case ingest.BuildSystemGoModule:
			return []string{"go", "test", "./..."}

		case ingest.BuildSystemCargo:
			return []string{"cargo", "test"}

		case ingest.BuildSystemNPM:
			return []string{"npm", "test"}

		case ingest.BuildSystemPython:
			return []string{"python", "-m", "pytest"}
		}
	}
	return nil
}

// Prove runs the patch-prover loop for a finding that was just promoted to T1.
// It either promotes to T0 (FixWitnessed) or records NeedsHuman on exhaustion.
func (p *PatchProver) Prove(ctx context.Context, st *store.Store, f store.Finding, att *Attempt) (patchOutcome, error) {
	maxAtt := p.maxAttempts
	if maxAtt <= 0 {
		maxAtt = patchDefaultMaxAttempts
	}

	suiteCmd := p.suiteCmd
	if len(suiteCmd) == 0 {
		suiteCmd = detectSuiteCmd(p.repoDir)
	}
	if len(suiteCmd) == 0 {
		// Unknown toolchain and no repro.suite_cmd configured: skip rather
		// than guess. A wrong suite command would silently weaken the
		// suite-green half of the witness.
		return patchOutcome{SkippedNoSuiteCmd: true}, nil
	}

	runner, err := p.newRunner()
	if err != nil {
		return patchOutcome{}, fmt.Errorf("patch-prover: build runner: %w", err)
	}

	var feedback string
	var lastFailure string

	for i := 0; i < maxAtt; i++ {
		plan, perr := p.planFor(ctx, runner, f, att, feedback)
		if perr != nil {
			return patchOutcome{}, fmt.Errorf("patch-prover: plan: %w", perr)
		}

		if verr := p.validatePatchPlan(plan, att.Plan); verr != nil {
			feedback = fmt.Sprintf("Your previous plan was rejected: %s. Revise it.", verr)
			lastFailure = "invalid plan: " + verr.Error()
			continue
		}

		// Build the merged WriteFiles: repro test files + patch files.
		writeFiles := mergedWriteFiles(att.Plan, plan)

		// Run 1: targeted — the repro command with patch applied.
		targetedRes, serr := p.execSandbox(ctx, att.Plan.Cmd, writeFiles)
		if serr != nil {
			return patchOutcome{}, fmt.Errorf("patch-prover: targeted sandbox: %w", serr)
		}

		// Patch verdict: exit 0 = SUCCESS (inverted from repro).
		// Thread the targeted command so the ecosystem classification
		// can be applied to the output (see bugbot-vig).
		tv := patchVerdict(targetedRes, att.Plan.Cmd)
		if tv.envFailure {
			// Infrastructure: stop attempts but do not flag
			// needs-human. We keep the error message distinctive
			// ("environment cannot run repro") so the prover's
			// failure reporting — and the human reviewer — can tell
			// this apart from a genuine fix-rejection.
			return patchOutcome{}, fmt.Errorf("patch-prover: environment cannot run repro in targeted run (ecosystem=%s): %s", tv.ecosystem, tv.summary)
		}
		if tv.fixRejected {
			out := trunc(combinedOutput(targetedRes), 600)
			feedback = fmt.Sprintf(
				"The fix was applied but the repro test still FAILS (exit %d).\n\nOutput:\n%s\n\nRevise the fix.",
				targetedRes.ExitCode, out)
			lastFailure = "repro test still fails with fix: " + trunc(combinedOutput(targetedRes), 200)
			continue
		}

		// Run 2: suite — the full suite command with the same files.
		suiteRes, serr := p.execSandbox(ctx, suiteCmd, writeFiles)
		if serr != nil {
			return patchOutcome{}, fmt.Errorf("patch-prover: suite sandbox: %w", serr)
		}

		// Suite run: same per-ecosystem classification, but the
		// command is the suite command (not the targeted repro
		// command). Both argv shapes are classified by
		// detectEcosystem; the env / toolchain / build marker
		// vocabulary is shared across both.
		sv := patchVerdict(suiteRes, suiteCmd)
		if sv.envFailure {
			// Infrastructure: stop attempts but do not flag
			// needs-human. Distinctive message — see the
			// targeted-run branch above.
			return patchOutcome{}, fmt.Errorf("patch-prover: environment cannot run repro in suite run (ecosystem=%s): %s", sv.ecosystem, sv.summary)
		}
		if sv.fixRejected {
			out := trunc(combinedOutput(suiteRes), 600)
			feedback = fmt.Sprintf(
				"The fix makes the repro test pass, but the FULL SUITE fails (exit %d).\n\nOutput:\n%s\n\nRevise the fix so the suite stays green.",
				suiteRes.ExitCode, out)
			lastFailure = "suite fails with fix: " + trunc(combinedOutput(suiteRes), 200)
			continue
		}

		// Both passes: witness confirmed. Compute the diff and persist.
		diffText, derr := computeDiff(p.repoDir, plan.Files)
		if derr != nil {
			// Diff is informational; don't abort on failure — store empty.
			// An error string here would be rendered inside a ```diff block in
			// the report, so it must NOT go into the column. The patched files
			// in the artifact bundle remain the authoritative witness.
			diffText = ""
		}

		// Write patch files into the existing artifact bundle.
		if werr := writePatchArtifacts(p.artifactDir, f.ID, plan, diffText); werr != nil {
			return patchOutcome{}, fmt.Errorf("patch-prover: write artifacts: %w", werr)
		}

		// Promote to T0.
		if perr := promoteToT0(ctx, st, f, diffText); perr != nil {
			return patchOutcome{}, fmt.Errorf("patch-prover: persist T0: %w", perr)
		}

		return patchOutcome{FixWitnessed: true}, nil
	}

	// Exhausted: flag needs-human.
	if perr := flagNeedsHuman(ctx, st, f, maxAtt, lastFailure); perr != nil {
		return patchOutcome{}, fmt.Errorf("patch-prover: persist needs-human: %w", perr)
	}
	return patchOutcome{NeedsHuman: true}, nil
}

// newRunner builds a read-only agent runner for the patch-prover role.
func (p *PatchProver) newRunner() (*agent.Runner, error) {
	tools, err := readOnlyTools(p.repoDir)
	if err != nil {
		return nil, err
	}
	var opts []agent.Option
	opts = append(opts, agent.WithLimits(p.agentLimits))
	if p.transcriptDir != "" {
		opts = append(opts, agent.WithTranscriptDir(p.transcriptDir))
	}
	return agent.NewRunner(p.client, tools, patchSystemPrompt, opts...), nil
}

// planFor asks the patch-prover agent for a fix plan.
func (p *PatchProver) planFor(ctx context.Context, runner *agent.Runner, f store.Finding, att *Attempt, feedback string) (*PatchPlan, error) {
	task := buildPatchTask(f, att, feedback)
	var plan PatchPlan
	if _, err := runner.RunJSON(ctx, task, patchSchema, &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// buildPatchTask renders the per-finding patch-prover task prompt.
func buildPatchTask(f store.Finding, att *Attempt, feedback string) string {
	var b strings.Builder
	b.WriteString("A bug has been confirmed by a failing sandboxed test. Produce a MINIMAL fix.\n\n")
	fmt.Fprintf(&b, "Title: %s\n", f.Title)
	if f.Severity != "" {
		fmt.Fprintf(&b, "Severity: %s\n", f.Severity)
	}
	if f.File != "" {
		fmt.Fprintf(&b, "Location: %s:%d\n", f.File, f.Line)
	}
	if f.Description != "" {
		fmt.Fprintf(&b, "\nDescription:\n%s\n", f.Description)
	}
	if f.Reasoning != "" {
		fmt.Fprintf(&b, "\nVerification reasoning:\n%s\n", f.Reasoning)
	}
	if att != nil && att.Plan != nil {
		b.WriteString("\n--- Repro plan ---\n")
		b.WriteString("Command: ")
		b.WriteString(strings.Join(att.Plan.Cmd, " "))
		b.WriteString("\n")
		b.WriteString("Expected failure: ")
		b.WriteString(att.Plan.Expect)
		b.WriteString("\n")
		b.WriteString("Repro test files (do NOT modify these):\n")
		for path := range att.Plan.Files {
			fmt.Fprintf(&b, "  %s\n", path)
		}
		if att.Output != "" {
			fmt.Fprintf(&b, "\nRepro output (failing):\n%s\n", trunc(att.Output, 800))
		}
	}
	if strings.TrimSpace(feedback) != "" {
		b.WriteString("\n--- Revision required ---\n")
		b.WriteString(feedback)
		b.WriteString("\nProduce a corrected plan.\n")
	}
	return b.String()
}

// validatePatchPlan validates a patch plan against the safety rules.
func (p *PatchProver) validatePatchPlan(plan *PatchPlan, reproPlan *Plan) error {
	if len(plan.Files) == 0 {
		return fmt.Errorf("no files in patch plan")
	}

	// Build a set of repro-plan file paths for collision detection.
	reproFiles := make(map[string]bool, len(reproPlan.Files))
	for path := range reproPlan.Files {
		reproFiles[filepath.Clean(path)] = true
	}

	for rel := range plan.Files {
		if rel == "" {
			return fmt.Errorf("empty file path in patch plan")
		}
		clean := filepath.Clean(rel)

		// (c) Escape guard: must not be absolute or traverse outside the repo.
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("path %q escapes the repository root", rel)
		}

		// (b) Test-file guard: fix must not touch test files (any language).
		if isTestPath(clean) {
			return fmt.Errorf("path %q is a test file; the fix must not modify or delete test files", rel)
		}

		// (a) Repro-collision guard: fix must not overwrite the repro witness.
		if reproFiles[clean] {
			return fmt.Errorf("path %q collides with a repro test file; the fix must not touch the repro witness", rel)
		}

		// (d) Existing-file guard: the minimal fix edits existing code only.
		// New files are out of scope (they change the module surface area).
		abs := filepath.Join(p.repoDir, clean)
		if _, err := os.Stat(abs); os.IsNotExist(err) {
			return fmt.Errorf("path %q does not exist in the repository; the minimal fix must only edit existing files (adding new files is out of scope)", rel)
		}
	}
	return nil
}

// execSandbox executes a command in the sandbox with the given write files.
func (p *PatchProver) execSandbox(ctx context.Context, cmd []string, writeFiles map[string][]byte) (sandbox.Result, error) {
	to := p.timeout
	if to <= 0 {
		to = DefaultTimeout
	}
	spec := sandbox.Spec{
		RepoDir:    p.repoDir,
		Cmd:        cmd,
		Image:      p.image,
		Network:    "none",
		Timeout:    to,
		WriteFiles: writeFiles,
		ROMounts:   p.depMounts,
		Env:        p.depEnv,
		SetupCmds:  p.setupCmds,
	}
	return p.sb.Exec(ctx, spec)
}

// patchVerdictResult is the interpretation of a sandbox run in the
// patch-prover context.  Exit-code semantics are inverted relative to
// the repro stage: exit 0 means the test passed (fix proved); non-zero
// means it failed.
type patchVerdictResult struct {
	// passed is true ONLY when exit == 0 (and the run was not
	// stopped by a timeout). Anything else leaves passed == false.
	passed bool
	// envFailure reports that the run never produced a real
	// pass/fail signal — the environment, toolchain, or a build
	// step broke before the test could run.  The prover must
	// distinguish this from "fix rejected" (the test ran and still
	// failed) so the human reviewer is not told a fix is
	// misdiagnosed when the sandbox itself could not run it.
	envFailure bool
	// summary is a short human-readable digest of the run's output.
	summary string
	// ecosystem is the detected ecosystem name (e.g. "go",
	// "python", "unknown"). Mirrors verdict.ecosystem so
	// patch.go's prover loop can disambiguate env-failure from
	// fix-rejected without re-running detection.
	ecosystem string
	// fixRejected is true when the test ran and FAILED with the
	// patch applied — i.e. the test still does not pass, so the
	// proposed fix is rejected.  Mutually exclusive with passed
	// and with envFailure.
	fixRejected bool
}

// patchVerdict interprets a sandbox result in the patch-prover
// context.
//
// The cmd argument is the argv that produced res (typically
// att.Plan.Cmd for the targeted run, or suiteCmd for the suite
// run).  It feeds the same detectEcosystem table the repro stage
// uses, so the env-failure / toolchain / build classification
// stays in lockstep across both stages — that is the seam that
// makes "the sandbox could not run the repro" reportable as a
// separate category from "the fix is wrong".
//
// Rules (note the exit-code inversion vs interpret()):
//   - Exit 0 and not timed-out: PASS — the fix works.
//   - TimedOut / exit 125/126/127 / env markers / toolchain
//     markers / build markers: envFailure — the test never ran,
//     so we cannot say the fix works or not.
//   - Non-zero exit with positive ran-evidence (the test ran and
//     FAILED): fixRejected — the proposed fix did not fix it.
//   - Non-zero exit without markers: fixRejected — the run
//     issued a non-zero exit, and since we cannot rule out
//     env/toolchain/build failure from a non-zero by itself, the
//     default is the more conservative "fix rejected" classification
//     so the agent is told to revise the fix.
func patchVerdict(res sandbox.Result, cmd []string) patchVerdictResult {
	out := combinedOutput(res)
	eco := detectEcosystem(cmd)

	if res.TimedOut {
		return patchVerdictResult{envFailure: true, summary: trunc(out, 400), ecosystem: eco.name}
	}
	if res.ExitCode == 125 || res.ExitCode == 126 || res.ExitCode == 127 {
		return patchVerdictResult{envFailure: true, summary: trunc(out, 400), ecosystem: eco.name}
	}
	if res.ExitCode == 0 {
		return patchVerdictResult{passed: true, summary: trunc(out, 400), ecosystem: eco.name}
	}

	// Non-zero exit: classify against the detected ecosystem's
	// blacklists. Order matches interpret() in the repro stage so
	// the two stages agree on what counts as "env failure".
	lowOut := strings.ToLower(out)
	switch {
	case hasAnyMarker(lowOut, defaultEnvMarkers):
		return patchVerdictResult{envFailure: true, summary: trunc(out, 400), ecosystem: eco.name}
	case hasAnyMarker(lowOut, eco.toolchainMarkers):
		return patchVerdictResult{envFailure: true, summary: trunc(out, 400), ecosystem: eco.name}
	case hasAnyMarker(lowOut, eco.buildMarkers):
		return patchVerdictResult{envFailure: true, summary: trunc(out, 400), ecosystem: eco.name}
	}

	// Non-zero exit, no env / toolchain / build markers. The test
	// ran and failed (or we have no positive evidence either way,
	// in which case the conservative default is "fix rejected" so
	// the agent gets a chance to revise the fix). This is the
	// acceptance-criterion-3 distinction: env-failure must NOT be
	// conflated with fix-rejected, so we only set fixRejected here
	// and leave envFailure false.
	return patchVerdictResult{fixRejected: true, summary: trunc(out, 400), ecosystem: eco.name}
}

// mergedWriteFiles merges repro test files and patch files into a single
// WriteFiles map for sandbox injection.  Patch files take precedence for
// non-test paths (though validatePatchPlan already prevents collisions).
func mergedWriteFiles(reproPlan *Plan, patchPlan *PatchPlan) map[string][]byte {
	out := make(map[string][]byte, len(reproPlan.Files)+len(patchPlan.Files))
	for path, content := range reproPlan.Files {
		out[path] = []byte(content)
	}
	for path, content := range patchPlan.Files {
		out[path] = []byte(content)
	}
	return out
}

// computeDiff computes a unified diff between original repo files and the
// patched content by writing the patched versions to a temp directory and
// running `git diff --no-index`.
//
// git diff --no-index exits 1 when files differ (which is expected here) and
// exits 0 when they are identical.  Both are treated as success; only exit
// codes >= 2 indicate a genuine git error.  The diff text is capped at
// patchMaxDiffBytes.
func computeDiff(repoDir string, patchFiles map[string]string) (string, error) {
	tmp, err := os.MkdirTemp("", "bugbot-patch-diff-*")
	if err != nil {
		return "", fmt.Errorf("compute diff: mktemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	var diffParts []string

	for rel, newContent := range patchFiles {
		clean := filepath.Clean(rel)
		origPath := filepath.Join(repoDir, clean)

		// Write the patched content to the temp dir.
		patchedPath := filepath.Join(tmp, clean)
		if mkErr := os.MkdirAll(filepath.Dir(patchedPath), 0o755); mkErr != nil {
			return "", fmt.Errorf("compute diff: mkdir: %w", mkErr)
		}
		if werr := os.WriteFile(patchedPath, []byte(newContent), 0o644); werr != nil {
			return "", fmt.Errorf("compute diff: write patched file: %w", werr)
		}

		// git diff --no-index exits 0 (same) or 1 (diff) or >=2 (error).
		cmd := exec.Command("git", "diff", "--no-index", "--", origPath, patchedPath)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		runErr := cmd.Run()

		exitCode := 0
		if runErr != nil {
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				return "", fmt.Errorf("compute diff: run git: %w", runErr)
			}
		}
		if exitCode >= 2 {
			return "", fmt.Errorf("compute diff: git error (exit %d): %s", exitCode, out.String())
		}
		diffParts = append(diffParts, out.String())
	}

	full := strings.Join(diffParts, "")
	if len(full) > patchMaxDiffBytes {
		full = full[:patchMaxDiffBytes] + "\n... [diff truncated at 32KB]\n"
	}
	return full, nil
}

// writePatchArtifacts appends patch files and a patch.diff to the existing
// repro artifact bundle directory.
func writePatchArtifacts(artifactDir, findingID string, plan *PatchPlan, diffText string) error {
	id := findingID
	if id == "" {
		id = "unknown"
	}
	dir := filepath.Join(artifactDir, id)
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return mkErr
	}

	// Write each patched file under a patch/ subdirectory to avoid clobbering
	// the original repro witness files.
	patchDir := filepath.Join(dir, "patch")
	if mkErr := os.MkdirAll(patchDir, 0o755); mkErr != nil {
		return mkErr
	}
	for rel, content := range plan.Files {
		clean := filepath.Clean(rel)
		dst := filepath.Join(patchDir, clean)
		if mkErr := os.MkdirAll(filepath.Dir(dst), 0o755); mkErr != nil {
			return mkErr
		}
		if werr := os.WriteFile(dst, []byte(content), 0o644); werr != nil {
			return werr
		}
	}

	if diffText != "" {
		if werr := os.WriteFile(filepath.Join(dir, "patch.diff"), []byte(diffText), 0o644); werr != nil {
			return werr
		}
	}
	return nil
}

// promoteToT0 updates the finding to Tier-0 with the fix-patch diff text.
func promoteToT0(ctx context.Context, st *store.Store, f store.Finding, diffText string) error {
	current, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		return err
	}
	current.Tier = 0
	current.FixPatch = diffText
	if _, err := st.UpsertFinding(ctx, current); err != nil {
		return err
	}
	return nil
}

// flagNeedsHuman marks the finding as needing human review and appends a
// summary to its Reasoning.  Tier stays 1.
func flagNeedsHuman(ctx context.Context, st *store.Store, f store.Finding, attempts int, lastFailure string) error {
	current, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		return err
	}
	current.NeedsHuman = true
	// Idempotence: re-running repro+patch over an already-flagged finding must
	// not grow Reasoning with duplicate appends.
	if strings.Contains(current.Reasoning, "PATCH-PROVER:") {
		if _, err := st.UpsertFinding(ctx, current); err != nil {
			return err
		}
		return nil
	}
	suffix := fmt.Sprintf(
		"\n\nPATCH-PROVER: no plausible minimal fix after %d attempt(s) — possibly misdiagnosed; needs human review. Last failure: %s",
		attempts, trunc(lastFailure, 300),
	)
	current.Reasoning = current.Reasoning + suffix
	if _, err := st.UpsertFinding(ctx, current); err != nil {
		return err
	}
	return nil
}

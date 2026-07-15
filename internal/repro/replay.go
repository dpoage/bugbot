package repro

// replay.go is the exported entry point `bugbot bundle replay` (see
// internal/cli/bundle.go) uses to re-run a saved bundle against a target
// checkout. It shares buildSpec (repro.go) with the official Attempt path so
// a replay genuinely exercises the same workspace-reconstruction the
// official run does, and shares the promotion gate (ClassifyTargetExecution
// pre-execute, interpret post-execute) so a bundle that would not have
// promoted today is reported as such rather than silently "succeeding".
//
// Replay deliberately does NOT call writeArtifacts: replaying an existing
// bundle never mutates or re-mints it.

import (
	"context"
	"time"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// ReplayOptions configures a single Replay call.
type ReplayOptions struct {
	// Image is the sandbox image to run under. Empty uses the sandbox
	// backend's configured default. Callers that want replay to use exactly
	// the image the bundle was originally demonstrated under should pass
	// bundle.Manifest.Sandbox.Image.
	Image string
	// Timeout bounds the sandbox execution. Zero uses DefaultTimeout.
	Timeout time.Duration
	// Deps carries the dependency-mount plumbing (ROMounts/Env/SetupCmds) for
	// the target repo, resolved the same way New/BuildReproducer resolve it
	// (see sandbox.ResolveDeps). Zero value runs with no extra mounts.
	Deps sandbox.Resolution
}

// ReplayResult is the outcome of replaying a saved bundle.
type ReplayResult struct {
	// Demonstrated is true when the bug still reproduces: a non-zero exit
	// with positive ran-evidence, same as a fresh Attempt would promote on.
	Demonstrated bool
	// Reason is the non-demonstration category when Demonstrated is false;
	// the zero value (VerdictReasonExitZero's zero-value counterpart, "")
	// never occurs — every non-demonstrating outcome sets a reason,
	// including VerdictReasonTargetNotExecuted when the pre-execute static
	// gate rejected the plan without running the sandbox at all (ExitCode
	// and SandboxRan are both zero-value in that case).
	Reason VerdictReason
	// SandboxRan reports whether the sandbox actually executed the plan.
	// False only when the pre-execute ClassifyTargetExecution gate rejected
	// the bundle outright (Reason == VerdictReasonTargetNotExecuted and
	// ExitCode/Summary carry no sandbox evidence).
	SandboxRan bool
	// ExitCode is the sandboxed command's exit code. Only meaningful when
	// SandboxRan is true.
	ExitCode int
	// Summary is a short human-readable digest of the outcome (gate
	// rejection detail, or the sandbox run's output digest).
	Summary string
	// Ecosystem is the detected testing ecosystem (from plan.Cmd).
	Ecosystem sandbox.Ecosystem
	// WitnessOnly mirrors Attempt.WitnessOnly: true when Demonstrated is
	// true but the detected ecosystem has no execution-witness coverage
	// format at all (bugbot-qb4r layer b, witnessDemonstration) — repro-as-
	// evidence, not repro-as-promotion. Always false when Demonstrated is
	// false.
	WitnessOnly bool
}

// Replay re-runs bundle's plan against sb, rooted at repoDir (the target
// repository's current checkout — NOT necessarily the commit the bundle was
// originally demonstrated against; that is the point of replay).
//
// Network is always forced to "none": a replay is by definition re-executing
// an ALREADY-SAVED plan against a checkout the operator controls, so there is
// no dependency-resolution justification (unlike a live Attempt, which may
// need network to fetch a fresh CMake/Cargo dependency) to ever loosen it,
// and a saved bundle is exactly the kind of untrusted, previously-generated
// command that the sandbox's hardened default exists to contain.
//
// The gate sequence mirrors Attempt's exactly (bugbot-qb4r layers a and b):
// ClassifyTargetExecution runs first (pre-execute, static, no sandbox cost)
// against the bundle's own recorded finding.File; only when it raises no
// objection does Replay spend a sandbox run. The result is interpreted with
// the same interpret() the official path uses, and — when that interpretation
// demonstrates the bug — witnessDemonstration re-checks it against any
// per-file coverage evidence in the run's output, exactly as Attempt does
// before writeArtifacts.
func Replay(ctx context.Context, sb sandbox.Sandbox, repoDir string, b *Bundle, opts ReplayOptions) (ReplayResult, error) {
	plan := b.Plan()
	ecoName := detectEcosystem(plan.Cmd).name

	if reason, detail := ClassifyTargetExecution(plan.Files, b.Manifest.Finding.File, ecoName); reason != "" {
		return ReplayResult{Reason: reason, Summary: detail, Ecosystem: ecoName}, nil
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	spec := buildSpec(repoDir, plan, opts.Image, "none", timeout, opts.Deps)
	res, err := sb.Exec(ctx, spec)
	if err != nil {
		return ReplayResult{}, err
	}

	v := interpret(res, plan.Cmd)
	if v.demonstrated {
		v = witnessDemonstration(v, combinedOutput(res), b.Manifest.Finding.File)
	}
	return ReplayResult{
		Demonstrated: v.demonstrated,
		Reason:       v.reason,
		SandboxRan:   true,
		ExitCode:     res.ExitCode,
		Summary:      v.summary,
		Ecosystem:    v.ecosystem,
		WitnessOnly:  v.witnessOnly,
	}, nil
}

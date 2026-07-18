package repro

// audit.go is the exported entry point `bugbot bundle audit` (see
// internal/cli/bundle.go) uses to run the static, sandbox-free promotion
// gate over a saved bundle. It is the same pre-execute check Replay (and,
// once Qb4rImpl's Attempt wiring lands, the official reproduce stage) run
// before ever touching a container — audit exists so an operator (or a CI
// job) can flag a non-behavioral bundle at write time, with no container
// runtime, LLM, or target repo checkout required at all.

import "github.com/dpoage/bugbot/internal/sandbox"

// AuditResult is the outcome of statically auditing a saved bundle.
type AuditResult struct {
	// Reason is empty when no static objection was raised (the bundle's test
	// files demonstrably reach the target through an executable edge).
	// Non-empty (VerdictReasonTargetNotExecuted today) flags the bundle.
	Reason VerdictReason
	// Detail is a short human-readable explanation, empty when Reason is.
	Detail string
	// Ecosystem is the testing ecosystem detected from the bundle's plan.cmd.
	Ecosystem sandbox.Ecosystem
}

// Flagged reports whether the audit raised a static objection.
func (r AuditResult) Flagged() bool { return r.Reason != "" }

// Audit runs the static target-execution gate (ClassifyTargetExecution)
// against b's recorded plan and finding target, with no sandbox execution.
func Audit(b *Bundle) AuditResult {
	plan := b.Plan()
	ecoName := detectEcosystem(plan.Cmd).name
	reason, detail := ClassifyTargetExecution(plan.Files, plan.Cmd, b.Manifest.Finding.File, targetGateEcosystem(ecoName, b.Manifest.Finding.File))
	return AuditResult{Reason: reason, Detail: detail, Ecosystem: ecoName}
}

// Package agent is Bugbot's tool-call execution harness: a reusable loop that
// drives an [llm.Client] through a bounded set of tools until the model
// produces a final answer, runs out of iterations, or exhausts a token budget.
//
// The pipeline stages (finder / verifier / reproducer agents) all instantiate
// the same [Runner] with different system prompts and tool sets. The harness
// itself is provider-agnostic: it speaks only the normalized [llm] vocabulary.
//
// # Tools
//
// A [Tool] declares its schema via [Tool.Def] and executes via [Tool.Run].
// Tool errors are *not* loop failures: they are fed back to the model as
// tool-result content prefixed with "ERROR:" so the model can recover (retry
// with different arguments, try another tool, or give up gracefully). Only
// infrastructure-level failures (a failed [llm.Client.Complete], context
// cancellation) abort the loop.
//
// The built-in read-only code tools ([NewReadFile], [NewListDir], [NewGrep])
// are rooted at a single repository directory and enforce path-traversal
// protection: absolute paths and "../" escapes are rejected, and symlinks may
// not resolve outside the root.
//
// # Limits and partial results
//
// The loop enforces two limits: [Limits.MaxIterations] (model turns) and
// [Limits.TokenBudget] (cumulative input+output tokens from [llm.Usage]).
// Exceeding either stops the loop cleanly, returning an [Outcome] with
// Truncated set and the last assistant text preserved — partial results are
// data, not errors. Only context cancellation and infra failures return a
// non-nil error from [Runner.Run].
//
// # Transcripts
//
// Every run can be recorded as an ordered [Transcript] of events
// (requests, assistant turns, tool calls, tool results, usage). Transcripts
// serialize to JSONL and can be replayed offline through a [ReplayClient],
// which is the building block for the eval harness.
package agent

import "github.com/dpoage/bugbot/internal/llm"

// Default limits applied when a Runner is constructed with zero-value limits.
const (
	// DefaultMaxIterations bounds the number of model turns in a single run.
	DefaultMaxIterations = 20
	// DefaultTokenBudget bounds cumulative input+output tokens across a run.
	// Zero in Limits means "use this default"; a negative value means unlimited.
	DefaultTokenBudget int64 = 1_000_000
)

// Limits bounds a single [Runner.Run]. The zero value is valid and resolves to
// the package defaults.
type Limits struct {
	// MaxIterations caps the number of model turns. Zero uses
	// DefaultMaxIterations. A negative value disables the iteration cap.
	MaxIterations int
	// TokenBudget caps cumulative input+output tokens (summed from llm.Usage
	// across every completion in the run). Zero uses DefaultTokenBudget. A
	// negative value disables the budget.
	TokenBudget int64
	// BudgetCheck, when non-nil, is consulted by the Runner BEFORE each model
	// call. It lets a shared budget pool (see BudgetPool) stop an in-flight run
	// at the next turn boundary once a run-spanning ceiling is hit, independent
	// of this run's own per-run TokenBudget. Returning ErrBudgetExhausted stops
	// the run cleanly with TruncBudgetPool; any other non-nil error is treated
	// as an infrastructure failure and returned from Run rather than being
	// misreported as a budget stop. The hook MUST be read-only with respect
	// to the request (it must not mutate system/tools/history), so it cannot
	// break request prefix stability. It may be invoked concurrently across
	// parallel runs, so it must be safe for concurrent use.
	BudgetCheck func() error
}

// resolve returns the effective limits, substituting defaults for zero values.
func (l Limits) resolve() Limits {
	out := l
	if out.MaxIterations == 0 {
		out.MaxIterations = DefaultMaxIterations
	}
	if out.TokenBudget == 0 {
		out.TokenBudget = DefaultTokenBudget
	}
	return out
}

// Truncation reasons recorded in [Outcome.TruncationReason].
const (
	// TruncMaxIterations means the run hit the iteration cap before the model
	// produced a final (non-tool) answer.
	TruncMaxIterations = "max_iterations"
	// TruncTokenBudget means cumulative token usage exceeded the budget.
	TruncTokenBudget = "token_budget"
	// TruncBudgetPool means a shared budget pool (via Limits.BudgetCheck) was
	// exhausted before this run's own per-run budget, so the run stopped
	// pre-turn. It is distinct from TruncTokenBudget so the funnel can tell a
	// run that was stopped by the run-spanning ceiling ("budget-stopped") from
	// one that merely exhausted its own allowance.
	TruncBudgetPool = "budget_pool"
)

// Outcome is the result of a [Runner.Run]. A truncated run is still a valid
// outcome: FinalText holds whatever the model produced last, and the Transcript
// captures the full interaction.
type Outcome struct {
	// FinalText is the model's last assistant text. On a clean finish it is the
	// model's answer; on truncation it is the most recent assistant text (which
	// may be empty if the model only ever requested tools).
	FinalText string
	// Truncated reports whether the run stopped because it hit a limit rather
	// than the model finishing its turn.
	Truncated bool
	// TruncationReason is one of the Trunc* constants when Truncated is true,
	// otherwise empty.
	TruncationReason string
	// Iterations is the number of completed model turns.
	Iterations int
	// Usage is cumulative token consumption across the run.
	Usage llm.Usage
	// Finalized reports whether forced finalization fired: the loop reserved its
	// last turn, injected the finalization prompt, and took one final completion
	// instead of returning dangling exploration prose. See [WithFinalization].
	Finalized bool
	// LastStopReason is the stop reason of the final completion in the run. It is
	// StopMaxTokens when the model's last output was truncated at the token cap,
	// which JSON-expecting callers use to distinguish "truncated mid-answer" from
	// a genuine parse failure.
	LastStopReason llm.StopReason
	// Transcript is the full ordered record of the run. Never nil.
	Transcript *Transcript
}

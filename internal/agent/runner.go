package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dpoage/bugbot/internal/llm"
)

// Runner drives an [llm.Client] through a tool-call loop. Construct one per
// pipeline role (finder/verifier/reproducer) with that role's system prompt and
// tool set, then call [Runner.Run] (or [Runner.RunJSON]) per task. A Runner is
// safe for sequential reuse across tasks; it holds no per-run mutable state.
type Runner struct {
	client       llm.Client
	tools        toolSet
	systemPrompt string
	limits       Limits

	// transcriptDir, when non-empty, receives an auto-saved JSONL transcript per
	// run, named "<timestamp>-<slug>.jsonl".
	transcriptDir string
	// maxTokens caps output tokens per completion (passed through to the client).
	// Zero lets the adapter apply its own default.
	maxTokens int
}

// Option configures a [Runner] at construction.
type Option func(*Runner)

// WithLimits sets the per-run iteration and token limits. Zero fields resolve
// to package defaults.
func WithLimits(l Limits) Option {
	return func(r *Runner) { r.limits = l }
}

// WithTranscriptDir makes each run auto-save its transcript to a JSONL file
// under dir, named "<RFC3339-timestamp>-<task-slug>.jsonl". The directory is
// created on demand.
func WithTranscriptDir(dir string) Option {
	return func(r *Runner) { r.transcriptDir = dir }
}

// WithMaxTokens caps output tokens per completion. Zero uses the adapter
// default.
func WithMaxTokens(n int) Option {
	return func(r *Runner) { r.maxTokens = n }
}

// NewRunner builds a Runner bound to client, the given tools, and a system
// prompt. Options tune limits, transcript persistence, and output token caps.
func NewRunner(client llm.Client, tools []Tool, systemPrompt string, opts ...Option) *Runner {
	r := &Runner{
		client:       client,
		tools:        newToolSet(tools),
		systemPrompt: systemPrompt,
	}
	for _, opt := range opts {
		opt(r)
	}
	r.limits = r.limits.resolve()
	return r
}

// Run executes the tool loop for a single task. It seeds the conversation with
// the system prompt and the task as a user message, then repeatedly calls the
// model and executes any requested tools until the model finishes its turn, a
// limit is hit, or an infrastructure error occurs.
//
// Limit exhaustion is not an error: it returns an [Outcome] with Truncated set
// and the last assistant text preserved. Only context cancellation and
// client/IO failures return a non-nil error. The returned Outcome's Transcript
// is always non-nil, even on error, capturing whatever happened before the
// failure.
func (r *Runner) Run(ctx context.Context, task string) (*Outcome, error) {
	tr := NewTranscript()

	messages := []llm.Message{{Role: llm.RoleUser, Content: task}}

	outcome := &Outcome{Transcript: tr}

	for {
		// Stop before the next turn if we've hit the iteration cap.
		if r.limits.MaxIterations >= 0 && outcome.Iterations >= r.limits.MaxIterations {
			r.finishTruncated(outcome, TruncMaxIterations)
			break
		}
		// Stop before the next turn if we're already over budget.
		if r.overBudget(outcome.Usage) {
			r.finishTruncated(outcome, TruncTokenBudget)
			break
		}
		// Consult the shared budget pool (if any) before issuing the next model
		// call, so a run already in flight stops at this turn boundary once the
		// run-spanning ceiling is hit rather than running to completion. This is a
		// read-only check: it does not touch System/Tools/Messages, so request
		// prefix stability is preserved.
		if r.limits.BudgetCheck != nil {
			if err := r.limits.BudgetCheck(); err != nil {
				r.finishTruncated(outcome, TruncBudgetPool)
				break
			}
		}

		if err := ctx.Err(); err != nil {
			r.autosave(tr, task)
			return outcome, err
		}

		req := llm.Request{
			System:    r.systemPrompt,
			Messages:  messages,
			Tools:     r.tools.defs,
			MaxTokens: r.maxTokens,
		}
		tr.recordRequest(outcome.Iterations+1, messages)

		resp, err := r.client.Complete(ctx, req)
		if err != nil {
			r.autosave(tr, task)
			return outcome, fmt.Errorf("agent: completion failed at iteration %d: %w", outcome.Iterations+1, err)
		}

		outcome.Iterations++
		outcome.Usage.InputTokens += resp.Usage.InputTokens
		outcome.Usage.OutputTokens += resp.Usage.OutputTokens
		outcome.Usage.CacheReadInputTokens += resp.Usage.CacheReadInputTokens
		outcome.Usage.CacheCreationInputTokens += resp.Usage.CacheCreationInputTokens
		tr.recordAssistant(outcome.Iterations, resp)

		if resp.Text != "" {
			outcome.FinalText = resp.Text
		}

		// Append the assistant turn (text + any tool-call requests) to the
		// conversation so the model sees its own prior turn.
		messages = append(messages, llm.Message{
			Role:      llm.RoleAssistant,
			Content:   resp.Text,
			ToolCalls: resp.ToolCalls,
		})

		// No tool calls => the model finished its turn. Clean completion.
		if len(resp.ToolCalls) == 0 {
			break
		}

		// Execute each requested tool call sequentially and feed results back.
		for _, call := range resp.ToolCalls {
			if err := ctx.Err(); err != nil {
				r.autosave(tr, task)
				return outcome, err
			}
			result, isErr := r.runTool(ctx, call)
			tr.recordToolResult(outcome.Iterations, call, result, isErr)
			messages = append(messages, llm.Message{
				Role:       llm.RoleToolResult,
				ToolCallID: call.ID,
				Content:    result,
				IsError:    isErr,
			})
		}

		// After executing tools, check the budget again before looping so we
		// truncate promptly rather than issuing one more expensive completion.
		if r.overBudget(outcome.Usage) {
			r.finishTruncated(outcome, TruncTokenBudget)
			break
		}
	}

	r.autosave(tr, task)
	return outcome, nil
}

// runTool dispatches one tool call. A missing tool or a Run error is returned to
// the model as an "ERROR:"-prefixed result (isErr=true) rather than aborting the
// loop. Context cancellation surfaced by the tool is still rendered as a tool
// error here; the loop's own ctx checks handle real cancellation.
func (r *Runner) runTool(ctx context.Context, call llm.ToolCall) (result string, isErr bool) {
	tool, ok := r.tools.lookup(call.Name)
	if !ok {
		return toolError(fmt.Errorf("unknown tool %q", call.Name)), true
	}
	out, err := tool.Run(ctx, call.Arguments)
	if err != nil {
		return toolError(err), true
	}
	return out, false
}

// overBudget reports whether cumulative usage has exceeded the token budget. A
// negative budget means unlimited.
func (r *Runner) overBudget(u llm.Usage) bool {
	if r.limits.TokenBudget < 0 {
		return false
	}
	return u.InputTokens+u.OutputTokens > r.limits.TokenBudget
}

// finishTruncated marks the outcome as a clean partial result.
func (r *Runner) finishTruncated(o *Outcome, reason string) {
	o.Truncated = true
	o.TruncationReason = reason
}

// autosave writes the transcript to the configured TranscriptDir, if any.
// Persistence failures are intentionally swallowed: a run's result must not
// hinge on disk availability, and the caller still has the in-memory transcript
// on the Outcome.
func (r *Runner) autosave(tr *Transcript, task string) {
	if r.transcriptDir == "" {
		return
	}
	if err := os.MkdirAll(r.transcriptDir, 0o755); err != nil {
		return
	}
	ts := tr.now().UTC().Format("20060102T150405.000Z")
	name := ts + "-" + slug(task) + ".jsonl"
	path := filepath.Join(r.transcriptDir, name)
	f, err := os.Create(path)
	if err != nil {
		return
	}
	// autosave is best-effort: persistence failures must not break a run, so we
	// deliberately discard both the write and close errors. Close still runs to
	// flush buffered data even on the SaveJSONL error path.
	defer func() { _ = f.Close() }()
	_ = tr.SaveJSONL(f)
}

// slugRE keeps slugs filesystem-safe: lowercase alphanumerics and dashes.
var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// slug derives a short, filesystem-safe label from a task string.
func slug(task string) string {
	s := strings.ToLower(task)
	s = slugRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "run"
	}
	if len(s) > 48 {
		s = strings.Trim(s[:48], "-")
		if s == "" {
			s = "run"
		}
	}
	return s
}

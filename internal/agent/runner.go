package agent

import (
	"context"
	"errors"
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
	return r.run(ctx, task, "")
}

// run is the shared loop body. finalizePrompt, when non-empty, enables forced
// finalization: one turn before the iteration cap the loop injects this
// user-role message and takes a final tool-less completion so the model emits
// its answer instead of dangling exploration prose. RunJSON passes a
// JSON-demanding prompt; the public Run passes "".
func (r *Runner) run(ctx context.Context, task, finalizePrompt string) (*Outcome, error) {
	tr := NewTranscript()

	messages := []llm.Message{{Role: llm.RoleUser, Content: task}}

	outcome := &Outcome{Transcript: tr}

	for {
		// Stop before the next turn if we've hit the iteration cap. When forced
		// finalization is enabled, reserve this last turn: inject the finalization
		// prompt and take one final completion so the model emits its answer
		// instead of leaving dangling exploration prose. We only do this once.
		if r.limits.MaxIterations >= 0 && outcome.Iterations >= r.limits.MaxIterations {
			if finalizePrompt != "" && !outcome.Finalized {
				messages = append(messages, llm.Message{
					Role:    llm.RoleUser,
					Content: finalizePrompt,
				})
				outcome.Finalized = true
				resp, err := r.completeOnce(ctx, tr, &messages, outcome, true)
				if err != nil {
					r.autosave(tr, task)
					return outcome, err
				}
				_ = resp
			}
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
				if !errors.Is(err, ErrBudgetExhausted) {
					// A hook failure that isn't a budget stop is an infrastructure
					// error; surface it rather than misreporting a clean stop.
					r.autosave(tr, task)
					return outcome, fmt.Errorf("agent: budget check: %w", err)
				}
				r.finishTruncated(outcome, TruncBudgetPool)
				break
			}
		}

		if err := ctx.Err(); err != nil {
			r.autosave(tr, task)
			return outcome, err
		}

		resp, err := r.completeOnce(ctx, tr, &messages, outcome, false)
		if err != nil {
			r.autosave(tr, task)
			return outcome, err
		}

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

// completeOnce issues one model completion against the current conversation,
// records it, folds its usage into the outcome, and appends the assistant turn
// to *messages. When final is true the request carries no tools (the
// finalization turn forbids further investigation).
//
// If the completion stops at the token cap (StopMaxTokens) it makes ONE
// continuation completion — appending a short user nudge — so a JSON answer cut
// off mid-object has a chance to be completed rather than failing to parse. The
// outcome's LastStopReason reflects the final completion served here.
func (r *Runner) completeOnce(ctx context.Context, tr *Transcript, messages *[]llm.Message, outcome *Outcome, final bool) (llm.Response, error) {
	resp, err := r.complete(ctx, tr, *messages, outcome, final)
	if err != nil {
		return llm.Response{}, err
	}
	*messages = append(*messages, llm.Message{
		Role:      llm.RoleAssistant,
		Content:   resp.Text,
		ToolCalls: resp.ToolCalls,
	})

	// One continuation retry when output was truncated mid-generation: ask the
	// model to continue and emit ONLY the remaining answer, then concatenate.
	// Guarded so it fires at most once per completeOnce call.
	if resp.StopReason == llm.StopMaxTokens && len(resp.ToolCalls) == 0 {
		*messages = append(*messages, llm.Message{
			Role:    llm.RoleUser,
			Content: "Your previous message was cut off at the output token limit. Continue from exactly where you stopped and output ONLY the remaining text needed to complete the answer — no preamble, no repetition.",
		})
		cont, cerr := r.complete(ctx, tr, *messages, outcome, final)
		if cerr != nil {
			return llm.Response{}, cerr
		}
		*messages = append(*messages, llm.Message{
			Role:      llm.RoleAssistant,
			Content:   cont.Text,
			ToolCalls: cont.ToolCalls,
		})
		// Stitch the two halves so the caller (and FinalText) sees one answer.
		joined := resp.Text + cont.Text
		outcome.FinalText = joined
		resp.Text = joined
		resp.ToolCalls = cont.ToolCalls
		resp.StopReason = cont.StopReason
	}
	return resp, nil
}

// complete issues a single completion, records it, and accumulates its usage and
// stop reason onto the outcome. It does NOT mutate the conversation; callers
// append the assistant turn so they control conversation shape (e.g. the
// continuation retry).
func (r *Runner) complete(ctx context.Context, tr *Transcript, messages []llm.Message, outcome *Outcome, final bool) (llm.Response, error) {
	req := llm.Request{
		System:    r.systemPrompt,
		Messages:  messages,
		Tools:     r.tools.defs,
		MaxTokens: r.maxTokens,
	}
	// The finalization turn forbids further investigation: drop the tools so the
	// model can only answer.
	if final {
		req.Tools = nil
	}
	tr.recordRequest(outcome.Iterations+1, messages)

	resp, err := r.client.Complete(ctx, req)
	if err != nil {
		return llm.Response{}, fmt.Errorf("agent: completion failed at iteration %d: %w", outcome.Iterations+1, err)
	}

	outcome.Iterations++
	outcome.Usage.InputTokens += resp.Usage.InputTokens
	outcome.Usage.OutputTokens += resp.Usage.OutputTokens
	outcome.Usage.CacheReadInputTokens += resp.Usage.CacheReadInputTokens
	outcome.Usage.CacheCreationInputTokens += resp.Usage.CacheCreationInputTokens
	outcome.LastStopReason = resp.StopReason
	tr.recordAssistant(outcome.Iterations, resp)

	if resp.Text != "" {
		outcome.FinalText = resp.Text
	}
	return resp, nil
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

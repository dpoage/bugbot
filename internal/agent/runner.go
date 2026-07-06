package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
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
	// activitySink, when non-nil, is called once per turn with a short
	// single-line note about what the agent is doing (derived from tool calls).
	// Nil by default; zero overhead when unset.
	activitySink func(activity string)
	// toolHealthSink, when non-nil, is called whenever a tool returns a
	// *ToolHealthError (a genuine harness/infra failure, not an ordinary
	// model-recoverable error). Plain tool errors do not trigger it. Nil by
	// default; zero overhead when unset.
	toolHealthSink func(tool string, he *ToolHealthError)
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

// WithActivitySink registers a callback invoked once per tool-call turn with a
// short single-line note about what the agent is doing (derived from its tool
// calls via [toolCallActivity]). The callback must be safe for concurrent use.
// A nil fn is a no-op. When unset, the runner emits no activity notes and
// incurs zero overhead.
func WithActivitySink(fn func(activity string)) Option {
	return func(r *Runner) { r.activitySink = fn }
}

// WithToolHealthSink registers a callback invoked when a tool returns a
// *ToolHealthError — a genuine harness/infra failure (missing container
// runtime, crashed language server) rather than an ordinary
// model-recoverable error (bad args, file-not-found). The callback must be
// safe for concurrent use. A nil fn is a no-op. When unset, the runner
// records no health signals and incurs zero overhead.
func WithToolHealthSink(fn func(tool string, he *ToolHealthError)) Option {
	return func(r *Runner) { r.toolHealthSink = fn }
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
//
// Max-tokens continuation: when a turn stops at the output token cap
// (StopMaxTokens) with no tool calls, Run makes ONE extra continuation
// completion that turn — nudging the model to emit the rest — and stitches the
// two halves into a single assistant message (see [Runner.completeOnce]). This
// applies to plain Run, not only RunJSON: a truncated final answer is completed
// rather than returned half-written. It costs at most one additional completion
// per truncated turn and is reflected in the Outcome's Iterations and Usage.
func (r *Runner) Run(ctx context.Context, task string) (*Outcome, error) {
	return r.run(ctx, task, "", nil)
}

// run is the shared loop body. finalizePrompt, when non-empty, enables forced
// finalization: when a stop condition fires (iteration cap, per-run token
// budget, or shared budget pool) the loop injects this user-role message and
// takes a single final tool-less completion so the model can emit its answer
// instead of dangling exploration prose or a silently empty output. RunJSON
// passes a JSON-demanding prompt; the public Run passes "" and therefore never
// pays the extra turn.
//
// responseSchema, when non-nil, is the JSON Schema for the final answer. It is
// attached to every completion in the run (capability-gated; see [complete]),
// so adapters that support structured output can apply grammar-constrained
// decoding. The public Run passes nil; RunJSON passes its schema.
func (r *Runner) run(ctx context.Context, task, finalizePrompt string, responseSchema json.RawMessage) (*Outcome, error) {
	tr := NewTranscript()
	if r.transcriptDir != "" {
		tr.enableStreaming(r.transcriptPath(tr, task))
	}

	messages := []llm.Message{{Role: llm.RoleUser, Content: task}}

	outcome := &Outcome{Transcript: tr}

	// History-compaction state. toolNameByID lets a tool-result stub name the
	// tool it answered; compactThreshold re-arms upward after each firing so
	// compaction is bounded and never thrashes the prompt cache turn-over-turn.
	toolNameByID := map[string]string{}
	compactThreshold := r.limits.HistoryTokenBudget

	for {
		// Stop before the next turn if we've hit the iteration cap. The
		// finalizeAndTruncate helper below gives RunJSON its one reserved
		// finalization turn so a near-cap model can still emit its answer; the
		// public Run (finalizePrompt == "") is a no-op and proceeds straight
		// to the truncation mark.
		if r.limits.MaxIterations >= 0 && outcome.Iterations >= r.limits.MaxIterations {
			if err := r.finalizeAndTruncate(ctx, tr, &messages, outcome, finalizePrompt, responseSchema, compactThreshold, toolNameByID, task); err != nil {
				return outcome, err
			}
			r.finishTruncated(outcome, TruncMaxIterations)
			break
		}
		// Stop before the next turn if we're already over budget. The budget
		// stop gets the same one reserved finalization turn the iteration cap
		// gets (RunJSON only), so a near-budget finder can emit its answer
		// instead of returning a silently empty result to the funnel.
		if r.overBudget(outcome.Usage) {
			if err := r.finalizeAndTruncate(ctx, tr, &messages, outcome, finalizePrompt, responseSchema, compactThreshold, toolNameByID, task); err != nil {
				return outcome, err
			}
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
					tr.closeStream()
					return outcome, fmt.Errorf("agent: budget check: %w", err)
				}
				// Shared pool exhausted: give the model one reserved finalization
				// turn (RunJSON only) so a near-budget finder can still emit its
				// answer before we classify the stop as TruncBudgetPool.
				if err := r.finalizeAndTruncate(ctx, tr, &messages, outcome, finalizePrompt, responseSchema, compactThreshold, toolNameByID, task); err != nil {
					return outcome, err
				}
				r.finishTruncated(outcome, TruncBudgetPool)
				break
			}
		}

		if err := ctx.Err(); err != nil {
			tr.closeStream()
			return outcome, err
		}

		// Threshold-triggered, one-shot-per-crossing history compaction. Done at
		// the turn boundary BEFORE the completion so the smaller history is what
		// gets billed this turn. Re-arming the threshold upward after a firing
		// keeps compaction bounded and avoids re-paying a prefix cache miss every
		// subsequent turn (see compactRearmFactor).
		messages, compactThreshold = r.maybeCompact(messages, compactThreshold, toolNameByID)

		resp, err := r.completeOnce(ctx, tr, &messages, outcome, responseSchema, false)
		if err != nil {
			tr.closeStream()
			return outcome, err
		}

		// No tool calls => the model finished its turn.
		if len(resp.ToolCalls) == 0 {
			// StopError means the model stopped for a provider-specific error
			// reason (refusal, safety filter, recitation). Breaking cleanly here
			// would record refusal prose — or stale FinalText from an earlier
			// turn — as the answer (bugbot-wm2m). Surface a typed error instead.
			if resp.StopReason == llm.StopError {
				tr.closeStream()
				return outcome, &ErrStopReason{StopReason: resp.StopReason, Text: resp.Text, Outcome: outcome}
			}
			break
		}

		// Derive a short activity note from this turn's tool calls and route
		// it to the activity sink (if any). Done before executing the tools
		// so the observer sees "reading foo.go" as the work is happening, not
		// after the slow tool call completes.
		if r.activitySink != nil {
			r.activitySink(toolCallActivity(resp.ToolCalls))
		}

		// Execute each requested tool call sequentially and feed results back.
		for _, call := range resp.ToolCalls {
			if err := ctx.Err(); err != nil {
				tr.closeStream()
				return outcome, err
			}
			result, isErr := r.runTool(ctx, call)
			tr.recordToolResult(outcome.Iterations, call, result, isErr)
			toolNameByID[call.ID] = call.Name
			messages = append(messages, llm.Message{
				Role:       llm.RoleToolResult,
				ToolCallID: call.ID,
				Content:    result,
				IsError:    isErr,
			})
		}

		// After executing tools, check the budget again before looping so we
		// truncate promptly rather than issuing one more expensive completion.
		// A budget hit post-tool still gets the one reserved finalization turn
		// (RunJSON only) so a near-budget finder can emit its answer.
		if r.overBudget(outcome.Usage) {
			if err := r.finalizeAndTruncate(ctx, tr, &messages, outcome, finalizePrompt, responseSchema, compactThreshold, toolNameByID, task); err != nil {
				return outcome, err
			}
			r.finishTruncated(outcome, TruncTokenBudget)
			break
		}
	}

	tr.closeStream()
	return outcome, nil
}

// finalizeAndTruncate is the single reserved finalization turn used by EVERY
// stop condition the loop can hit (iteration cap, per-run token budget, shared
// budget pool). It is a no-op unless finalizePrompt is non-empty AND the run
// has not already taken a finalization turn this run, so the public Run path
// (finalizePrompt == "") never pays an extra model call. When it does fire it:
//
//   - appends finalizePrompt as a user-role message;
//   - sets outcome.Finalized = true;
//   - compacts once so the prompt that is sent is the smallest it can be;
//   - takes ONE tool-less completion via completeOnce (which itself handles
//     the StopMaxTokens continuation retry), giving the model a cheap final
//     shot at emitting its answer instead of leaving the funnel with a
//     silently empty output.
//
// responseSchema, when non-nil, is attached to the finalization completion so a
// schema-aware adapter applies grammar-constrained decoding on the final turn
// too. The public Run path passes nil so no schema is attached.
//
// Returns done=true when a finalization turn was actually taken (so the caller
// knows Finalized is now true), and a non-nil err only when the underlying
// completion failed — in which case the caller should return early with the
// error. The caller is responsible for finishTruncated(reason) + break: the
// reason is the STOP condition (budget/iteration), not "finalized".
func (r *Runner) finalizeAndTruncate(
	ctx context.Context,
	tr *Transcript,
	messages *[]llm.Message,
	outcome *Outcome,
	finalizePrompt string,
	responseSchema json.RawMessage,
	compactThreshold int64,
	toolNameByID map[string]string,
	task string,
) error {
	if finalizePrompt == "" || outcome.Finalized {
		return nil
	}
	*messages = append(*messages, llm.Message{
		Role:    llm.RoleUser,
		Content: finalizePrompt,
	})
	outcome.Finalized = true
	// Compact before the finalization turn: it is often the largest history of
	// the run, and the model needs only its own reasoning chain (preserved) to
	// emit the answer, not every earlier file dump. The re-armed threshold is
	// discarded: this is the run's final turn, so no later compaction can fire.
	*messages, _ = r.maybeCompact(*messages, compactThreshold, toolNameByID)
	if _, cerr := r.completeOnce(ctx, tr, messages, outcome, responseSchema, true); cerr != nil {
		tr.closeStream()
		return cerr
	}
	return nil
}

// maybeCompact applies threshold-triggered history compaction. When compaction
// is enabled (threshold > 0) and the estimated history size exceeds threshold,
// it prunes tool-result content older than the most recent few turns to short
// stubs and returns the compacted history together with a re-armed (higher)
// threshold so the next firing only happens once history has grown materially
// again. When disabled or under threshold — or when there is nothing left to
// prune — it returns the messages and threshold unchanged, so a normal run pays
// no allocation and the append-only prefix (and its cache) is preserved.
func (r *Runner) maybeCompact(messages []llm.Message, threshold int64, toolNameByID map[string]string) ([]llm.Message, int64) {
	if threshold <= 0 {
		return messages, threshold
	}
	if estimateTokens(messages) <= threshold {
		return messages, threshold
	}
	compacted, pruned := compactHistory(messages, compactRecentToolResults, toolNameByID)
	if !pruned {
		// Over threshold but nothing prunable yet: either history is dominated by
		// assistant reasoning, or every prunable result is already a stub, or there
		// simply aren't more than compactRecentToolResults tool results so far. Leave the
		// threshold UNCHANGED so the check fires again next turn once a result ages out of
		// the recent window — re-arming here would starve real compaction by ratcheting
		// the threshold above the history before anything was ever reclaimed.
		return messages, threshold
	}
	// Real pruning happened (one prefix cache miss paid). Re-arm upward so the
	// next firing only comes after history has grown materially again, bounding
	// total firings and avoiding turn-over-turn cache thrash.
	return compacted, threshold * compactRearmFactor
}

// repair issues a SINGLE tools-less, schema-bearing completion against the
// repair prompt. It replaces the previous "fresh tool loop" repair path with
// the constrained shape: a single completion where adapters that support
// structured output apply grammar-constrained decoding natively, so the
// answer is shape-correct on the wire. Tools are dropped so Google and
// Anthropic (which refuse to combine tool use with native structured output)
// also get the schema honored.
//
// responseSchema, when non-nil, is attached capability-gated; when the
// adapter's StructuredOutput capability is off, the schema is dropped
// silently (per llm.Request docs) and the prompt-embedded schema instruction
// is the only enforcement — same contract as the main run path.
//
// The repair uses a fresh outcome but shares the caller's transcript so the
// assistant turn is recorded there for parity with the main run path. Only
// ONE completion is issued: no max-tokens continuation, no tool loop. This
// bounds the repair to exactly the one model call the spec mandates.
func (r *Runner) repair(ctx context.Context, tr *Transcript, prompt string, responseSchema json.RawMessage) (*Outcome, error) {
	outcome := &Outcome{Transcript: tr}
	messages := []llm.Message{{Role: llm.RoleUser, Content: prompt}}
	if _, err := r.completeOnce(ctx, tr, &messages, outcome, responseSchema, true); err != nil {
		return outcome, err
	}
	return outcome, nil
}

// completeOnce issues one model completion against the current conversation,
// records it, folds its usage into the outcome, and appends the assistant turn
// to *messages. When final is true the request carries no tools (the
// finalization turn forbids further investigation).
//
// responseSchema, when non-nil and the client's StructuredOutput capability is
// on, is attached to the request so adapters can apply native
// schema-constrained output. When the capability is off, the schema is
// dropped silently — the prompt-embedded schema instruction is the only
// enforcement, matching the no-cap passthrough path's contract.
//
// If the completion stops at the token cap (StopMaxTokens) it makes ONE
// continuation completion — appending a short user nudge — so a JSON answer cut
// off mid-object has a chance to be completed rather than failing to parse. The
// outcome's LastStopReason reflects the final completion served here.
func (r *Runner) completeOnce(ctx context.Context, tr *Transcript, messages *[]llm.Message, outcome *Outcome, responseSchema json.RawMessage, final bool) (llm.Response, error) {
	resp, err := r.complete(ctx, tr, *messages, outcome, responseSchema, final)
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
		cont, cerr := r.complete(ctx, tr, *messages, outcome, responseSchema, final)
		if cerr != nil {
			return llm.Response{}, cerr
		}
		*messages = append(*messages, llm.Message{
			Role:      llm.RoleAssistant,
			Content:   cont.Text,
			ToolCalls: cont.ToolCalls,
		})
		// Stitch the two halves so the caller (and FinalText) sees one answer.
		// Models frequently ignore "continue from where you stopped" and instead
		// restart, repeating some head of the first half. A naive resp.Text+cont.Text
		// would then double that prefix and corrupt the JSON. Trim the longest
		// suffix of resp.Text that the continuation re-emits as its prefix before
		// concatenating, so a clean cut and a repeated-prefix restart both stitch
		// into one well-formed answer.
		joined := stitchContinuation(resp.Text, cont.Text)
		outcome.FinalText = joined
		resp.Text = joined
		resp.ToolCalls = cont.ToolCalls
		resp.StopReason = cont.StopReason
	}
	return resp, nil
}

// maxStitchOverlap bounds how far back stitchContinuation scans for a repeated
// prefix. The overlap is the chunk a restarting model re-emits; a few KB covers
// realistic restarts while keeping the scan O(n) and immune to a pathological
// continuation that happens to share a huge prefix with the head.
const maxStitchOverlap = 4096

// stitchContinuation joins a truncated first half with its continuation, undoing
// the common case where the model restarts and repeats some head of head as the
// prefix of cont. It finds the LONGEST suffix of head (bounded by
// maxStitchOverlap) that is a prefix of cont and drops that overlap from cont
// before concatenating. With no overlap it degrades to head+cont (the clean-cut
// case). The scan is longest-first so a model that repeats more text wins over a
// coincidental short match.
func stitchContinuation(head, cont string) string {
	max := len(head)
	if max > maxStitchOverlap {
		max = maxStitchOverlap
	}
	if max > len(cont) {
		max = len(cont)
	}
	for n := max; n > 0; n-- {
		if head[len(head)-n:] == cont[:n] {
			return head + cont[n:]
		}
	}
	return head + cont
}

// complete issues a single completion, records it, and accumulates its usage and
// stop reason onto the outcome. It does NOT mutate the conversation; callers
// append the assistant turn so they control conversation shape (e.g. the
// continuation retry).
//
// responseSchema, when non-nil AND the client's StructuredOutput capability is
// on, is attached to the request so adapters that support native structured
// output can apply grammar-constrained decoding. When the capability is off
// (a conservative openai-compatible endpoint, etc.), the schema is silently
// dropped on the wire (per [llm.Request.ResponseSchema] docs) and only the
// prompt-embedded schema instruction is in effect. This is the agent-layer
// gate: the no-cap passthrough path sends NO schema, matching today's
// behavior, while the with-cap path gets a hard native shape guarantee.
func (r *Runner) complete(ctx context.Context, tr *Transcript, messages []llm.Message, outcome *Outcome, responseSchema json.RawMessage, final bool) (llm.Response, error) {
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
	// Native schema-constrained output is capability-gated at the agent layer:
	// when the client cannot honor a schema, do not put one on the wire. The
	// schema name is left empty so each adapter picks its own default
	// ("response" on OpenAI, "emit_answer" on Anthropic).
	if len(responseSchema) > 0 && r.client.Capabilities().StructuredOutput {
		req.ResponseSchema = responseSchema
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
		outcome.FinalTextSet = true
	} else {
		outcome.FinalTextSet = false
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
		// Record a tool-health signal only for a genuine *ToolHealthError AND
		// only when ctx is not already cancelled: a failure caused by run
		// teardown/cancellation must never be counted as a tool-health problem.
		var he *ToolHealthError
		if ctx.Err() == nil && errors.As(err, &he) && r.toolHealthSink != nil {
			r.toolHealthSink(call.Name, he)
		}
		return toolError(err), true
	}
	return out, false
}

// overBudget reports whether cumulative usage has exceeded the token budget. A
// negative budget means unlimited. Cache reads are discounted by
// CacheReadWeight (resolved to 1.0 by Limits.resolve() when unset) so a
// cache-heavy run is bounded by its real cost, not raw prompt size.
func (r *Runner) overBudget(u llm.Usage) bool {
	if r.limits.TokenBudget < 0 {
		return false
	}
	return u.ChargeableTokens(r.limits.CacheReadWeight) > r.limits.TokenBudget
}

// finishTruncated marks the outcome as a clean partial result. reason MUST be
// one of the Trunc* constants and non-empty; passing an empty reason panics to
// surface a programming error at the call site rather than silently producing
// an Outcome that violates the Truncated→TruncationReason invariant.
func (r *Runner) finishTruncated(o *Outcome, reason string) {
	if reason == "" {
		panic("agent: finishTruncated called with empty reason — callers must pass a Trunc* constant")
	}
	o.Truncated = true
	o.TruncationReason = reason
}

// transcriptPath computes the JSONL path for a run's transcript under
// r.transcriptDir, named "<timestamp>-<task-slug>.jsonl". Streaming (see
// Transcript.enableStreaming) opens this path lazily on the first recorded
// event; a run that never records anything never creates the file or its
// parent directory.
func (r *Runner) transcriptPath(tr *Transcript, task string) string {
	ts := tr.now().UTC().Format("20060102T150405.000Z")
	name := ts + "-" + slug(task) + ".jsonl"
	return filepath.Join(r.transcriptDir, name)
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

// toolCallActivity derives a short, single-line activity string from a turn's
// tool calls. It returns a human-readable description of the first call (e.g.
// "reading main.go", "grepping "foo"") truncated to 80 runes. When calls is
// empty, it returns "".
//
// This is a PURE function: it reads only the tool-call metadata that the LLM
// already produced and makes no model calls. Safe for concurrent use.
func toolCallActivity(calls []llm.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	call := calls[0]

	// Extract a key argument from the JSON args to make the note concrete.
	var args struct {
		Path      string `json:"path"`
		Dir       string `json:"dir"`
		Directory string `json:"directory"`
		Pattern   string `json:"pattern"`
		Symbol    string `json:"symbol"`
	}
	// Best-effort parse; zero struct fields on failure produce a sane fallback.
	_ = json.Unmarshal(call.Arguments, &args)

	var note string
	switch call.Name {
	case "read_file":
		p := args.Path
		if p == "" {
			p = "file"
		}
		note = "reading " + p
	case "list_dir":
		d := args.Dir
		if d == "" {
			d = args.Directory
		}
		if d == "" {
			d = "."
		}
		note = "listing " + d
	case "grep":
		pat := args.Pattern
		if pat == "" {
			pat = "…"
		}
		note = "grepping " + strconv.Quote(pat)
	case "find_definition", "find_references", "find_implementations", "read_symbol":
		sym := args.Symbol
		if sym == "" {
			sym = "symbol"
		}
		note = "navigating " + sym
	case "sandbox_exec":
		note = "running sandbox"
	case "post_lead":
		note = "posting lead"
	default:
		note = call.Name
	}

	// Collapse any embedded newlines and truncate to 80 runes.
	note = strings.Join(strings.Fields(note), " ")
	runes := []rune(note)
	if len(runes) > 80 {
		note = string(runes[:79]) + "…"
	}
	return note
}

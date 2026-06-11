// Package llm is a thin, provider-agnostic abstraction over the LLM backends
// Bugbot drives (Anthropic, OpenAI, Google, and any OpenAI-compatible endpoint).
//
// It normalizes three things that otherwise differ wildly between providers:
//
//   - the request/response shape (messages, tools, tool-call round-trips, usage);
//   - capability profiles (context window, parallel tool calls, prompt caching,
//     structured output) so callers can adapt without provider sniffing;
//   - errors (rate limiting, auth, context-too-long, ...) into a small typed set.
//
// The layer is deliberately thin: each adapter maps these normalized types
// to/from its vendor SDK and nothing more. Higher-level concerns (the agent
// tool loop, budgets, transcripts) live in sibling packages.
//
// API keys are never logged. They are resolved at construction time via
// config.Config.APIKey (which reads the env var named in config) and handed
// straight to the vendor SDK.
package llm

import (
	"context"
	"encoding/json"
)

// Role enumerates the normalized message roles. Each adapter maps these to its
// provider's own role vocabulary.
type Role string

const (
	// RoleSystem is a system / developer instruction. Adapters hoist these into
	// the provider's dedicated system field where one exists.
	RoleSystem Role = "system"
	// RoleUser is an end-user (or harness) turn.
	RoleUser Role = "user"
	// RoleAssistant is a model turn. When it carries ToolCalls it represents the
	// model's request to invoke tools (Anthropic tool_use / OpenAI tool_calls /
	// Gemini functionCall).
	RoleAssistant Role = "assistant"
	// RoleToolResult carries the result of a previously-requested tool call back
	// to the model. ToolCallID must reference the originating ToolCall.ID.
	RoleToolResult Role = "tool-result"
)

// Message is a single normalized turn in a conversation.
//
// The meaning of the fields depends on Role:
//
//   - system/user: Content holds the text.
//   - assistant: Content holds any text; ToolCalls holds tool-use requests.
//     Either or both may be present.
//   - tool-result: ToolCallID identifies the call being answered, Content holds
//     the (textual) result, and IsError marks a failed execution.
type Message struct {
	Role      Role
	Content   string
	ToolCalls []ToolCall
	// ToolCallID is set only on RoleToolResult messages.
	ToolCallID string
	// IsError marks a RoleToolResult as a failed tool execution.
	IsError bool
}

// ToolDef declares a tool the model may call. Parameters is a JSON Schema object
// describing the tool's arguments, carried verbatim as raw JSON so callers keep
// full control over the schema and adapters never lose fidelity.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolCall is a single tool invocation requested by the model. Arguments is the
// raw JSON argument object exactly as the model produced it; callers should
// json.Unmarshal it rather than string-matching, since providers differ in
// escaping.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Request is a normalized completion request.
type Request struct {
	// System is an optional system prompt. It is kept separate from Messages so
	// adapters can route it to the provider's dedicated system field.
	System string
	// Messages is the ordered conversation. It must not be empty.
	Messages []Message
	// Tools is the set of tools the model may call. May be empty.
	Tools []ToolDef
	// MaxTokens caps output tokens. If zero, the adapter applies a sane default.
	MaxTokens int
	// Temperature is the sampling temperature. Nil means "use the provider
	// default" (some models reject an explicit temperature). Use a pointer so
	// callers can distinguish "0.0" from "unset".
	Temperature *float64
}

// StopReason is the normalized reason a completion ended.
type StopReason string

const (
	// StopEndTurn: the model finished its turn naturally.
	StopEndTurn StopReason = "end_turn"
	// StopToolUse: the model is requesting one or more tool calls.
	StopToolUse StopReason = "tool_use"
	// StopMaxTokens: output was truncated at the token limit.
	StopMaxTokens StopReason = "max_tokens"
	// StopError: the model stopped for a provider-specific reason that maps to
	// none of the above (refusal, safety, recitation, ...). Check the response
	// text and provider logs for detail.
	StopError StopReason = "error"
)

// Usage reports token consumption for a single completion. Callers ledger this
// per role/provider/model via a Recorder.
//
// Normalization convention: InputTokens is the TOTAL prompt size — it INCLUDES
// any tokens that were read from or written to the provider's prompt cache.
// CacheReadInputTokens and CacheCreationInputTokens are informational subsets
// of InputTokens used to compute cache savings; they never add to it.
//
// This matches the native OpenAI (prompt_tokens includes cached_tokens) and
// Gemini (promptTokenCount includes cachedContentTokenCount) conventions.
// Anthropic reports input_tokens EXCLUDING cache reads/writes, so its adapter
// sums input_tokens + cache_read_input_tokens + cache_creation_input_tokens
// into InputTokens. The payoff: existing ledger math (budgets, context-size
// estimates, input+output totals) keeps meaning "tokens the model processed"
// regardless of cache hits, and a caller that ignores the cache fields sees
// exactly the pre-caching numbers.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	// CacheReadInputTokens is the subset of InputTokens served from the
	// provider's prompt cache (billed at a steep discount: ~0.1x on Anthropic,
	// 0.25–0.5x on OpenAI). Zero when the provider reports no cache activity.
	CacheReadInputTokens int64
	// CacheCreationInputTokens is the subset of InputTokens written to the
	// prompt cache this call (Anthropic bills these at 1.25x). Only Anthropic
	// reports this; it is zero elsewhere.
	CacheCreationInputTokens int64
}

// ChargeableTokens returns the budget-relevant token count for this usage,
// discounting cache reads by cacheReadWeight (0..1). Raw InputTokens counts
// cache reads at full weight, but they bill at a steep discount (~0.1x
// Anthropic, 0.25–0.5x OpenAI), so a cache-heavy run exhausts a raw-token
// budget far faster than its real cost warrants. cacheReadWeight of 1.0
// reproduces the old behavior; the funnel default discounts to ~0.1.
//
// Cache CREATION tokens are left at full weight (Anthropic bills them at
// 1.25x; treating them as cheap would understate cost).
func (u Usage) ChargeableTokens(cacheReadWeight float64) int64 {
	uncached := u.InputTokens - u.CacheReadInputTokens
	if uncached < 0 {
		uncached = 0
	}
	weighted := uncached + int64(float64(u.CacheReadInputTokens)*cacheReadWeight)
	return weighted + u.OutputTokens
}

// Response is a normalized completion response.
type Response struct {
	// Text is the concatenated assistant text output (may be empty when the model
	// only requested tools).
	Text string
	// ToolCalls holds any tool-use requests the model made.
	ToolCalls []ToolCall
	// Usage reports token consumption.
	Usage Usage
	// StopReason is the normalized stop reason.
	StopReason StopReason
}

// Capabilities describes what a given provider+model supports, so callers can
// adapt (e.g. serialize tool calls when ParallelToolCalls is false) without
// sniffing the provider type.
type Capabilities struct {
	// ContextWindow is the model's maximum input+output token window. Zero means
	// unknown (e.g. an arbitrary OpenAI-compatible endpoint).
	ContextWindow int
	// ParallelToolCalls reports whether the model may return more than one tool
	// call in a single response.
	ParallelToolCalls bool
	// PromptCaching reports whether the provider supports prompt caching.
	PromptCaching bool
	// StructuredOutput reports whether the provider supports schema-constrained
	// JSON output.
	StructuredOutput bool
}

// Client is the single interface every adapter implements. It is intentionally
// minimal: one synchronous completion call plus a capability probe.
type Client interface {
	// Complete runs a single completion. It is context-aware: cancellation and
	// deadlines propagate to the underlying request. Errors are normalized into
	// the typed errors in errors.go where possible.
	Complete(ctx context.Context, req Request) (Response, error)
	// Capabilities returns the static capability profile for this client's
	// provider+model.
	Capabilities() Capabilities
}

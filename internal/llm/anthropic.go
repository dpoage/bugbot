package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// anthropicAdapter maps the normalized types onto the official Anthropic SDK.
type anthropicAdapter struct {
	client anthropic.Client
	model  string
	caps   Capabilities
}

// anthropicOptions configures an Anthropic adapter.
//
// Exactly one of apiKey and authToken must be non-empty — they are mutually
// exclusive. apiKey drives x-api-key authentication (api_key mode). authToken
// drives OAuth bearer-token authentication (oauth-token mode), which requires
// the anthropic-beta: oauth-2025-04-20 header and must NOT also set x-api-key
// (the Anthropic API rejects requests that carry both credentials).
type anthropicOptions struct {
	apiKey     string
	authToken  string       // OAuth bearer token; mutually exclusive with apiKey
	baseURL    string       // optional; for testing or proxies
	httpClient *http.Client // optional; for testing (httptest)
}

// newAnthropicAdapter builds an Anthropic-backed Client. The vendor SDK's
// built-in retries are disabled (WithMaxRetries(0)) so our shared retry wrapper
// is the single source of retry policy.
//
// When opts.authToken is non-empty the adapter uses OAuth bearer-token
// authentication: the SDK sets Authorization: Bearer <token> via
// option.WithAuthToken, and option.WithHeaderAdd appends the required
// "oauth-2025-04-20" value to the anthropic-beta header without clobbering any
// other beta values the SDK may already have set.
func newAnthropicAdapter(model string, opts anthropicOptions) *anthropicAdapter {
	reqOpts := []option.RequestOption{
		option.WithMaxRetries(0),
	}
	if opts.authToken != "" {
		// OAuth mode: bearer token in Authorization header; x-api-key must be absent.
		reqOpts = append(reqOpts, option.WithAuthToken(opts.authToken))
		// Append rather than replace so other anthropic-beta values set by the SDK
		// are preserved alongside the oauth beta flag.
		reqOpts = append(reqOpts, option.WithHeaderAdd("anthropic-beta", "oauth-2025-04-20"))
	} else {
		// API-key mode: standard x-api-key authentication.
		reqOpts = append(reqOpts, option.WithAPIKey(opts.apiKey))
	}
	if opts.baseURL != "" {
		reqOpts = append(reqOpts, option.WithBaseURL(opts.baseURL))
	}
	if opts.httpClient != nil {
		reqOpts = append(reqOpts, option.WithHTTPClient(opts.httpClient))
	}
	return &anthropicAdapter{
		client: anthropic.NewClient(reqOpts...),
		model:  model,
		caps:   anthropicCapabilities(model),
	}
}

func (a *anthropicAdapter) Capabilities() Capabilities { return a.caps }

func (a *anthropicAdapter) Complete(ctx context.Context, req Request) (Response, error) {
	params, err := a.buildParams(req)
	if err != nil {
		return Response{}, err
	}

	msg, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return Response{}, a.normalizeErr(err)
	}
	return a.toResponse(msg), nil
}

func (a *anthropicAdapter) buildParams(req Request) (anthropic.MessageNewParams, error) {
	maxTokens := int64(req.MaxTokens)
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: maxTokens,
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	if req.Temperature != nil {
		params.Temperature = anthropic.Float(*req.Temperature)
	}

	msgs, err := toAnthropicMessages(req.Messages)
	if err != nil {
		return anthropic.MessageNewParams{}, err
	}
	params.Messages = msgs

	if len(req.Tools) > 0 {
		tools := make([]anthropic.ToolUnionParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			tp, err := toAnthropicTool(t)
			if err != nil {
				return anthropic.MessageNewParams{}, err
			}
			tools = append(tools, anthropic.ToolUnionParam{OfTool: tp})
		}
		params.Tools = tools
	}

	applyCacheBreakpoints(&params)
	return params, nil
}

// applyCacheBreakpoints marks ephemeral prompt-cache breakpoints on the
// request. Anthropic caches the prefix up to each breakpoint (max 4 per
// request), so the agent loop's stable prefix — tool definitions, then the
// system prompt, then the append-only conversation — is re-read from cache
// instead of re-billed at full price each iteration.
//
// Placement (≤ 4 total):
//
//   - the last tool definition (tools serialize before system/messages, so one
//     breakpoint there caches the whole tool block);
//   - the last system block (caches tools + system);
//   - the last content block of the final message AND of the message before it.
//     The final-message breakpoint writes this iteration's full conversation to
//     cache; the second-to-last one lands exactly where the previous iteration's
//     final breakpoint was, anchoring an exact cache hit even when a turn
//     appends more blocks than the server's automatic ~20-block lookback covers.
//
// Moving breakpoints between requests does not invalidate cache entries —
// lookup is by content prefix, not by marker position.
func applyCacheBreakpoints(params *anthropic.MessageNewParams) {
	if n := len(params.Tools); n > 0 {
		if tp := params.Tools[n-1].OfTool; tp != nil {
			tp.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
	}
	if n := len(params.System); n > 0 {
		params.System[n-1].CacheControl = anthropic.NewCacheControlEphemeralParam()
	}
	for i, marked := len(params.Messages)-1, 0; i >= 0 && marked < 2; i-- {
		if markLastBlock(&params.Messages[i]) {
			marked++
		}
	}
}

// markLastBlock sets an ephemeral cache_control on the last content block of m,
// reporting whether a markable block was found. Thinking blocks (and any other
// variant without a CacheControl field) are skipped by GetCacheControl
// returning nil; the adapter never produces those today.
func markLastBlock(m *anthropic.MessageParam) bool {
	for i := len(m.Content) - 1; i >= 0; i-- {
		if cc := m.Content[i].GetCacheControl(); cc != nil {
			*cc = anthropic.NewCacheControlEphemeralParam()
			return true
		}
	}
	return false
}

func toAnthropicTool(t ToolDef) (*anthropic.ToolParam, error) {
	var schema anthropic.ToolInputSchemaParam
	if len(t.Parameters) > 0 {
		var props map[string]any
		if err := json.Unmarshal(t.Parameters, &props); err != nil {
			return nil, newAPIError("anthropic", 0, 0, ErrInvalidRequest,
				"tool "+t.Name+": invalid parameters JSON schema", err)
		}
		// The Anthropic schema param carries the JSON-schema "properties" and
		// "required" within an object whose type is implicitly "object". The SDK
		// marshals Properties as the schema body; pass the decoded properties
		// (and required, if present) through.
		if p, ok := props["properties"]; ok {
			if pm, ok := p.(map[string]any); ok {
				schema.Properties = pm
			}
		} else {
			// Caller passed a bare properties object rather than a full schema.
			schema.Properties = props
		}
		if r, ok := props["required"]; ok {
			if rs, ok := r.([]any); ok {
				req := make([]string, 0, len(rs))
				for _, v := range rs {
					if s, ok := v.(string); ok {
						req = append(req, s)
					}
				}
				schema.Required = req
			}
		}
	}
	tp := &anthropic.ToolParam{
		Name:        t.Name,
		InputSchema: schema,
	}
	if t.Description != "" {
		tp.Description = anthropic.String(t.Description)
	}
	return tp, nil
}

// toAnthropicMessages converts normalized messages into Anthropic message
// params, coalescing consecutive tool-result turns into a single user message
// (Anthropic requires tool_result blocks to ride in a user turn).
func toAnthropicMessages(msgs []Message) ([]anthropic.MessageParam, error) {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	var pendingResults []anthropic.ContentBlockParamUnion

	flush := func() {
		if len(pendingResults) > 0 {
			out = append(out, anthropic.NewUserMessage(pendingResults...))
			pendingResults = nil
		}
	}

	for _, m := range msgs {
		switch m.Role {
		case RoleSystem:
			// System messages are hoisted into params.System by the caller; if one
			// appears inline, treat it as a user instruction to preserve content.
			flush()
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		case RoleUser:
			flush()
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		case RoleAssistant:
			flush()
			blocks := make([]anthropic.ContentBlockParamUnion, 0, 1+len(m.ToolCalls))
			if m.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
			}
			for _, tc := range m.ToolCalls {
				var input any
				if len(tc.Arguments) > 0 {
					if err := json.Unmarshal(tc.Arguments, &input); err != nil {
						return nil, newAPIError("anthropic", 0, 0, ErrInvalidRequest,
							"assistant tool call "+tc.Name+": invalid arguments JSON", err)
					}
				}
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID:    tc.ID,
						Name:  tc.Name,
						Input: input,
					},
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anthropic.NewTextBlock(""))
			}
			out = append(out, anthropic.NewAssistantMessage(blocks...))
		case RoleToolResult:
			pendingResults = append(pendingResults,
				anthropic.NewToolResultBlock(m.ToolCallID, m.Content, m.IsError))
		default:
			return nil, newAPIError("anthropic", 0, 0, ErrInvalidRequest,
				"unknown message role "+string(m.Role), nil)
		}
	}
	flush()
	return out, nil
}

func (a *anthropicAdapter) toResponse(msg *anthropic.Message) Response {
	var resp Response
	var text string
	for _, block := range msg.Content {
		switch v := block.AsAny().(type) {
		case anthropic.TextBlock:
			text += v.Text
		case anthropic.ToolUseBlock:
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID:        v.ID,
				Name:      v.Name,
				Arguments: json.RawMessage(v.JSON.Input.Raw()),
			})
		}
	}
	resp.Text = text
	// Anthropic's input_tokens EXCLUDES cache reads/writes; normalize to the
	// inclusive convention documented on Usage by summing all three.
	resp.Usage = Usage{
		InputTokens: msg.Usage.InputTokens +
			msg.Usage.CacheReadInputTokens + msg.Usage.CacheCreationInputTokens,
		OutputTokens:             msg.Usage.OutputTokens,
		CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
		CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
	}
	resp.StopReason = mapAnthropicStop(msg.StopReason)
	return resp
}

func mapAnthropicStop(sr anthropic.StopReason) StopReason {
	switch sr {
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
		return StopEndTurn
	case anthropic.StopReasonToolUse:
		return StopToolUse
	case anthropic.StopReasonMaxTokens:
		return StopMaxTokens
	case anthropic.StopReasonRefusal:
		return StopError
	default:
		return StopEndTurn
	}
}

func (a *anthropicAdapter) normalizeErr(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		status := apiErr.StatusCode
		resp := apiErr.Response
		kind := classifyStatus(status, apiErr.Error())
		ra := time.Duration(0)
		if kind == ErrRateLimited || kind == ErrOverloaded {
			ra = parseRetryAfter(resp)
		}
		return newAPIError("anthropic", status, ra, kind, apiErr.Error(), err)
	}
	// Transport/timeout error: leave status 0, mark as server-class so it is
	// retried.
	return newAPIError("anthropic", 0, 0, ErrServer, err.Error(), err)
}

func anthropicCapabilities(model string) Capabilities {
	// Anthropic models support parallel tool calls, prompt caching, and
	// structured output. Context window varies; 200k is a safe floor for the
	// 4.x family and beyond.
	return Capabilities{
		ContextWindow:     200_000,
		ParallelToolCalls: true,
		PromptCaching:     true,
		StructuredOutput:  true,
	}
}

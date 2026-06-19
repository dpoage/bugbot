package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// openaiAdapter maps the normalized types onto the OpenAI Go SDK's Chat
// Completions API. It backs both the "openai" provider and the
// "openai-compatible" provider (Ollama/vLLM/Groq/etc.) — the only difference is
// the base URL and capability profile, set at construction.
type openaiAdapter struct {
	client   openai.Client
	model    string
	caps     Capabilities
	provider string // "openai" or "openai-compatible", for error tagging
	// requireBoolAdditionalProps forces every object/array-valued
	// "additionalProperties" in outbound tool-parameter and response_format
	// schemas down to a permissive boolean. Strict openai-compatible schema
	// validators (notably MiniMax) reject the JSON-Schema subschema form with a
	// 400 before any tokens are generated. Off for first-party OpenAI, which
	// accepts the subschema form in lenient mode. See coerceBoolAdditionalProperties.
	requireBoolAdditionalProps bool
}

type openaiOptions struct {
	apiKey                     string
	baseURL                    string
	httpClient                 *http.Client
	provider                   string
	caps                       Capabilities
	requireBoolAdditionalProps bool
}

// newOpenAIAdapter builds an OpenAI-backed Client. With a custom baseURL it
// serves any OpenAI-compatible endpoint. The vendor SDK's retries are disabled
// so the shared retry wrapper governs retry policy.
func newOpenAIAdapter(model string, opts openaiOptions) *openaiAdapter {
	reqOpts := []option.RequestOption{
		option.WithAPIKey(opts.apiKey),
		option.WithMaxRetries(0),
	}
	if opts.baseURL != "" {
		reqOpts = append(reqOpts, option.WithBaseURL(opts.baseURL))
	}
	if opts.httpClient != nil {
		reqOpts = append(reqOpts, option.WithHTTPClient(opts.httpClient))
	}
	provider := opts.provider
	if provider == "" {
		provider = "openai"
	}
	return &openaiAdapter{
		client:                     openai.NewClient(reqOpts...),
		model:                      model,
		caps:                       opts.caps,
		provider:                   provider,
		requireBoolAdditionalProps: opts.requireBoolAdditionalProps,
	}
}

func (o *openaiAdapter) Capabilities() Capabilities { return o.caps }

func (o *openaiAdapter) Complete(ctx context.Context, req Request) (Response, error) {
	params, err := o.buildParams(req)
	if err != nil {
		return Response{}, err
	}
	cc, err := o.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return Response{}, o.normalizeErr(err)
	}
	return o.toResponse(cc), nil
}

func (o *openaiAdapter) buildParams(req Request) (openai.ChatCompletionNewParams, error) {
	params := openai.ChatCompletionNewParams{
		Model: shared.ChatModel(o.model),
	}
	if req.MaxTokens > 0 {
		params.MaxCompletionTokens = openai.Int(int64(req.MaxTokens))
	}
	if req.Temperature != nil {
		params.Temperature = openai.Float(*req.Temperature)
	}

	msgs := make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, openai.SystemMessage(req.System))
	}
	converted, err := toOpenAIMessages(req.Messages)
	if err != nil {
		return openai.ChatCompletionNewParams{}, err
	}
	msgs = append(msgs, converted...)
	params.Messages = msgs

	if len(req.Tools) > 0 {
		tools := make([]openai.ChatCompletionToolUnionParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := shared.FunctionDefinitionParam{Name: t.Name}
			if t.Description != "" {
				fn.Description = openai.String(t.Description)
			}
			if len(t.Parameters) > 0 {
				var schema map[string]any
				if err := json.Unmarshal(t.Parameters, &schema); err != nil {
					return openai.ChatCompletionNewParams{}, newAPIError(o.provider, 0, 0,
						ErrInvalidRequest, "tool "+t.Name+": invalid parameters JSON schema", err)
				}
				if o.requireBoolAdditionalProps {
					coerceBoolAdditionalProperties(schema)
				}
				fn.Parameters = shared.FunctionParameters(schema)
			}
			tools = append(tools, openai.ChatCompletionFunctionTool(fn))
		}
		params.Tools = tools
	}

	// Schema-constrained output. Honored even when tools are present
	// (OpenAI permits response_format alongside tools; compatible backends
	// that don't will surface a 4xx, which is the correct failure mode
	// rather than silent loss). Skipped when the capability is off so a
	// provider with StructuredOutput=false (e.g. conservative
	// openai-compatible endpoint without opt-in) does not get a schema it
	// can't honor.
	if len(req.ResponseSchema) > 0 && o.caps.StructuredOutput {
		// parseResponseSchema unmarshals the JSON Schema and returns the
		// resolved name (caller's name, or "response" if unset). The OpenAI
		// SDK expects the verbatim schema object, not the properties/required
		// unwrap that Anthropic's ToolInputSchemaParam requires, so we
		// deliberately do NOT call parseToolParameters here — the on-the-wire
		// shape would change.
		defaultName := req.ResponseSchemaName
		if defaultName == "" {
			defaultName = "response"
		}
		schema, name, err := parseResponseSchema(req.ResponseSchema, defaultName)
		if err != nil {
			return openai.ChatCompletionNewParams{}, newAPIError(o.provider, 0, 0,
				ErrInvalidRequest, "ResponseSchema: invalid JSON", err)
		}
		if o.requireBoolAdditionalProps {
			schema = coerceBoolAdditionalProperties(schema)
		}
		// strict=false: our schemas are not strict-mode-clean (extra fields,
		// union types) and lenient mode still drives grammar-constrained
		// decoding on OpenAI and on compatible backends that implement it.
		params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   name,
					Schema: schema,
				},
			},
		}
	}
	return params, nil
}

// coerceBoolAdditionalProperties rewrites an object/array-valued
// "additionalProperties" in a decoded JSON Schema to the permissive boolean
// true, recursing through nested subschemas. Some OpenAI-compatible backends
// (notably MiniMax) run a strict schema type-checker over response_format /
// tool parameters that accepts ONLY a boolean additionalProperties and rejects
// the JSON-Schema subschema form (e.g. {"type":"string"} used for free-form
// string maps) with HTTP 400 before any tokens are generated. Downgrading to
// true keeps the map "open" on the wire; the value-type constraint is still
// enforced locally by the agent's RunJSON validator against the ORIGINAL
// schema, so no validation is lost. A boolean additionalProperties (including
// false) is passed through untouched.
//
// The walk is schema-aware so it never corrupts a schema that legitimately
// uses "additionalProperties" as data rather than the keyword:
//   - name-keyed subschema maps (properties, patternProperties, $defs,
//     definitions, dependentSchemas) recurse into their VALUES only, so a
//     property literally NAMED "additionalProperties" is left intact;
//   - value-bearing keywords (enum, const, default, examples) hold literal
//     instance data, not schemas, and are never descended into.
//
// Nodes are mutated in place; the return value is the same node so callers can
// reassign generically.
func coerceBoolAdditionalProperties(v any) any {
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			switch k {
			case "additionalProperties":
				if _, isBool := child.(bool); !isBool {
					node[k] = true
				}
			case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas":
				if named, ok := child.(map[string]any); ok {
					for name, sub := range named {
						named[name] = coerceBoolAdditionalProperties(sub)
					}
				}
			case "enum", "const", "default", "examples":
				// literal instance data, not subschemas — leave untouched.
			default:
				node[k] = coerceBoolAdditionalProperties(child)
			}
		}
		return node
	case []any:
		for i, child := range node {
			node[i] = coerceBoolAdditionalProperties(child)
		}
		return node
	default:
		return v
	}
}

func toOpenAIMessages(msgs []Message) ([]openai.ChatCompletionMessageParamUnion, error) {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case RoleSystem:
			out = append(out, openai.SystemMessage(m.Content))
		case RoleUser:
			out = append(out, openai.UserMessage(m.Content))
		case RoleAssistant:
			am := openai.ChatCompletionAssistantMessageParam{}
			if m.Content != "" {
				am.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
					OfString: openai.String(m.Content),
				}
			}
			for _, tc := range m.ToolCalls {
				args := string(tc.Arguments)
				if args == "" {
					args = "{}"
				}
				am.ToolCalls = append(am.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: tc.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      tc.Name,
							Arguments: args,
						},
					},
				})
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: &am})
		case RoleToolResult:
			out = append(out, openai.ToolMessage(m.Content, m.ToolCallID))
		default:
			return nil, newAPIError("openai", 0, 0, ErrInvalidRequest,
				"unknown message role "+string(m.Role), nil)
		}
	}
	return out, nil
}

func (o *openaiAdapter) toResponse(cc *openai.ChatCompletion) Response {
	var resp Response
	if len(cc.Choices) > 0 {
		choice := cc.Choices[0]
		resp.Text = choice.Message.Content
		for _, tc := range choice.Message.ToolCalls {
			// Only function tool calls carry an arguments payload we surface.
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: json.RawMessage(tc.Function.Arguments),
			})
		}
		resp.StopReason = mapOpenAIStop(choice.FinishReason, len(resp.ToolCalls) > 0)
	}
	// prompt_tokens already INCLUDES cached tokens (OpenAI convention), which
	// matches the normalized Usage semantics directly. Caching is automatic
	// server-side for OpenAI; prompt_tokens_details.cached_tokens reports the
	// discounted subset. OpenAI-compatible endpoints (Ollama/vLLM/MiniMax/...)
	// often omit prompt_tokens_details entirely, in which case the SDK leaves
	// CachedTokens at zero — exactly the "no cache activity" value.
	resp.Usage = Usage{
		InputTokens:          cc.Usage.PromptTokens,
		OutputTokens:         cc.Usage.CompletionTokens,
		CacheReadInputTokens: cc.Usage.PromptTokensDetails.CachedTokens,
	}
	return resp
}

func mapOpenAIStop(reason string, hasToolCalls bool) StopReason {
	switch reason {
	case "stop":
		return StopEndTurn
	case "tool_calls", "function_call":
		return StopToolUse
	case "length":
		return StopMaxTokens
	case "content_filter":
		return StopError
	default:
		// Some OpenAI-compatible servers return tool calls with an empty or
		// nonstandard finish_reason; trust the presence of tool calls.
		if hasToolCalls {
			return StopToolUse
		}
		return StopEndTurn
	}
}

func (o *openaiAdapter) normalizeErr(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return normalizeSDKError(o.provider, apiErr.StatusCode, apiErr.Error(), apiErr.Response, err)
	}
	return newAPIError(o.provider, 0, 0, ErrServer, err.Error(), err)
}

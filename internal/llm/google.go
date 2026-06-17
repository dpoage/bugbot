package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"google.golang.org/genai"
)

// googleAdapter maps the normalized types onto the google.golang.org/genai SDK
// (Gemini API backend).
type googleAdapter struct {
	client *genai.Client
	model  string
	caps   Capabilities
}

type googleOptions struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// newGoogleAdapter builds a Gemini-backed Client. genai's only built-in retry
// path is for file uploads, so the shared retry wrapper is the sole retry layer
// for completions.
func newGoogleAdapter(ctx context.Context, model string, opts googleOptions) (*googleAdapter, error) {
	cc := &genai.ClientConfig{
		APIKey:  opts.apiKey,
		Backend: genai.BackendGeminiAPI,
	}
	if opts.baseURL != "" {
		cc.HTTPOptions.BaseURL = opts.baseURL
	}
	if opts.httpClient != nil {
		cc.HTTPClient = opts.httpClient
	}
	client, err := genai.NewClient(ctx, cc)
	if err != nil {
		return nil, newAPIError("google", 0, 0, ErrInvalidRequest,
			"failed to construct genai client: "+err.Error(), err)
	}
	return &googleAdapter{
		client: client,
		model:  model,
		caps:   googleCapabilities(model),
	}, nil
}

func (g *googleAdapter) Capabilities() Capabilities { return g.caps }

func (g *googleAdapter) Complete(ctx context.Context, req Request) (Response, error) {
	contents, err := toGoogleContents(req.Messages)
	if err != nil {
		return Response{}, err
	}

	cfg := &genai.GenerateContentConfig{}
	if req.System != "" {
		cfg.SystemInstruction = &genai.Content{
			Parts: []*genai.Part{{Text: req.System}},
		}
	}
	if req.MaxTokens > 0 {
		cfg.MaxOutputTokens = int32(req.MaxTokens)
	}
	if req.Temperature != nil {
		t := float32(*req.Temperature)
		cfg.Temperature = &t
	}
	if len(req.Tools) > 0 {
		decls := make([]*genai.FunctionDeclaration, 0, len(req.Tools))
		for _, t := range req.Tools {
			fd := &genai.FunctionDeclaration{
				Name:        t.Name,
				Description: t.Description,
			}
			if len(t.Parameters) > 0 {
				var schema any
				if err := json.Unmarshal(t.Parameters, &schema); err != nil {
					return Response{}, newAPIError("google", 0, 0, ErrInvalidRequest,
						"tool "+t.Name+": invalid parameters JSON schema", err)
				}
				// ParametersJsonSchema accepts a raw JSON-schema object, avoiding a
				// lossy conversion into genai's typed *Schema.
				fd.ParametersJsonSchema = schema
			}
			decls = append(decls, fd)
		}
		cfg.Tools = []*genai.Tool{{FunctionDeclarations: decls}}
	}

	// Schema-constrained output. Gemini cannot combine structured output
	// with function-calling, so we only attach the schema when the caller
	// didn't ask for tools — when they did, we silently fall back to the
	// prompt-embedded schema, which is the same behavior the caller would
	// have gotten before this field existed.
	if len(req.ResponseSchema) > 0 && g.caps.StructuredOutput && len(req.Tools) == 0 {
		var schema any
		if err := json.Unmarshal(req.ResponseSchema, &schema); err != nil {
			return Response{}, newAPIError("google", 0, 0, ErrInvalidRequest,
				"ResponseSchema: invalid JSON", err)
		}
		// ResponseJsonSchema accepts a raw JSON Schema object, mirroring
		// ParametersJsonSchema at ~88 — no lossy conversion to genai's
		// typed *Schema.
		cfg.ResponseMIMEType = "application/json"
		cfg.ResponseJsonSchema = schema
	}

	resp, err := g.client.Models.GenerateContent(ctx, g.model, contents, cfg)
	if err != nil {
		return Response{}, g.normalizeErr(err)
	}
	return g.toResponse(resp), nil
}

// toGoogleContents converts normalized messages into genai Contents. Gemini
// uses "user"/"model" roles; tool results are sent as user-turn
// functionResponse parts.
func toGoogleContents(msgs []Message) ([]*genai.Content, error) {
	out := make([]*genai.Content, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case RoleSystem:
			// Hoisted into SystemInstruction by the caller; if inline, attach as a
			// user turn to preserve content.
			out = append(out, &genai.Content{
				Role:  "user",
				Parts: []*genai.Part{{Text: m.Content}},
			})
		case RoleUser:
			out = append(out, &genai.Content{
				Role:  "user",
				Parts: []*genai.Part{{Text: m.Content}},
			})
		case RoleAssistant:
			parts := make([]*genai.Part, 0, 1+len(m.ToolCalls))
			if m.Content != "" {
				parts = append(parts, &genai.Part{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				var args map[string]any
				if len(tc.Arguments) > 0 {
					if err := json.Unmarshal(tc.Arguments, &args); err != nil {
						return nil, newAPIError("google", 0, 0, ErrInvalidRequest,
							"assistant tool call "+tc.Name+": invalid arguments JSON", err)
					}
				}
				parts = append(parts, &genai.Part{
					FunctionCall: &genai.FunctionCall{
						ID:   tc.ID,
						Name: tc.Name,
						Args: args,
					},
				})
			}
			out = append(out, &genai.Content{Role: "model", Parts: parts})
		case RoleToolResult:
			// Gemini expects the function result wrapped under an "output" key.
			resultObj := map[string]any{"output": m.Content}
			if m.IsError {
				resultObj = map[string]any{"error": m.Content}
			}
			out = append(out, &genai.Content{
				Role: "user",
				Parts: []*genai.Part{{
					FunctionResponse: &genai.FunctionResponse{
						ID:       m.ToolCallID,
						Name:     m.ToolCallID, // genai matches on name; ID is best-effort
						Response: resultObj,
					},
				}},
			})
		default:
			return nil, newAPIError("google", 0, 0, ErrInvalidRequest,
				"unknown message role "+string(m.Role), nil)
		}
	}
	return out, nil
}

func (g *googleAdapter) toResponse(resp *genai.GenerateContentResponse) Response {
	var out Response
	var text string
	var stop genai.FinishReason

	if len(resp.Candidates) > 0 {
		cand := resp.Candidates[0]
		stop = cand.FinishReason
		if cand.Content != nil {
			for _, p := range cand.Content.Parts {
				if p == nil {
					continue
				}
				if p.Text != "" {
					text += p.Text
				}
				if p.FunctionCall != nil {
					args, _ := json.Marshal(p.FunctionCall.Args)
					if len(args) == 0 || string(args) == "null" {
						args = json.RawMessage("{}")
					}
					out.ToolCalls = append(out.ToolCalls, ToolCall{
						ID:        p.FunctionCall.ID,
						Name:      p.FunctionCall.Name,
						Arguments: json.RawMessage(args),
					})
				}
			}
		}
	}
	out.Text = text
	if resp.UsageMetadata != nil {
		// promptTokenCount already INCLUDES cachedContentTokenCount (Gemini
		// convention), matching the normalized Usage semantics directly.
		// Gemini 2.x caches implicitly server-side; we only surface the hit
		// count — no explicit CachedContent management in this thin layer.
		out.Usage = Usage{
			InputTokens:          int64(resp.UsageMetadata.PromptTokenCount),
			OutputTokens:         int64(resp.UsageMetadata.CandidatesTokenCount),
			CacheReadInputTokens: int64(resp.UsageMetadata.CachedContentTokenCount),
		}
	}
	out.StopReason = mapGoogleStop(stop, len(out.ToolCalls) > 0)
	return out
}

func mapGoogleStop(reason genai.FinishReason, hasToolCalls bool) StopReason {
	switch reason {
	case genai.FinishReasonStop, genai.FinishReasonUnspecified, "":
		if hasToolCalls {
			return StopToolUse
		}
		return StopEndTurn
	case genai.FinishReasonMaxTokens:
		return StopMaxTokens
	case genai.FinishReasonSafety, genai.FinishReasonRecitation,
		genai.FinishReasonProhibitedContent, genai.FinishReasonBlocklist,
		genai.FinishReasonSPII:
		return StopError
	default:
		if hasToolCalls {
			return StopToolUse
		}
		return StopError
	}
}

func (g *googleAdapter) normalizeErr(err error) error {
	// genai returns APIError by value (not a pointer).
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		status := apiErr.Code
		kind := classifyStatus(status, apiErr.Message)
		// genai.APIError carries no *http.Response, so Retry-After is unavailable;
		// the retry wrapper falls back to exponential backoff.
		return newAPIError("google", status, 0, kind, apiErr.Message, err)
	}
	return newAPIError("google", 0, 0, ErrServer, err.Error(), err)
}

func googleCapabilities(model string) Capabilities {
	// Gemini supports parallel function calls and structured output. Prompt
	// caching is implicit (server-side, automatic on 2.x models); the adapter
	// surfaces cache hits via Usage.CacheReadInputTokens but does not manage
	// explicit CachedContent.
	return Capabilities{
		ContextWindow:     1_000_000,
		ParallelToolCalls: true,
		PromptCaching:     true,
		StructuredOutput:  true,
	}
}

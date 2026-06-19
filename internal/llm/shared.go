package llm

import (
	"encoding/json"
	"net/http"
	"time"
)

// This file collects the small parsing/assembly helpers shared by the
// Anthropic, OpenAI, and Google adapters. They replace near-duplicate blocks
// that had drifted apart and centralize the "what shape does each provider's
// SDK actually want" knowledge here. No framework or shared base struct —
// each adapter still owns its own buildParams / toResponse / normalizeErr;
// the helpers are called from those methods and nothing more.
//
// ---------------------------------------------------------------------------
// Provider capability matrix (deliberate, not a bug)
// ---------------------------------------------------------------------------
//
// (a) Native structured output. Anthropic has no response_format, so the
//     adapter injects a synthetic tool (see structuredOutputToolName) but
//     drops injection when the caller also supplies user tools — Anthropic
//     can combine tool_choice with user tools, but layering a synthetic
//     tool on top is ambiguous. Google cannot combine structured output
//     with function-calling at all, so the same drop applies. OpenAI
//     honors response_format alongside tools and does NOT drop. When the
//     caller wants native structured output AND user tools on Anthropic
//     or Google, RunJSON's repair round-trip drops tools to restore
//     native schema, then re-adds them on the next iteration.
//
// (b) CacheCreationInputTokens. Only Anthropic exposes a "cache write"
//     primitive (cache_creation_input_tokens); OpenAI and Google charge
//     cache writes implicitly with no separate counter, so this field
//     is 0 on those providers' Usage.
//
// (c) Default MaxTokens. Anthropic's API requires max_tokens; the
//     adapter applies a floor of 4096 when req.MaxTokens <= 0 (see
//     anthropicAdapter.buildParams). OpenAI and Google accept a 0/omitted
//     cap, so no default is applied there.
// ---------------------------------------------------------------------------

// parseToolParameters unmarshals a tool's JSON Schema and extracts the
// JSON-Schema "properties" and "required" keys, returning them in the shape
// the Anthropic SDK's ToolInputSchemaParam expects. Anthropic's schema
// representation is the decoded properties (not a {"type":"object",
// "properties":...} envelope), so the helper performs the unwrap.
//
// Adapters whose SDK expects the envelope verbatim (OpenAI's
// shared.FunctionParameters, Google's ParametersJsonSchema) call this for
// validation only and unmarshal params themselves for the wire — the unwrap
// performed here would change the on-the-wire shape and break those SDKs.
//
// A zero-length input is treated as "no schema" and returns nil values with
// no error, matching the original per-adapter guards (`if len(t.Parameters)
// > 0`).
func parseToolParameters(params json.RawMessage) (properties map[string]any, required []string, err error) {
	if len(params) == 0 {
		return nil, nil, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(params, &raw); err != nil {
		return nil, nil, err
	}
	// The Anthropic schema param carries the JSON-schema "properties" and
	// "required" within an object whose type is implicitly "object". The SDK
	// marshals Properties as the schema body; pass the decoded properties
	// (and required, if present) through.
	if p, ok := raw["properties"]; ok {
		if pm, ok := p.(map[string]any); ok {
			properties = pm
		}
	} else {
		// Caller passed a bare properties object rather than a full schema.
		properties = raw
	}
	if r, ok := raw["required"]; ok {
		if rs, ok := r.([]any); ok {
			for _, v := range rs {
				if s, ok := v.(string); ok {
					required = append(required, s)
				}
			}
		}
	}
	return properties, required, nil
}

// parseResponseSchema unmarshals a JSON Schema for schema-constrained output
// and resolves the schema name. The caller supplies defaultName — the
// provider's fallback when the user didn't name the schema — and the
// returned name is exactly that string. The helper does NOT inspect the
// raw schema for a "name" key; each adapter decides what name to use, so
// the resolution stays in one place per adapter.
func parseResponseSchema(raw json.RawMessage, defaultName string) (schema any, name string, err error) {
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, "", err
	}
	return schema, defaultName, nil
}

// normalizeSDKError folds the post-extraction assembly common to every
// adapter's normalizeErr: classify the status, parse Retry-After when the
// kind makes it useful, and wrap as a normalized *APIError. Each adapter
// supplies only the (status, msg, resp) extracted from its SDK error; resp
// is nil when the SDK error carries no *http.Response (e.g. genai.APIError),
// in which case Retry-After is unavailable and the retry wrapper falls
// back to exponential backoff. underlying is the original SDK error,
// preserved on the APIError for Unwrap chaining.
func normalizeSDKError(provider string, status int, msg string, resp *http.Response, underlying error) error {
	kind := classifyStatus(status, msg)
	var ra time.Duration
	if kind == ErrRateLimited || kind == ErrOverloaded {
		ra = parseRetryAfter(resp)
	}
	return newAPIError(provider, status, ra, kind, msg, underlying)
}

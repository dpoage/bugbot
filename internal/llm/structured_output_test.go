package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/dpoage/bugbot/internal/config"
)

// schemaForTest is a small, valid JSON Schema used across the
// structured-output tests. It declares a top-level object with a single
// required string property.
var schemaForTest = json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)

// withCaps returns a copy of base with StructuredOutput toggled. Used to
// construct adapter instances whose cap is off so the "no schema on the
// wire" assertion can run.
func withCaps(base Capabilities, structured bool) Capabilities {
	base.StructuredOutput = structured
	return base
}

// TestStructuredOutput_OpenAI_AttachesResponseFormat asserts that when
// ResponseSchema is set on a Request and the adapter's StructuredOutput
// capability is on, the OpenAI wire body carries response_format.type ==
// "json_schema" with the schema. Also asserts the body STILL carries
// response_format when user tools are present (per spec — OpenAI permits
// the combination and the adapter honors it).
func TestStructuredOutput_OpenAI_AttachesResponseFormat(t *testing.T) {
	t.Run("schema only", func(t *testing.T) {
		var captured map[string]any
		base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &captured)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(mockTextBody("openai", "ok", 1, 1)))
		})
		adapter := newOpenAIAdapter("gpt-test", openaiOptions{
			apiKey:   "k",
			baseURL:  base,
			provider: "openai",
			caps:     openAICapabilities("gpt-test"),
		})
		req := simpleRequest()
		req.ResponseSchema = schemaForTest
		req.ResponseSchemaName = "answer"
		if _, err := adapter.Complete(context.Background(), req); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		rf, ok := captured["response_format"].(map[string]any)
		if !ok {
			t.Fatalf("response_format missing or wrong type: %T", captured["response_format"])
		}
		if rf["type"] != "json_schema" {
			t.Errorf("response_format.type = %v, want json_schema", rf["type"])
		}
		js, ok := rf["json_schema"].(map[string]any)
		if !ok {
			t.Fatalf("response_format.json_schema missing or wrong type: %T", rf["json_schema"])
		}
		if js["name"] != "answer" {
			t.Errorf("json_schema.name = %v, want answer", js["name"])
		}
		// Strict is intentionally NOT set on the wire. The openai-go SDK uses
		// param.Opt[bool], so the zero value is omitted; the adapter's lenient
		// policy (no Strict assignment) means the field is absent, which the
		// OpenAI API treats as non-strict. Asserting presence would couple the
		// test to a specific SDK encoding choice; absence is what we want.
		if v, ok := js["strict"]; ok && v != false {
			t.Errorf("json_schema.strict = %v, want absent or false", v)
		}
		// The schema body must round-trip through to the wire.
		sch, ok := js["schema"].(map[string]any)
		if !ok {
			t.Fatalf("json_schema.schema missing or wrong type: %T", js["schema"])
		}
		if sch["type"] != "object" {
			t.Errorf("schema.type = %v, want object", sch["type"])
		}
	})

	t.Run("honored with tools", func(t *testing.T) {
		var captured map[string]any
		base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &captured)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(mockTextBody("openai", "ok", 1, 1)))
		})
		adapter := newOpenAIAdapter("gpt-test", openaiOptions{
			apiKey:   "k",
			baseURL:  base,
			provider: "openai",
			caps:     openAICapabilities("gpt-test"),
		})
		req := simpleRequest()
		req.ResponseSchema = schemaForTest
		req.Tools = []ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}
		if _, err := adapter.Complete(context.Background(), req); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if _, ok := captured["response_format"]; !ok {
			t.Error("response_format missing — schema must be honored alongside tools")
		}
		if _, ok := captured["tools"]; !ok {
			t.Error("tools missing — adapter dropped user tools")
		}
	})
}

// TestStructuredOutput_OpenAICompatible_AttachesResponseFormat mirrors the
// openai test for the openai-compatible factory. With the cap on (forced,
// because the conservative default is false), the wire body must carry
// response_format.
func TestStructuredOutput_OpenAICompatible_AttachesResponseFormat(t *testing.T) {
	var captured map[string]any
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockTextBody("openai-compatible", "ok", 1, 1)))
	})
	adapter := newOpenAIAdapter("llama-test", openaiOptions{
		apiKey:   "k",
		baseURL:  base,
		provider: "openai-compatible",
		caps:     withCaps(openAICompatibleCapabilities(), true),
	})
	req := simpleRequest()
	req.ResponseSchema = schemaForTest
	if _, err := adapter.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	rf, ok := captured["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format missing: %T", captured["response_format"])
	}
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type = %v, want json_schema", rf["type"])
	}
}

// TestStructuredOutput_OpenAI_GatedByCapability asserts that when the cap is
// OFF, the adapter silently drops ResponseSchema and no response_format field
// appears on the wire.
func TestStructuredOutput_OpenAI_GatedByCapability(t *testing.T) {
	var captured map[string]any
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockTextBody("openai", "ok", 1, 1)))
	})
	adapter := newOpenAIAdapter("gpt-test", openaiOptions{
		apiKey:   "k",
		baseURL:  base,
		provider: "openai",
		caps:     withCaps(openAICapabilities("gpt-test"), false),
	})
	req := simpleRequest()
	req.ResponseSchema = schemaForTest
	if _, err := adapter.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if v, ok := captured["response_format"]; ok {
		t.Errorf("response_format present with cap off: %v", v)
	}
}

// TestStructuredOutput_Google_AttachesSchema asserts that when ResponseSchema
// is set on a Gemini request with the cap on and NO tools, the wire body
// carries responseJsonSchema + responseMimeType="application/json".
func TestStructuredOutput_Google_AttachesSchema(t *testing.T) {
	var captured map[string]any
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockTextBody("google", "ok", 1, 1)))
	})
	adapter, err := newGoogleAdapter(context.Background(), "gemini-test", googleOptions{
		apiKey:  "k",
		baseURL: base,
	})
	if err != nil {
		t.Fatalf("newGoogleAdapter: %v", err)
	}
	req := simpleRequest()
	req.ResponseSchema = schemaForTest
	if _, err := adapter.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	gen, ok := captured["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig missing or wrong type: %T", captured["generationConfig"])
	}
	if gen["responseMimeType"] != "application/json" {
		t.Errorf("responseMimeType = %v, want application/json", gen["responseMimeType"])
	}
	if _, ok := gen["responseJsonSchema"]; !ok {
		t.Errorf("responseJsonSchema missing from wire body")
	}
}

// TestStructuredOutput_Google_SkippedWithTools asserts that when the caller
// supplies tools alongside a schema, the Gemini adapter omits both the
// schema and the mime type (Gemini can't combine structured output with
// function-calling), but still carries the user tools.
func TestStructuredOutput_Google_SkippedWithTools(t *testing.T) {
	var captured map[string]any
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockTextBody("google", "ok", 1, 1)))
	})
	adapter, err := newGoogleAdapter(context.Background(), "gemini-test", googleOptions{
		apiKey:  "k",
		baseURL: base,
	})
	if err != nil {
		t.Fatalf("newGoogleAdapter: %v", err)
	}
	req := simpleRequest()
	req.ResponseSchema = schemaForTest
	req.Tools = []ToolDef{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}
	if _, err := adapter.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	gen, ok := captured["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig missing: %T", captured["generationConfig"])
	}
	if _, ok := gen["responseJsonSchema"]; ok {
		t.Errorf("responseJsonSchema present with tools — Gemini can't combine")
	}
	if _, ok := gen["responseMimeType"]; ok {
		t.Errorf("responseMimeType present with tools — Gemini can't combine")
	}
	// Tools must still be present.
	if _, ok := captured["tools"]; !ok {
		t.Error("tools missing from wire body")
	}
}

// TestStructuredOutput_Anthropic_InjectsForcedTool asserts the wire body
// carries a single synthetic tool (the schema's properties become its
// input_schema) and tool_choice forcing that tool by name.
func TestStructuredOutput_Anthropic_InjectsForcedTool(t *testing.T) {
	var captured map[string]any
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	adapter := newAnthropicAdapter("claude-test", anthropicOptions{
		apiKey:  "k",
		baseURL: base,
	})
	req := simpleRequest()
	req.ResponseSchema = schemaForTest
	req.ResponseSchemaName = "emit_answer"
	if _, err := adapter.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	tools, ok := captured["tools"].([]any)
	if !ok {
		t.Fatalf("tools missing or wrong type: %T", captured["tools"])
	}
	if len(tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1 (synthetic tool)", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "emit_answer" {
		t.Errorf("synthetic tool name = %v, want emit_answer", tool["name"])
	}
	tc, ok := captured["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice missing or wrong type: %T", captured["tool_choice"])
	}
	if tc["type"] != "tool" {
		t.Errorf("tool_choice.type = %v, want tool", tc["type"])
	}
	if tc["name"] != "emit_answer" {
		t.Errorf("tool_choice.name = %v, want emit_answer", tc["name"])
	}
}

// TestStructuredOutput_Anthropic_DefaultToolName covers the case where the
// caller doesn't set ResponseSchemaName — the adapter must pick
// "emit_answer" as the synthetic tool name.
func TestStructuredOutput_Anthropic_DefaultToolName(t *testing.T) {
	var captured map[string]any
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	adapter := newAnthropicAdapter("claude-test", anthropicOptions{
		apiKey:  "k",
		baseURL: base,
	})
	req := simpleRequest()
	req.ResponseSchema = schemaForTest
	if _, err := adapter.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	tools, _ := captured["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "emit_answer" {
		t.Errorf("default synthetic tool name = %v, want emit_answer", tool["name"])
	}
}

// TestStructuredOutput_Anthropic_SkippedWithUserTools asserts that when the
// caller supplies user tools, the Anthropic adapter does NOT inject a
// synthetic tool (matching the safe fallback used by Gemini).
func TestStructuredOutput_Anthropic_SkippedWithUserTools(t *testing.T) {
	var captured map[string]any
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockTextBody("anthropic", "ok", 1, 1)))
	})
	adapter := newAnthropicAdapter("claude-test", anthropicOptions{
		apiKey:  "k",
		baseURL: base,
	})
	req := simpleRequest()
	req.ResponseSchema = schemaForTest
	req.Tools = []ToolDef{{Name: "user_tool", Parameters: json.RawMessage(`{"type":"object"}`)}}
	if _, err := adapter.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	tools, _ := captured["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1 (user tool only, no synthetic)", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "user_tool" {
		t.Errorf("tool name = %v, want user_tool (synthetic was injected!)", tool["name"])
	}
	if _, ok := captured["tool_choice"]; ok {
		t.Errorf("tool_choice present — synthetic tool_choice was injected despite user tools")
	}
}

// TestStructuredOutput_Anthropic_SurfacesForcedToolAsText asserts the
// response path: when the model returns the forced tool_use block, the
// adapter surfaces its `input` JSON as Response.Text (and clears the
// tool-call list), so RunJSON sees JSON text instead of a tool call.
func TestStructuredOutput_Anthropic_SurfacesForcedToolAsText(t *testing.T) {
	forcedArgs := `{"answer":"42"}`
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockToolCallBody("anthropic", "tu_1", "emit_answer", forcedArgs, 5, 7)))
	})
	adapter := newAnthropicAdapter("claude-test", anthropicOptions{
		apiKey:  "k",
		baseURL: base,
	})
	req := simpleRequest()
	req.ResponseSchema = schemaForTest
	resp, err := adapter.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %d, want 0 (forced tool should be converted to text)", len(resp.ToolCalls))
	}
	if resp.Text != forcedArgs {
		t.Errorf("Text = %q, want %q", resp.Text, forcedArgs)
	}
	if resp.StopReason != StopEndTurn {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, StopEndTurn)
	}
}

// TestStructuredOutput_Anthropic_LeavesOtherToolCallsAlone asserts the
// defensive path: if the model returns a tool call that ISN'T our synthetic
// one, it stays as a tool call.
func TestStructuredOutput_Anthropic_LeavesOtherToolCallsAlone(t *testing.T) {
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockToolCallBody("anthropic", "tu_x", "some_other_tool", `{}`, 1, 1)))
	})
	adapter := newAnthropicAdapter("claude-test", anthropicOptions{
		apiKey:  "k",
		baseURL: base,
	})
	req := simpleRequest()
	req.ResponseSchema = schemaForTest
	resp, err := adapter.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "some_other_tool" {
		t.Errorf("ToolCalls = %+v, want one call named some_other_tool", resp.ToolCalls)
	}
}

// hasNestedField returns a closure suitable for use with runOne that
// reports whether `body` contains the nested key path (e.g. ["a","b"] checks
// body["a"]["b"]). The leaf must be present and non-nil.
func hasNestedField(keys ...string) func(map[string]any) bool {
	return func(body map[string]any) bool {
		cur := body
		for i, k := range keys {
			v, ok := cur[k]
			if !ok || v == nil {
				return false
			}
			if i == len(keys)-1 {
				return true
			}
			m, ok := v.(map[string]any)
			if !ok {
				return false
			}
			cur = m
		}
		return false
	}
}

// TestStructuredOutput_AllGatedByCapability walks each provider with
// StructuredOutput=false and asserts the schema is dropped on the wire.
// This is the cross-adapter safety net: if any adapter ever forgets to gate
// the schema on the cap, this test surfaces it.
func TestStructuredOutput_AllGatedByCapability(t *testing.T) {
	// Pick a response body shape that works for the provider under test.
	respBody := func(name string) string {
		switch name {
		case "anthropic":
			return `{"id":"m","type":"message","role":"assistant","model":"x","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		case "google":
			return `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
		default:
			return `{"id":"x","object":"chat.completion","created":1,"model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
		}
	}
	runOne := func(t *testing.T, name string, build func(base string) Client, leak func(map[string]any) bool) {
		var captured map[string]any
		base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &captured)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(respBody(name)))
		})
		client := build(base)
		req := simpleRequest()
		req.ResponseSchema = schemaForTest
		if _, err := client.Complete(context.Background(), req); err != nil {
			t.Fatalf("%s Complete: %v", name, err)
		}
		if leak(captured) {
			t.Errorf("%s: schema leaked onto wire despite cap off: %v", name, captured)
		}
	}

	t.Run("openai", func(t *testing.T) {
		runOne(t, "openai", func(base string) Client {
			return newOpenAIAdapter("gpt-test", openaiOptions{
				apiKey: "k", baseURL: base, provider: "openai",
				caps: withCaps(openAICapabilities("gpt-test"), false),
			})
		}, hasNestedField("response_format"))
	})
	t.Run("openai-compatible", func(t *testing.T) {
		runOne(t, "openai-compatible", func(base string) Client {
			return newOpenAIAdapter("llama-test", openaiOptions{
				apiKey: "k", baseURL: base, provider: "openai-compatible",
				caps: withCaps(openAICompatibleCapabilities(), false),
			})
		}, hasNestedField("response_format"))
	})
	t.Run("google", func(t *testing.T) {
		runOne(t, "google", func(base string) Client {
			a, err := newGoogleAdapter(context.Background(), "gemini-test", googleOptions{
				apiKey: "k", baseURL: base,
			})
			if err != nil {
				t.Fatalf("newGoogleAdapter: %v", err)
			}
			a.caps.StructuredOutput = false
			return a
		}, hasNestedField("generationConfig", "responseJsonSchema"))
	})
	t.Run("anthropic", func(t *testing.T) {
		runOne(t, "anthropic", func(base string) Client {
			a := newAnthropicAdapter("claude-test", anthropicOptions{
				apiKey: "k", baseURL: base,
			})
			a.caps.StructuredOutput = false
			return a
		}, func(body map[string]any) bool {
			tools, ok := body["tools"].([]any)
			return ok && len(tools) > 0
		})
	})
}

// TestStructuredOutput_ConfigOverride_FlipsOpenAICompatibleCapabilities
// asserts the registry-level integration: a Provider with
// StructuredOutput=&true causes NewClient to return an openai-compatible
// client whose capabilities report StructuredOutput=true.
func TestStructuredOutput_ConfigOverride_FlipsOpenAICompatibleCapabilities(t *testing.T) {
	tr := true
	provider := config.Provider{
		Type:             config.ProviderOpenAICompatible,
		BaseURL:          "http://example.invalid",
		APIKeyEnv:        "X",
		StructuredOutput: &tr,
	}
	client, err := NewClient(context.Background(), provider, "test", "llama3", "k", Options{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if !client.Capabilities().StructuredOutput {
		t.Error("Capabilities().StructuredOutput = false, want true (config override should flip it on)")
	}
}

// TestStructuredOutput_ConfigOverride_OffUnaffected asserts that an explicit
// false in the config suppresses StructuredOutput even on a first-party
// provider that would default to true.
func TestStructuredOutput_ConfigOverride_OffUnaffected(t *testing.T) {
	f := false
	provider := config.Provider{
		Type:             config.ProviderOpenAI,
		APIKeyEnv:        "X",
		StructuredOutput: &f,
	}
	client, err := NewClient(context.Background(), provider, "test", "gpt-test", "k", Options{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client.Capabilities().StructuredOutput {
		t.Error("Capabilities().StructuredOutput = true, want false (config override should force it off)")
	}
}

// TestStructuredOutput_ConfigOverride_UnaffectedWhenUnset is a regression
// guard: a provider without the override must keep its default capability
// (true for first-party, false for openai-compatible).
func TestStructuredOutput_ConfigOverride_UnaffectedWhenUnset(t *testing.T) {
	t.Run("first-party default true", func(t *testing.T) {
		provider := config.Provider{Type: config.ProviderOpenAI, APIKeyEnv: "X"}
		client, err := NewClient(context.Background(), provider, "t", "gpt-x", "k", Options{})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if !client.Capabilities().StructuredOutput {
			t.Error("first-party default should be true")
		}
	})
	t.Run("openai-compatible default false", func(t *testing.T) {
		provider := config.Provider{Type: config.ProviderOpenAICompatible, BaseURL: "http://x", APIKeyEnv: "X"}
		client, err := NewClient(context.Background(), provider, "t", "llama", "k", Options{})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if client.Capabilities().StructuredOutput {
			t.Error("openai-compatible default should be false")
		}
	})
}

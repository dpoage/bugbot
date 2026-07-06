package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// --- cross-adapter cache-usage normalization --------------------------------

// cacheUsageBody returns a text response body carrying cache-hit usage in the
// provider's native convention:
//
//   - anthropic: input_tokens EXCLUDES cache reads/writes, reported separately;
//   - openai/compatible: prompt_tokens INCLUDES prompt_tokens_details.cached_tokens;
//   - google: promptTokenCount INCLUDES cachedContentTokenCount.
//
// Every body below describes the SAME logical call — a 130-token prompt of
// which 100 came from cache and (anthropic only) 20 were written to it — so a
// single normalized expectation runs against every adapter.
func cacheUsageBody(provider string) string {
	switch provider {
	case "anthropic":
		b, _ := json.Marshal(map[string]any{
			"id": "msg_c", "type": "message", "role": "assistant", "model": "claude-test",
			"content":     []any{map[string]any{"type": "text", "text": "hi"}},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens": 10, "output_tokens": 5,
				"cache_read_input_tokens": 100, "cache_creation_input_tokens": 20,
			},
		})
		return string(b)
	case "openai", "openai-compatible":
		b, _ := json.Marshal(map[string]any{
			"id": "c", "object": "chat.completion", "created": 1, "model": "gpt-test",
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "hi"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens": 130, "completion_tokens": 5, "total_tokens": 135,
				"prompt_tokens_details": map[string]any{"cached_tokens": 100},
			},
		})
		return string(b)
	case "google":
		b, _ := json.Marshal(map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": "hi"}}},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{
				"promptTokenCount": 130, "candidatesTokenCount": 5, "totalTokenCount": 135,
				"cachedContentTokenCount": 100,
			},
		})
		return string(b)
	}
	return "{}"
}

func TestConformance_CacheUsageNormalization(t *testing.T) {
	for _, f := range allAdapters() {
		t.Run(f.name, func(t *testing.T) {
			base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(cacheUsageBody(f.name)))
			})
			client := f.build(t, base)

			resp, err := client.Complete(context.Background(), simpleRequest())
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			// InputTokens must be the inclusive total regardless of how the
			// provider splits it on the wire.
			if resp.Usage.InputTokens != 130 {
				t.Errorf("InputTokens = %d, want 130 (inclusive of cached)", resp.Usage.InputTokens)
			}
			if resp.Usage.OutputTokens != 5 {
				t.Errorf("OutputTokens = %d, want 5", resp.Usage.OutputTokens)
			}
			if resp.Usage.CacheReadInputTokens != 100 {
				t.Errorf("CacheReadInputTokens = %d, want 100", resp.Usage.CacheReadInputTokens)
			}
			wantCreated := int64(0)
			if f.name == "anthropic" {
				wantCreated = 20
			}
			if resp.Usage.CacheCreationInputTokens != wantCreated {
				t.Errorf("CacheCreationInputTokens = %d, want %d", resp.Usage.CacheCreationInputTokens, wantCreated)
			}
		})
	}
}

// TestOpenAICompatible_NoCachedTokensField covers endpoints (Ollama, vLLM, many
// MiniMax deployments) that omit prompt_tokens_details entirely: usage must
// still parse, with zero cache reads.
func TestOpenAICompatible_NoCachedTokensField(t *testing.T) {
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockTextBody("openai-compatible", "hi", 130, 5)))
	})
	client := newOpenAIAdapter("llama-test", openaiOptions{
		apiKey: "test-key", baseURL: base,
		provider: "openai-compatible", caps: openAICompatibleCapabilities("llama-test"),
	})

	resp, err := client.Complete(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.InputTokens != 130 || resp.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v, want {130 5}", resp.Usage)
	}
	if resp.Usage.CacheReadInputTokens != 0 || resp.Usage.CacheCreationInputTokens != 0 {
		t.Errorf("cache fields = %d/%d, want 0/0 when the endpoint omits them",
			resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)
	}
}

// --- anthropic cache breakpoint placement -----------------------------------

// breakpointRequest is a representative mid-loop agent request: a system
// prompt, three tools, and a conversation with a completed tool round-trip.
func breakpointRequest() Request {
	return Request{
		System: "you are a careful bug finder",
		Messages: []Message{
			{Role: RoleUser, Content: "audit these files"},
			{Role: RoleAssistant, Content: "reading", ToolCalls: []ToolCall{
				{ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`)},
				{ID: "c2", Name: "read_file", Arguments: json.RawMessage(`{"path":"b.go"}`)},
			}},
			{Role: RoleToolResult, ToolCallID: "c1", Content: "package a"},
			{Role: RoleToolResult, ToolCallID: "c2", Content: "package b"},
		},
		Tools: []ToolDef{
			{Name: "read_file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
			{Name: "list_dir", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
			{Name: "grep", Parameters: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"}}}`)},
		},
		MaxTokens: 64,
	}
}

// cacheControlOf extracts the "cache_control" member of a serialized block (or
// nil when absent).
func cacheControlOf(t *testing.T, block any) map[string]any {
	t.Helper()
	m, ok := block.(map[string]any)
	if !ok {
		t.Fatalf("block is not an object: %T", block)
	}
	cc, _ := m["cache_control"].(map[string]any)
	return cc
}

func TestAnthropic_CacheBreakpointPlacement(t *testing.T) {
	a := newAnthropicAdapter("claude-test", anthropicOptions{apiKey: "k"})
	params, err := a.buildParams(breakpointRequest())
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	var body struct {
		System   []any `json:"system"`
		Tools    []any `json:"tools"`
		Messages []struct {
			Role    string `json:"role"`
			Content []any  `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal serialized params: %v", err)
	}

	// Tools: only the LAST definition carries the breakpoint (it caches the
	// whole tool block, which serializes ahead of system/messages).
	if n := len(body.Tools); n != 3 {
		t.Fatalf("tools = %d, want 3", n)
	}
	for i, tool := range body.Tools[:2] {
		if cc := cacheControlOf(t, tool); cc != nil {
			t.Errorf("tool %d unexpectedly has cache_control", i)
		}
	}
	if cc := cacheControlOf(t, body.Tools[2]); cc == nil || cc["type"] != "ephemeral" {
		t.Errorf("last tool cache_control = %v, want ephemeral", cc)
	}

	// System: the last (only) block carries the breakpoint.
	if n := len(body.System); n != 1 {
		t.Fatalf("system blocks = %d, want 1", n)
	}
	if cc := cacheControlOf(t, body.System[0]); cc == nil || cc["type"] != "ephemeral" {
		t.Errorf("system cache_control = %v, want ephemeral", cc)
	}

	// Conversation: after coalescing the two tool results into one user turn,
	// messages are [user, assistant, user]. The LAST block of each of the final
	// two messages carries a breakpoint; nothing earlier does.
	if n := len(body.Messages); n != 3 {
		t.Fatalf("messages = %d, want 3 (tool results coalesced)", n)
	}
	for _, block := range body.Messages[0].Content {
		if cc := cacheControlOf(t, block); cc != nil {
			t.Error("first message unexpectedly has cache_control")
		}
	}
	for mi := 1; mi <= 2; mi++ {
		blocks := body.Messages[mi].Content
		for bi, block := range blocks {
			cc := cacheControlOf(t, block)
			if bi == len(blocks)-1 {
				if cc == nil || cc["type"] != "ephemeral" {
					t.Errorf("message %d last block cache_control = %v, want ephemeral", mi, cc)
				}
			} else if cc != nil {
				t.Errorf("message %d block %d unexpectedly has cache_control", mi, bi)
			}
		}
	}

	// Anthropic allows at most 4 breakpoints per request; count every marker in
	// the serialized body to guard against accidental extras.
	if got := strings.Count(string(raw), `"cache_control"`); got != 4 {
		t.Errorf("total cache_control markers = %d, want exactly 4", got)
	}
}

// TestAnthropic_CacheBreakpoints_MinimalRequest covers the loop's first
// iteration: no tools, one user message. Breakpoints degrade gracefully —
// system + the single message — without exceeding the budget or panicking.
func TestAnthropic_CacheBreakpoints_MinimalRequest(t *testing.T) {
	a := newAnthropicAdapter("claude-test", anthropicOptions{apiKey: "k"})
	params, err := a.buildParams(Request{
		System:    "sys",
		Messages:  []Message{{Role: RoleUser, Content: "hello"}},
		MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	if got := strings.Count(string(raw), `"cache_control"`); got != 2 {
		t.Errorf("total cache_control markers = %d, want 2 (system + sole message)", got)
	}
}

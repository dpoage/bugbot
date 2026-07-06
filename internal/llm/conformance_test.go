package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// adapterFactory builds an adapter Client whose underlying SDK is redirected to
// the given base URL (an httptest server). Each provider implements one.
//
// We test the raw adapter (no retry/recorder wrappers) so the conformance suite
// exercises wire-format mapping and error normalization directly; wrapper
// behavior has dedicated tests.
type adapterFactory struct {
	name  string
	build func(t *testing.T, baseURL string) Client
}

// allAdapters returns a factory per provider, each pointed at an httptest
// server via the unexported option structs. The genai backend needs its base
// URL to end without a path; the SDK appends the version + model path itself.
func allAdapters() []adapterFactory {
	return []adapterFactory{
		{
			name: "anthropic",
			build: func(t *testing.T, baseURL string) Client {
				return newAnthropicAdapter("claude-test", anthropicOptions{
					apiKey:  "test-key",
					baseURL: baseURL,
				})
			},
		},
		{
			name: "openai",
			build: func(t *testing.T, baseURL string) Client {
				return newOpenAIAdapter("gpt-test", openaiOptions{
					apiKey:   "test-key",
					baseURL:  baseURL,
					provider: "openai",
					caps:     openAICapabilities("gpt-test"),
				})
			},
		},
		{
			name: "openai-compatible",
			build: func(t *testing.T, baseURL string) Client {
				return newOpenAIAdapter("llama-test", openaiOptions{
					apiKey:   "test-key",
					baseURL:  baseURL,
					provider: "openai-compatible",
					caps:     openAICompatibleCapabilities("llama-test"),
				})
			},
		},
		{
			name: "google",
			build: func(t *testing.T, baseURL string) Client {
				c, err := newGoogleAdapter(context.Background(), "gemini-test", googleOptions{
					apiKey:  "test-key",
					baseURL: baseURL,
				})
				if err != nil {
					t.Fatalf("build google adapter: %v", err)
				}
				return c
			},
		},
	}
}

// --- per-provider mock response builders -----------------------------------
//
// Each returns a JSON body in that provider's wire format. The conformance
// tests pick the right builder by provider name, so a single logical assertion
// runs against every adapter.

// mockText returns a successful text-only response body for the named provider.
func mockTextBody(provider, text string, inTok, outTok int64) string {
	switch provider {
	case "anthropic":
		b, _ := json.Marshal(map[string]any{
			"id":            "msg_1",
			"type":          "message",
			"role":          "assistant",
			"model":         "claude-test",
			"content":       []any{map[string]any{"type": "text", "text": text}},
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": inTok, "output_tokens": outTok},
		})
		return string(b)
	case "openai", "openai-compatible":
		b, _ := json.Marshal(map[string]any{
			"id":      "chatcmpl-1",
			"object":  "chat.completion",
			"created": 1,
			"model":   "gpt-test",
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": text},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": inTok, "completion_tokens": outTok, "total_tokens": inTok + outTok},
		})
		return string(b)
	case "google":
		b, _ := json.Marshal(map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{"promptTokenCount": inTok, "candidatesTokenCount": outTok, "totalTokenCount": inTok + outTok},
		})
		return string(b)
	}
	return "{}"
}

// mockToolCallBody returns a response in which the model requests a single tool
// call named toolName with the given raw-JSON arguments.
func mockToolCallBody(provider, callID, toolName, argsJSON string, inTok, outTok int64) string {
	switch provider {
	case "anthropic":
		var args any
		_ = json.Unmarshal([]byte(argsJSON), &args)
		b, _ := json.Marshal(map[string]any{
			"id":    "msg_2",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-test",
			"content": []any{map[string]any{
				"type":  "tool_use",
				"id":    callID,
				"name":  toolName,
				"input": args,
			}},
			"stop_reason": "tool_use",
			"usage":       map[string]any{"input_tokens": inTok, "output_tokens": outTok},
		})
		return string(b)
	case "openai", "openai-compatible":
		b, _ := json.Marshal(map[string]any{
			"id":      "chatcmpl-2",
			"object":  "chat.completion",
			"created": 1,
			"model":   "gpt-test",
			"choices": []any{map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []any{map[string]any{
						"id":       callID,
						"type":     "function",
						"function": map[string]any{"name": toolName, "arguments": argsJSON},
					}},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{"prompt_tokens": inTok, "completion_tokens": outTok, "total_tokens": inTok + outTok},
		})
		return string(b)
	case "google":
		var args map[string]any
		_ = json.Unmarshal([]byte(argsJSON), &args)
		b, _ := json.Marshal(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{"role": "model", "parts": []any{map[string]any{
					"functionCall": map[string]any{"id": callID, "name": toolName, "args": args},
				}}},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{"promptTokenCount": inTok, "candidatesTokenCount": outTok, "totalTokenCount": inTok + outTok},
		})
		return string(b)
	}
	return "{}"
}

// errorBody returns an error response body for the provider at the given status.
func errorBody(provider string, status int, message string) string {
	switch provider {
	case "anthropic":
		b, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "rate_limit_error", "message": message},
		})
		return string(b)
	case "openai", "openai-compatible":
		b, _ := json.Marshal(map[string]any{
			"error": map[string]any{"message": message, "type": "rate_limit_error", "code": nil, "param": nil},
		})
		return string(b)
	case "google":
		b, _ := json.Marshal(map[string]any{
			"error": map[string]any{"code": status, "message": message, "status": "RESOURCE_EXHAUSTED"},
		})
		return string(b)
	}
	return "{}"
}

// newServer starts an httptest server whose handler is invoked for every
// request and returns it plus its base URL.
func newServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func simpleRequest() Request {
	return Request{
		System:    "you are a test",
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
		MaxTokens: 64,
	}
}

// --- conformance tests ------------------------------------------------------

func TestConformance_TextCompletion(t *testing.T) {
	for _, f := range allAdapters() {
		t.Run(f.name, func(t *testing.T) {
			base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(mockTextBody(f.name, "hello world", 12, 5)))
			})
			client := f.build(t, base)

			resp, err := client.Complete(context.Background(), simpleRequest())
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.Text != "hello world" {
				t.Errorf("Text = %q, want %q", resp.Text, "hello world")
			}
			if resp.StopReason != StopEndTurn {
				t.Errorf("StopReason = %q, want %q", resp.StopReason, StopEndTurn)
			}
			if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 5 {
				t.Errorf("Usage = %+v, want {12 5}", resp.Usage)
			}
			if len(resp.ToolCalls) != 0 {
				t.Errorf("ToolCalls = %d, want 0", len(resp.ToolCalls))
			}
		})
	}
}

func TestConformance_ToolCallRoundTrip(t *testing.T) {
	const (
		callID   = "call_abc"
		toolName = "read_file"
		argsJSON = `{"path":"/etc/hosts"}`
	)
	for _, f := range allAdapters() {
		t.Run(f.name, func(t *testing.T) {
			var captured map[string]any
			base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				// Decode the request to verify the round-tripped assistant tool_use
				// + tool result reach the wire without error. We don't assert exact
				// shape (it differs per provider) — just that the SDK serialized a
				// valid body.
				_ = json.NewDecoder(r.Body).Decode(&captured)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(mockTextBody(f.name, "done", 20, 8)))
			})
			client := f.build(t, base)

			// First, the model requests a tool. We simulate the prior assistant
			// tool_use and the tool result in the conversation, then expect a
			// normal completion. This exercises the assistant tool_use ->
			// tool-result serialization path in each adapter.
			req := Request{
				System: "sys",
				Messages: []Message{
					{Role: RoleUser, Content: "read the hosts file"},
					{Role: RoleAssistant, ToolCalls: []ToolCall{{
						ID: callID, Name: toolName, Arguments: json.RawMessage(argsJSON),
					}}},
					{Role: RoleToolResult, ToolCallID: callID, Content: "127.0.0.1 localhost"},
				},
				Tools: []ToolDef{{
					Name:        toolName,
					Description: "Read a file",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
				}},
				MaxTokens: 128,
			}

			resp, err := client.Complete(context.Background(), req)
			if err != nil {
				t.Fatalf("Complete (round-trip): %v", err)
			}
			if resp.Text != "done" {
				t.Errorf("Text = %q, want %q", resp.Text, "done")
			}
			if captured == nil {
				t.Errorf("server received no decodable request body")
			}
		})
	}
}

func TestConformance_ToolCallResponse(t *testing.T) {
	const (
		callID   = "call_xyz"
		toolName = "search"
		argsJSON = `{"query":"bug"}`
	)
	for _, f := range allAdapters() {
		t.Run(f.name, func(t *testing.T) {
			base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(mockToolCallBody(f.name, callID, toolName, argsJSON, 30, 10)))
			})
			client := f.build(t, base)

			req := Request{
				Messages: []Message{{Role: RoleUser, Content: "find bugs"}},
				Tools: []ToolDef{{
					Name:       toolName,
					Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
				}},
				MaxTokens: 64,
			}
			resp, err := client.Complete(context.Background(), req)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.StopReason != StopToolUse {
				t.Errorf("StopReason = %q, want %q", resp.StopReason, StopToolUse)
			}
			if len(resp.ToolCalls) != 1 {
				t.Fatalf("ToolCalls = %d, want 1", len(resp.ToolCalls))
			}
			tc := resp.ToolCalls[0]
			if tc.Name != toolName {
				t.Errorf("ToolCall.Name = %q, want %q", tc.Name, toolName)
			}
			// callID round-trips for anthropic/openai; genai may not echo IDs, so
			// only assert it where the provider supports it.
			if f.name != "google" && tc.ID != callID {
				t.Errorf("ToolCall.ID = %q, want %q", tc.ID, callID)
			}
			// Arguments must be parseable JSON matching what we sent.
			var got map[string]any
			if err := json.Unmarshal(tc.Arguments, &got); err != nil {
				t.Fatalf("ToolCall.Arguments not valid JSON: %v (raw %q)", err, tc.Arguments)
			}
			if got["query"] != "bug" {
				t.Errorf("ToolCall.Arguments query = %v, want %q", got["query"], "bug")
			}
		})
	}
}

func TestConformance_ErrorNormalization_RateLimit(t *testing.T) {
	for _, f := range allAdapters() {
		t.Run(f.name, func(t *testing.T) {
			base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(errorBody(f.name, http.StatusTooManyRequests, "slow down")))
			})
			client := f.build(t, base)

			_, err := client.Complete(context.Background(), simpleRequest())
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, ErrRateLimited) {
				t.Errorf("error = %v, want ErrRateLimited", err)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error is not *APIError: %T", err)
			}
			if apiErr.StatusCode != http.StatusTooManyRequests {
				t.Errorf("StatusCode = %d, want 429", apiErr.StatusCode)
			}
			// Anthropic and OpenAI expose the *http.Response so Retry-After is
			// parsed; genai does not, so RetryAfter is 0 there.
			if f.name == "google" {
				if apiErr.RetryAfter != 0 {
					t.Errorf("google RetryAfter = %v, want 0 (no http.Response available)", apiErr.RetryAfter)
				}
			} else if apiErr.RetryAfter != 7e9 { // 7s in ns
				t.Errorf("RetryAfter = %v, want 7s", apiErr.RetryAfter)
			}
			// No API key should leak into the normalized message.
			if strings.Contains(apiErr.Error(), "test-key") {
				t.Errorf("error message leaked API key: %q", apiErr.Error())
			}
		})
	}
}

func TestConformance_ErrorNormalization_Auth(t *testing.T) {
	for _, f := range allAdapters() {
		t.Run(f.name, func(t *testing.T) {
			base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(errorBody(f.name, http.StatusUnauthorized, "bad key")))
			})
			client := f.build(t, base)

			_, err := client.Complete(context.Background(), simpleRequest())
			if !errors.Is(err, ErrAuth) {
				t.Errorf("error = %v, want ErrAuth", err)
			}
		})
	}
}

func TestConformance_StopReasonMaxTokens(t *testing.T) {
	for _, f := range allAdapters() {
		t.Run(f.name, func(t *testing.T) {
			base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(maxTokensBody(f.name)))
			})
			client := f.build(t, base)
			resp, err := client.Complete(context.Background(), simpleRequest())
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.StopReason != StopMaxTokens {
				t.Errorf("StopReason = %q, want %q", resp.StopReason, StopMaxTokens)
			}
		})
	}
}

func maxTokensBody(provider string) string {
	switch provider {
	case "anthropic":
		b, _ := json.Marshal(map[string]any{
			"id": "m", "type": "message", "role": "assistant", "model": "claude-test",
			"content":     []any{map[string]any{"type": "text", "text": "trunc"}},
			"stop_reason": "max_tokens",
			"usage":       map[string]any{"input_tokens": 1, "output_tokens": 64},
		})
		return string(b)
	case "openai", "openai-compatible":
		b, _ := json.Marshal(map[string]any{
			"id": "c", "object": "chat.completion", "created": 1, "model": "gpt-test",
			"choices": []any{map[string]any{"index": 0,
				"message": map[string]any{"role": "assistant", "content": "trunc"}, "finish_reason": "length"}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 64, "total_tokens": 65},
		})
		return string(b)
	case "google":
		b, _ := json.Marshal(map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": "trunc"}}},
				"finishReason": "MAX_TOKENS"}},
			"usageMetadata": map[string]any{"promptTokenCount": 1, "candidatesTokenCount": 64, "totalTokenCount": 65},
		})
		return string(b)
	}
	return "{}"
}

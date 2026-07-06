package llm

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestNewClient_OpenAICompatibleSerializesToolCalls(t *testing.T) {
	// An openai-compatible endpoint defaults to ParallelToolCalls=false, and
	// NewClient wires WithSerializedToolCalls as the outermost decorator for
	// that capability profile. The test asserts the property end-to-end:
	// (1) the returned client advertises ParallelToolCalls=false; and
	// (2) a multi-tool-call HTTP response is truncated to one tool call by
	//     the time Complete returns, with no caller-side wrapping needed.
	multiBody := `{
		"id": "chatcmpl-multi",
		"object": "chat.completion",
		"created": 1,
		"model": "llama3",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [
					{"id": "call_1", "type": "function",
					 "function": {"name": "read_file", "arguments": "{}"}},
					{"id": "call_2", "type": "function",
					 "function": {"name": "list_dir", "arguments": "{}"}}
				]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
	}`
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(multiBody))
	})
	spec := ProviderSpec{Type: ProviderOpenAICompatible, BaseURL: base}
	client, err := NewClient(context.Background(), spec, "ollama", "llama3", "key", Options{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client.Capabilities().ParallelToolCalls {
		t.Error("openai-compatible client should report ParallelToolCalls=false (wrapper applied)")
	}
	// The wrapper must truncate the parallel tool calls down to one.
	resp, err := client.Complete(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1 (truncated by WithSerializedToolCalls)", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "call_1" {
		t.Errorf("kept tool call ID = %q, want first (call_1)", resp.ToolCalls[0].ID)
	}
}

// TestNewClient_RequestTimeoutWiring asserts that an explicit RequestTimeout in
// opts.Retry is honoured by NewClient and threads through to the *retryClient
// layer. This is a white-box test (package llm) so it can type-assert *retryClient
// directly without an exported accessor.
//
// Anthropic reports ParallelToolCalls=true so WithSerializedToolCalls is a
// passthrough; the outermost client is the *retryClient itself.
func TestNewClient_RequestTimeoutWiring(t *testing.T) {
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockTextBody("anthropic", "ok", 1, 1)))
	})
	t.Setenv("UNUSED_KEY", "sk-test")

	spec := ProviderSpec{Type: ProviderAnthropic, BaseURL: base}

	// Zero MaxAttempts triggers the DefaultRetryConfig reset; RequestTimeout
	// must be the package default.
	c, err := NewClient(context.Background(), spec, "p", "m", "sk-test", Options{})
	if err != nil {
		t.Fatalf("NewClient(default): %v", err)
	}
	rc, ok := c.(*retryClient)
	if !ok {
		t.Fatalf("NewClient returned %T, want *retryClient (anthropic skips serializing wrapper)", c)
	}
	if rc.cfg.RequestTimeout != DefaultRequestTimeout {
		t.Errorf("default RequestTimeout = %v, want %v", rc.cfg.RequestTimeout, DefaultRequestTimeout)
	}

	// Explicit timeout: supplied value must survive the construction path.
	want := 42 * time.Second
	retryCfg := DefaultRetryConfig()
	retryCfg.RequestTimeout = want
	c, err = NewClient(context.Background(), spec, "p", "m", "sk-test", Options{Retry: retryCfg})
	if err != nil {
		t.Fatalf("NewClient(explicit): %v", err)
	}
	rc, ok = c.(*retryClient)
	if !ok {
		t.Fatalf("NewClient returned %T, want *retryClient", c)
	}
	if rc.cfg.RequestTimeout != want {
		t.Errorf("explicit RequestTimeout = %v, want %v", rc.cfg.RequestTimeout, want)
	}
	if rc.cfg.MaxAttempts != DefaultRetryConfig().MaxAttempts {
		t.Errorf("MaxAttempts = %d, want default %d", rc.cfg.MaxAttempts, DefaultRetryConfig().MaxAttempts)
	}
}

// TestOpenAIContextWindow_PerModel asserts that openAIContextWindow returns
// distinct values for models with different context sizes, and that unknown
// models get the 128k default. This proves the capability constructors consult
// the model parameter (Part B — scaleFinderForContext fires correctly for small
// models like gpt-4 which is 8k, not 128k).
func TestOpenAIContextWindow_PerModel(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"gpt-4", 8_192},
		{"gpt-4-32k", 32_768},
		{"gpt-4o", 128_000},
		{"o1", 200_000},
		{"gpt-4.1", 1_047_576},
		{"unknown-model-xyz", 128_000}, // default fallback
	}
	for _, tc := range cases {
		caps := openAICapabilities(tc.model)
		if caps.ContextWindow != tc.want {
			t.Errorf("openAICapabilities(%q).ContextWindow = %d, want %d", tc.model, caps.ContextWindow, tc.want)
		}
	}
}

package llm

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/config"
)

func TestResolveRole_MixedProviders(t *testing.T) {
	// Two servers: an Anthropic-shaped one for finder, an OpenAI-shaped one for
	// verifier. This proves roles resolve across different providers from one
	// config.
	anthropicBase := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockTextBody("anthropic", "finder-says-hi", 10, 4)))
	})
	openaiBase := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockTextBody("openai", "verifier-says-hi", 20, 8)))
	})

	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"claude": {Type: config.ProviderAnthropic, BaseURL: anthropicBase, APIKeyEnv: "TEST_ANTHROPIC_KEY"},
			"oai":    {Type: config.ProviderOpenAI, BaseURL: openaiBase, APIKeyEnv: "TEST_OPENAI_KEY"},
		},
		Roles: config.Roles{
			Finder:     config.RoleModel{Provider: "claude", Model: "claude-test"},
			Verifier:   config.RoleModel{Provider: "oai", Model: "gpt-test"},
			Reproducer: config.RoleModel{Provider: "claude", Model: "claude-test"},
		},
	}
	t.Setenv("TEST_ANTHROPIC_KEY", "sk-ant-xxx")
	t.Setenv("TEST_OPENAI_KEY", "sk-oai-xxx")

	var recorded []UsageEvent
	rec := RecorderFunc(func(ev UsageEvent) { recorded = append(recorded, ev) })
	opts := Options{Recorder: rec}

	finder, err := ResolveRole(context.Background(), cfg, "finder", opts)
	if err != nil {
		t.Fatalf("ResolveRole finder: %v", err)
	}
	verifier, err := ResolveRole(context.Background(), cfg, "verifier", opts)
	if err != nil {
		t.Fatalf("ResolveRole verifier: %v", err)
	}

	fResp, err := finder.Complete(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("finder.Complete: %v", err)
	}
	if fResp.Text != "finder-says-hi" {
		t.Errorf("finder text = %q", fResp.Text)
	}

	vResp, err := verifier.Complete(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("verifier.Complete: %v", err)
	}
	if vResp.Text != "verifier-says-hi" {
		t.Errorf("verifier text = %q", vResp.Text)
	}

	// Usage was recorded per role/provider/model.
	if len(recorded) != 2 {
		t.Fatalf("recorded %d events, want 2", len(recorded))
	}
	byRole := map[string]UsageEvent{}
	for _, ev := range recorded {
		byRole[ev.Role] = ev
	}
	if byRole["finder"].Provider != "claude" || byRole["finder"].Usage.InputTokens != 10 {
		t.Errorf("finder event = %+v", byRole["finder"])
	}
	if byRole["verifier"].Provider != "oai" || byRole["verifier"].Usage.OutputTokens != 8 {
		t.Errorf("verifier event = %+v", byRole["verifier"])
	}
}

func TestResolveRole_UnknownRole(t *testing.T) {
	cfg := &config.Config{}
	_, err := ResolveRole(context.Background(), cfg, "nonsense", Options{})
	if err == nil {
		t.Fatal("expected error for unknown role")
	}
}

func TestResolveRole_MissingAPIKey(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"p": {Type: config.ProviderAnthropic, APIKeyEnv: "DEFINITELY_UNSET_KEY_12345"},
		},
		Roles: config.Roles{
			Finder: config.RoleModel{Provider: "p", Model: "m"},
		},
	}
	_, err := ResolveRole(context.Background(), cfg, "finder", Options{})
	if err == nil {
		t.Fatal("expected error when API key env var is unset")
	}
}

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
	provider := config.Provider{Type: config.ProviderOpenAICompatible, BaseURL: base, APIKeyEnv: "X"}
	client, err := NewClient(context.Background(), provider, "ollama", "llama3", "key", Options{})
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

// TestRoleModel_CartographerFallback pins the optional cartographer role: an
// unset [roles.cartographer] resolves to the finder's mapping, while an
// explicit mapping wins. This is what lets the summary pass point at a cheaper
// model without forcing every config to declare one.
func TestRoleModel_CartographerFallback(t *testing.T) {
	cfg := &config.Config{Roles: config.Roles{
		Finder: config.RoleModel{Provider: "p", Model: "finder-model"},
	}}
	rm, ok := roleModel(cfg, "cartographer")
	if !ok {
		t.Fatal("roleModel(cartographer) not ok")
	}
	if rm.Provider != "p" || rm.Model != "finder-model" {
		t.Errorf("unset cartographer = %+v, want finder fallback {p finder-model}", rm)
	}
	cfg.Roles.Cartographer = config.RoleModel{Provider: "p2", Model: "carto-model"}
	rm, ok = roleModel(cfg, "cartographer")
	if !ok || rm.Provider != "p2" || rm.Model != "carto-model" {
		t.Errorf("explicit cartographer = %+v, want {p2 carto-model}", rm)
	}
}

// TestResolveRole_LLMRequestTimeoutWiring asserts that the per-attempt
// request_timeout config field is threaded through to the constructed
// role client's RetryConfig.RequestTimeout. Anthropic reports
// ParallelToolCalls=true so WithSerializedToolCalls is a passthrough and
// the returned client is the *retryClient built inside NewClient — the
// most idiomatic layer at which to observe the wiring without adding a
// public accessor.
func TestResolveRole_LLMRequestTimeoutWiring(t *testing.T) {
	t.Setenv("TEST_LLM_TLS_KEY", "sk-test")
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockTextBody("anthropic", "ok", 1, 1)))
	})
	mk := func(rt time.Duration) *config.Config {
		return &config.Config{
			Providers: map[string]config.Provider{
				"p": {Type: config.ProviderAnthropic, BaseURL: base, APIKeyEnv: "TEST_LLM_TLS_KEY"},
			},
			Roles: config.Roles{
				Finder: config.RoleModel{Provider: "p", Model: "m"},
			},
			LLM: config.LLM{RequestTimeout: rt},
		}
	}
	// Zero / omitted: the resolved client must carry the LLM package default.
	c, err := ResolveRole(context.Background(), mk(0), "finder", Options{})
	if err != nil {
		t.Fatalf("ResolveRole(zero): %v", err)
	}
	rc, ok := c.(*retryClient)
	if !ok {
		t.Fatalf("ResolveRole returned %T, want *retryClient", c)
	}
	if rc.cfg.RequestTimeout != DefaultRequestTimeout {
		t.Errorf("zero-config RequestTimeout = %v, want default %v", rc.cfg.RequestTimeout, DefaultRequestTimeout)
	}
	// Explicit positive: the configured value must be the one the client runs with.
	want := 42 * time.Second
	c, err = ResolveRole(context.Background(), mk(want), "finder", Options{})
	if err != nil {
		t.Fatalf("ResolveRole(explicit): %v", err)
	}
	rc, ok = c.(*retryClient)
	if !ok {
		t.Fatalf("ResolveRole returned %T, want *retryClient", c)
	}
	if rc.cfg.RequestTimeout != want {
		t.Errorf("explicit RequestTimeout = %v, want %v", rc.cfg.RequestTimeout, want)
	}
	// MaxAttempts must be the package default — guards against the NewClient
	// reset path (MaxAttempts==0) silently re-deriving the cfg and dropping
	// our override.
	if rc.cfg.MaxAttempts != DefaultRetryConfig().MaxAttempts {
		t.Errorf("MaxAttempts = %d, want default %d (reset must not have fired)", rc.cfg.MaxAttempts, DefaultRetryConfig().MaxAttempts)
	}
}

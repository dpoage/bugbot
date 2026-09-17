package config

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/provider"
)

// newWireServer creates a test HTTP server and returns its base URL.
func newWireServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

// anthropicTextBody returns a minimal Anthropic /v1/messages response.
func anthropicTextBody(text string, inTok, outTok int64) string {
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
}

// openaiTextBody returns a minimal OpenAI /chat/completions response.
func openaiTextBody(text string, inTok, outTok int64) string {
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
}

// simpleWireRequest returns a minimal llmkit.Request for use in wiring tests.
func simpleWireRequest() llmkit.Request {
	return llmkit.Request{
		Messages: []llmkit.Message{
			llmkit.TextMessage(llmkit.RoleUser, "hello"),
		},
	}
}

func TestResolveRole_MixedProviders(t *testing.T) {
	// Two servers: an Anthropic-shaped one for finder, an OpenAI-shaped one for
	// verifier. This proves roles resolve across different providers from one config.
	anthropicBase := newWireServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicTextBody("finder-says-hi", 10, 4)))
	})
	openaiBase := newWireServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openaiTextBody("verifier-says-hi", 20, 8)))
	})

	cfg := &Config{
		Providers: map[string]Provider{
			"claude": {Type: ProviderAnthropic, BaseURL: anthropicBase, APIKeyEnv: "TEST_ANTHROPIC_KEY"},
			"oai":    {Type: ProviderOpenAI, BaseURL: openaiBase, APIKeyEnv: "TEST_OPENAI_KEY"},
		},
		Roles: Roles{
			Finder:     RoleModel{Provider: "claude", Model: "claude-test"},
			Verifier:   RoleModel{Provider: "oai", Model: "gpt-test"},
			Reproducer: RoleModel{Provider: "claude", Model: "claude-test"},
		},
	}
	t.Setenv("TEST_ANTHROPIC_KEY", "sk-ant-xxx")
	t.Setenv("TEST_OPENAI_KEY", "sk-oai-xxx")

	var recorded []llmkit.UsageEvent
	rec := llmkit.RecorderFunc(func(ev llmkit.UsageEvent) { recorded = append(recorded, ev) })
	opts := provider.Options{Recorder: rec}

	finder, err := ResolveRole(context.Background(), cfg, "finder", opts)
	if err != nil {
		t.Fatalf("ResolveRole finder: %v", err)
	}
	verifier, err := ResolveRole(context.Background(), cfg, "verifier", opts)
	if err != nil {
		t.Fatalf("ResolveRole verifier: %v", err)
	}

	fResp, err := finder.Complete(context.Background(), simpleWireRequest())
	if err != nil {
		t.Fatalf("finder.Complete: %v", err)
	}
	if fResp.Text != "finder-says-hi" {
		t.Errorf("finder text = %q", fResp.Text)
	}

	vResp, err := verifier.Complete(context.Background(), simpleWireRequest())
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
	byRole := map[string]llmkit.UsageEvent{}
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
	cfg := &Config{}
	_, err := ResolveRole(context.Background(), cfg, "nonsense", provider.Options{})
	if err == nil {
		t.Fatal("expected error for unknown role")
	}
}

func TestResolveRole_MissingAPIKey(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"p": {Type: ProviderAnthropic, APIKeyEnv: "DEFINITELY_UNSET_KEY_12345"},
		},
		Roles: Roles{
			Finder: RoleModel{Provider: "p", Model: "m"},
		},
	}
	_, err := ResolveRole(context.Background(), cfg, "finder", provider.Options{})
	if err == nil {
		t.Fatal("expected error when API key env var is unset")
	}
}

// TestRoleModel_CartographerFallback pins the optional cartographer role: an
// unset [roles.cartographer] resolves to the finder's mapping, while an
// explicit mapping wins.
func TestRoleModel_CartographerFallback(t *testing.T) {
	cfg := &Config{Roles: Roles{
		Finder: RoleModel{Provider: "p", Model: "finder-model"},
	}}
	rm, ok := roleModel(cfg, "cartographer")
	if !ok {
		t.Fatal("roleModel(cartographer) not ok")
	}
	if rm.Provider != "p" || rm.Model != "finder-model" {
		t.Errorf("unset cartographer = %+v, want finder fallback {p finder-model}", rm)
	}
	cfg.Roles.Cartographer = RoleModel{Provider: "p2", Model: "carto-model"}
	rm, ok = roleModel(cfg, "cartographer")
	if !ok || rm.Provider != "p2" || rm.Model != "carto-model" {
		t.Errorf("explicit cartographer = %+v, want {p2 carto-model}", rm)
	}
}

// TestRetryConfigFor asserts the per-attempt request_timeout mapping from
// config.LLM into llmkit.RetryConfig. Tests the unexported retryConfigFor helper
// directly, which is the unit that ResolveRole delegates to for retry wiring.
func TestRetryConfigFor(t *testing.T) {
	// Zero / omitted: must produce the LLM package default.
	rc := retryConfigFor(&Config{})
	if rc.RequestTimeout != llmkit.DefaultRequestTimeout {
		t.Errorf("zero config: RequestTimeout = %v, want default %v", rc.RequestTimeout, llmkit.DefaultRequestTimeout)
	}
	if rc.MaxAttempts != llmkit.DefaultRetryConfig().MaxAttempts {
		t.Errorf("zero config: MaxAttempts = %d, want default %d", rc.MaxAttempts, llmkit.DefaultRetryConfig().MaxAttempts)
	}

	// Explicit positive: the configured value must be the one returned.
	want := 42 * time.Second
	rc = retryConfigFor(&Config{LLM: LLM{RequestTimeout: want}})
	if rc.RequestTimeout != want {
		t.Errorf("explicit config: RequestTimeout = %v, want %v", rc.RequestTimeout, want)
	}
	// MaxAttempts must be the package default; only RequestTimeout is overridden.
	if rc.MaxAttempts != llmkit.DefaultRetryConfig().MaxAttempts {
		t.Errorf("explicit config: MaxAttempts = %d, want default %d (must not reset)", rc.MaxAttempts, llmkit.DefaultRetryConfig().MaxAttempts)
	}

	// Negative timeout must not be applied (treated as zero/default).
	rc = retryConfigFor(&Config{LLM: LLM{RequestTimeout: -1}})
	if rc.RequestTimeout != llmkit.DefaultRequestTimeout {
		t.Errorf("negative timeout: RequestTimeout = %v, want default %v", rc.RequestTimeout, llmkit.DefaultRequestTimeout)
	}
}

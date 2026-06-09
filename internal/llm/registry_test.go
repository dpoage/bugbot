package llm

import (
	"context"
	"net/http"
	"testing"

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
	// An openai-compatible endpoint defaults to ParallelToolCalls=false, so the
	// degraded path applies when wrapped.
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockTextBody("openai-compatible", "hi", 1, 1)))
	})
	provider := config.Provider{Type: config.ProviderOpenAICompatible, BaseURL: base, APIKeyEnv: "X"}
	client, err := NewClient(context.Background(), provider, "ollama", "llama3", "key", Options{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client.Capabilities().ParallelToolCalls {
		t.Error("openai-compatible should default to ParallelToolCalls=false")
	}
	// Wrapping with the degraded path must be a usable client.
	serial := WithSerializedToolCalls(client)
	if _, err := serial.Complete(context.Background(), simpleRequest()); err != nil {
		t.Fatalf("serialized Complete: %v", err)
	}
}

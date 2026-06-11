package llm

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/config"
)

// mockAnthropicResponse returns a minimal valid Anthropic /v1/messages response
// so the test server does not need to serve elaborate JSON.
func mockAnthropicResponse() string {
	return mockTextBody("anthropic", "ok", 5, 2)
}

// TestAnthropicAuth_APIKeyMode asserts that an api_key-mode adapter sends the
// x-api-key header and no Authorization header containing "Bearer", and does
// not send the oauth beta header value.
func TestAnthropicAuth_APIKeyMode(t *testing.T) {
	var gotXAPIKey, gotAuth, gotBeta string

	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotXAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockAnthropicResponse()))
	})

	// Build the adapter in api_key mode directly.
	adapter := newAnthropicAdapter("claude-test", anthropicOptions{
		apiKey:  "sk-ant-test-key",
		baseURL: base,
	})

	_, err := adapter.Complete(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// x-api-key must be present.
	if gotXAPIKey == "" {
		t.Error("api_key mode: x-api-key header must be set")
	}
	// Authorization header must not contain a Bearer token.
	if strings.HasPrefix(strings.ToLower(gotAuth), "bearer ") {
		t.Errorf("api_key mode: Authorization header must not be a Bearer token, got %q", gotAuth)
	}
	// oauth-2025-04-20 beta value must NOT appear in api_key mode.
	if strings.Contains(gotBeta, "oauth-2025-04-20") {
		t.Errorf("api_key mode: anthropic-beta must not contain oauth-2025-04-20, got %q", gotBeta)
	}
}

// TestAnthropicAuth_OAuthMode asserts that an oauth-token-mode adapter:
//   - Sets Authorization: Bearer <token>
//   - Includes "oauth-2025-04-20" in the anthropic-beta header
//   - Does NOT set x-api-key
func TestAnthropicAuth_OAuthMode(t *testing.T) {
	var gotXAPIKey, gotAuth, gotBeta string

	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotXAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockAnthropicResponse()))
	})

	// Build the adapter in oauth-token mode directly.
	adapter := newAnthropicAdapter("claude-test", anthropicOptions{
		authToken: "claude-oauth-bearer-token-xyz",
		baseURL:   base,
	})

	_, err := adapter.Complete(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Authorization must be "Bearer <token>".
	wantAuth := "Bearer claude-oauth-bearer-token-xyz"
	if gotAuth != wantAuth {
		t.Errorf("oauth mode: Authorization = %q, want %q", gotAuth, wantAuth)
	}
	// anthropic-beta must contain the oauth value.
	if !strings.Contains(gotBeta, "oauth-2025-04-20") {
		t.Errorf("oauth mode: anthropic-beta = %q, must contain oauth-2025-04-20", gotBeta)
	}
	// x-api-key must be absent — the API rejects requests carrying both.
	if gotXAPIKey != "" {
		t.Errorf("oauth mode: x-api-key must be absent, got %q", gotXAPIKey)
	}
}

// TestNewClient_AnthropicAPIKeyMode exercises the registry-level dispatch for
// api_key mode to confirm the correct header is sent through NewClient.
func TestNewClient_AnthropicAPIKeyMode(t *testing.T) {
	var gotXAPIKey, gotAuth string

	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotXAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockAnthropicResponse()))
	})

	provider := config.Provider{
		Type:      config.ProviderAnthropic,
		BaseURL:   base,
		APIKeyEnv: "UNUSED_IN_THIS_TEST",
	}
	client, err := NewClient(context.Background(), provider, "claude", "claude-test", "sk-ant-via-registry", Options{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if _, err := client.Complete(context.Background(), simpleRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotXAPIKey == "" {
		t.Error("api_key mode via NewClient: x-api-key must be set")
	}
	if strings.HasPrefix(strings.ToLower(gotAuth), "bearer ") {
		t.Errorf("api_key mode via NewClient: Authorization must not be a Bearer token, got %q", gotAuth)
	}
}

// TestNewClient_AnthropicOAuthMode exercises the registry-level dispatch for
// oauth-token mode to confirm the token routes to the Authorization header.
func TestNewClient_AnthropicOAuthMode(t *testing.T) {
	var gotXAPIKey, gotAuth, gotBeta string

	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotXAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockAnthropicResponse()))
	})

	provider := config.Provider{
		Type:         config.ProviderAnthropic,
		Auth:         "oauth-token",
		BaseURL:      base,
		AuthTokenEnv: "UNUSED_IN_THIS_TEST",
	}
	// The secret (bearer token) is passed as the apiKey argument; NewClient
	// routes it to authToken because provider.Auth == "oauth-token".
	client, err := NewClient(context.Background(), provider, "claude-oauth", "claude-test", "claude-oauth-registry-token", Options{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if _, err := client.Complete(context.Background(), simpleRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	wantAuth := "Bearer claude-oauth-registry-token"
	if gotAuth != wantAuth {
		t.Errorf("oauth mode via NewClient: Authorization = %q, want %q", gotAuth, wantAuth)
	}
	if !strings.Contains(gotBeta, "oauth-2025-04-20") {
		t.Errorf("oauth mode via NewClient: anthropic-beta = %q, must contain oauth-2025-04-20", gotBeta)
	}
	if gotXAPIKey != "" {
		t.Errorf("oauth mode via NewClient: x-api-key must be absent, got %q", gotXAPIKey)
	}
}

// TestAnthropicAuth_OAuthModeIgnoresHostAPIKeyEnv pins the env-poisoning guard:
// anthropic.NewClient applies env defaults BEFORE explicit options, and a host
// ANTHROPIC_API_KEY eagerly sets the X-Api-Key header. Without the adapter's
// WithHeaderDel, an oauth-mode request would carry BOTH credentials and the API
// would reject it. t.Setenv simulates the poisoned host.
func TestAnthropicAuth_OAuthModeIgnoresHostAPIKeyEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-host-env-key")

	var gotXAPIKey, gotAuth string
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotXAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockAnthropicResponse()))
	})

	adapter := newAnthropicAdapter("claude-test", anthropicOptions{
		authToken: "oauth-test-token",
		baseURL:   base,
	})
	if _, err := adapter.Complete(context.Background(), simpleRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotXAPIKey != "" {
		t.Errorf("oauth mode with host ANTHROPIC_API_KEY: x-api-key must be stripped, got %q", gotXAPIKey)
	}
	if gotAuth != "Bearer oauth-test-token" {
		t.Errorf("oauth mode: Authorization = %q, want Bearer oauth-test-token", gotAuth)
	}
}

// TestAnthropicAuth_APIKeyModeIgnoresHostAuthTokenEnv pins the symmetric guard:
// a host ANTHROPIC_AUTH_TOKEN (with no ANTHROPIC_API_KEY) makes the env
// defaults eagerly set Authorization; api_key mode must strip it so only
// x-api-key is sent.
func TestAnthropicAuth_APIKeyModeIgnoresHostAuthTokenEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "host-env-bearer")

	var gotXAPIKey, gotAuth string
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotXAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockAnthropicResponse()))
	})

	adapter := newAnthropicAdapter("claude-test", anthropicOptions{
		apiKey:  "sk-ant-config-key",
		baseURL: base,
	})
	if _, err := adapter.Complete(context.Background(), simpleRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotAuth != "" {
		t.Errorf("api_key mode with host ANTHROPIC_AUTH_TOKEN: Authorization must be stripped, got %q", gotAuth)
	}
	if gotXAPIKey != "sk-ant-config-key" {
		t.Errorf("api_key mode: x-api-key = %q, want sk-ant-config-key", gotXAPIKey)
	}
}

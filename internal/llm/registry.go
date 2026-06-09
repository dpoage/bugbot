package llm

import (
	"context"
	"fmt"
	"net/http"

	"github.com/dpoage/bugbot/internal/config"
)

// Options tunes client construction. The zero value is valid: it uses default
// retry policy, no recorder, and the default HTTP transport.
type Options struct {
	// Retry configures the shared retry wrapper. If MaxAttempts is 0,
	// DefaultRetryConfig is used.
	Retry RetryConfig
	// Recorder, if non-nil, receives a UsageEvent after each successful
	// completion.
	Recorder Recorder
	// Role tags emitted UsageEvents (set automatically by ResolveRole).
	Role string
	// HTTPClient overrides the transport used by the underlying SDKs. Primarily
	// for tests (httptest) and proxies. nil uses the SDK default.
	HTTPClient *http.Client
}

// NewClient builds a fully-wrapped Client for the given provider config and
// model. The returned client is decorated, outer-to-inner, with:
//
//	recorder -> retry -> adapter
//
// so usage is recorded only for the final successful attempt, and retries see
// the raw adapter errors. The API key is resolved from the environment via
// cfg-style resolution and handed to the SDK; it is never logged.
//
// apiKey is the resolved secret (caller obtains it via config.Config.APIKey).
// Passing it explicitly keeps this constructor free of environment lookups and
// testable without real keys.
func NewClient(ctx context.Context, provider config.Provider, providerName, model, apiKey string, opts Options) (Client, error) {
	if model == "" {
		return nil, fmt.Errorf("llm: model must not be empty for provider %q", providerName)
	}

	var adapter Client
	switch provider.Type {
	case config.ProviderAnthropic:
		adapter = newAnthropicAdapter(model, anthropicOptions{
			apiKey:     apiKey,
			baseURL:    provider.BaseURL,
			httpClient: opts.HTTPClient,
		})
	case config.ProviderOpenAI:
		adapter = newOpenAIAdapter(model, openaiOptions{
			apiKey:     apiKey,
			baseURL:    provider.BaseURL,
			httpClient: opts.HTTPClient,
			provider:   string(config.ProviderOpenAI),
			caps:       openAICapabilities(model),
		})
	case config.ProviderOpenAICompatible:
		adapter = newOpenAIAdapter(model, openaiOptions{
			apiKey:     apiKey,
			baseURL:    provider.BaseURL,
			httpClient: opts.HTTPClient,
			provider:   string(config.ProviderOpenAICompatible),
			caps:       openAICompatibleCapabilities(),
		})
	case config.ProviderGoogle:
		ga, err := newGoogleAdapter(ctx, model, googleOptions{
			apiKey:     apiKey,
			baseURL:    provider.BaseURL,
			httpClient: opts.HTTPClient,
		})
		if err != nil {
			return nil, err
		}
		adapter = ga
	default:
		return nil, fmt.Errorf("llm: unsupported provider type %q", provider.Type)
	}

	retryCfg := opts.Retry
	if retryCfg.MaxAttempts == 0 {
		retryCfg = DefaultRetryConfig()
	}

	client := WithRetry(adapter, retryCfg)
	client = WithRecorder(client, opts.Recorder, opts.Role, providerName, model)
	return client, nil
}

// ResolveRole builds a Client for a pipeline role (finder/verifier/reproducer)
// by mapping the role to its provider+model via config, resolving the provider
// API key from the environment, and constructing the adapter. Roles may map to
// different providers, so a single config can mix Anthropic finders with OpenAI
// verifiers, etc.
//
// The role name tags emitted UsageEvents for per-role spend accounting.
func ResolveRole(ctx context.Context, cfg *config.Config, role string, opts Options) (Client, error) {
	rm, ok := roleModel(cfg, role)
	if !ok {
		return nil, fmt.Errorf("llm: unknown role %q (want finder, verifier, or reproducer)", role)
	}

	provider, ok := cfg.Providers[rm.Provider]
	if !ok {
		return nil, fmt.Errorf("llm: role %q references unknown provider %q", role, rm.Provider)
	}

	apiKey, err := cfg.APIKey(rm.Provider)
	if err != nil {
		return nil, err
	}

	opts.Role = role
	return NewClient(ctx, provider, rm.Provider, rm.Model, apiKey, opts)
}

// roleModel returns the RoleModel mapping for the named role.
func roleModel(cfg *config.Config, role string) (config.RoleModel, bool) {
	switch role {
	case "finder":
		return cfg.Roles.Finder, true
	case "verifier":
		return cfg.Roles.Verifier, true
	case "reproducer":
		return cfg.Roles.Reproducer, true
	default:
		return config.RoleModel{}, false
	}
}

// openAICapabilities returns the capability profile for first-party OpenAI
// models.
func openAICapabilities(model string) Capabilities {
	return Capabilities{
		ContextWindow:     128_000,
		ParallelToolCalls: true,
		PromptCaching:     true,
		StructuredOutput:  true,
	}
}

// openAICompatibleCapabilities returns a conservative profile for arbitrary
// OpenAI-compatible endpoints (Ollama/vLLM/Groq/etc.). We can't know the
// backend's true capabilities, so we assume no parallel tool calls (the
// degraded path serializes them) and no caching. Callers can override.
func openAICompatibleCapabilities() Capabilities {
	return Capabilities{
		ContextWindow:     0, // unknown
		ParallelToolCalls: false,
		PromptCaching:     false,
		StructuredOutput:  false,
	}
}

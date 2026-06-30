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
//	serialize -> recorder -> retry -> adapter
//
// so usage is recorded only for the final successful attempt, retries see
// the raw adapter errors, and non-parallel-capable providers (e.g. arbitrary
// openai-compatible endpoints) have their multi-tool-call responses
// truncated to one before the agent loop sees them. The API key is resolved
// from the environment via cfg-style resolution and handed to the SDK; it is
// never logged.
//
// WithSerializedToolCalls is a no-op for providers whose Capabilities report
// ParallelToolCalls=true (Anthropic, Google, first-party OpenAI), so
// decorating unconditionally is safe and capability-driven.
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
		aopts := anthropicOptions{
			baseURL:    provider.BaseURL,
			httpClient: opts.HTTPClient,
		}
		// The secret parameter carries either an API key or an OAuth bearer token
		// depending on the provider's auth mode. Route it to the right field so
		// newAnthropicAdapter can select the correct authentication path.
		if provider.Auth == "oauth-token" {
			aopts.authToken = apiKey
		} else {
			aopts.apiKey = apiKey
		}
		adapter = newAnthropicAdapter(model, aopts)
	case config.ProviderOpenAI:
		caps := openAICapabilities(model)
		if provider.StructuredOutput != nil {
			caps.StructuredOutput = *provider.StructuredOutput
		}
		adapter = newOpenAIAdapter(model, openaiOptions{
			apiKey:     apiKey,
			baseURL:    provider.BaseURL,
			httpClient: opts.HTTPClient,
			provider:   string(config.ProviderOpenAI),
			caps:       caps,
		})
	case config.ProviderOpenAICompatible:
		caps := openAICompatibleCapabilities()
		// The conservative default for openai-compatible endpoints is
		// StructuredOutput=false; let the config flip it on (e.g. for a
		// MiniMax-style endpoint that supports it). The override is also
		// applied to other providers for symmetry — first-party defaults
		// are already true, so flipping them off is the only meaningful
		// change, and that flows through the same code path.
		if provider.StructuredOutput != nil {
			caps.StructuredOutput = *provider.StructuredOutput
		}
		adapter = newOpenAIAdapter(model, openaiOptions{
			apiKey:     apiKey,
			baseURL:    provider.BaseURL,
			httpClient: opts.HTTPClient,
			provider:   string(config.ProviderOpenAICompatible),
			caps:       caps,
			// Strict openai-compatible validators (MiniMax) reject object-valued
			// additionalProperties; downgrade it to a boolean on the wire. See
			// coerceBoolAdditionalProperties. First-party OpenAI keeps the subschema.
			requireBoolAdditionalProps: true,
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
	// Outermost: force at-most-one tool call per response when the backend
	// does not support parallel tool calls (see serialize.go). Capable
	// providers short-circuit inside the wrapper, so this is a free check
	// for anthropic/google/openai and the safety net for openai-compatible.
	return WithSerializedToolCalls(client), nil
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
		return nil, fmt.Errorf("llm: unknown role %q (want finder, verifier, reproducer, cartographer, or arbiter)", role)
	}

	provider, ok := cfg.Providers[rm.Provider]
	if !ok {
		return nil, fmt.Errorf("llm: role %q references unknown provider %q", role, rm.Provider)
	}

	// Credential resolves the active secret for this provider's auth mode:
	// an API key in api_key mode, or an OAuth bearer token in oauth-token mode.
	apiKey, err := cfg.Credential(rm.Provider)
	if err != nil {
		return nil, err
	}

	// Apply the per-attempt request timeout from config when set. NewClient
	// resets opts.Retry to defaults whenever MaxAttempts==0, which would
	// silently drop a RequestTimeout set alone on opts.Retry; start from
	// DefaultRetryConfig so MaxAttempts is non-zero (the reset is skipped)
	// and the user's RequestTimeout is the only field overridden.
	opts.Retry = DefaultRetryConfig()
	if cfg.LLM.RequestTimeout > 0 {
		opts.Retry.RequestTimeout = cfg.LLM.RequestTimeout
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
	case "cartographer":
		// Optional role: fall back to the finder's mapping when no
		// [roles.cartographer] block is configured, so the summary pass
		// resolves a client without forcing every config to declare one.
		if cfg.Roles.Cartographer.Provider != "" {
			return cfg.Roles.Cartographer, true
		}
		return cfg.Roles.Finder, true
	case "arbiter":
		// Optional role: fall back to the verifier's mapping when no
		// [roles.arbiter] block is configured, so the split-verdict
		// arbiter resolves a client without forcing every config to
		// declare one (preserves the pre-role single-model behavior).
		if cfg.Roles.Arbiter.Provider != "" {
			return cfg.Roles.Arbiter, true
		}
		return cfg.Roles.Verifier, true
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
// degraded path serializes them) and no caching. The adapter still parses
// usage.prompt_tokens_details.cached_tokens opportunistically when the
// endpoint reports it (e.g. MiniMax), so cache hits are ledgered even with
// PromptCaching=false. Callers can override.
func openAICompatibleCapabilities() Capabilities {
	return Capabilities{
		ContextWindow:     0, // unknown
		ParallelToolCalls: false,
		PromptCaching:     false,
		StructuredOutput:  false,
	}
}

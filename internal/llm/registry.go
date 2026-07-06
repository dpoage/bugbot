package llm

import (
	"context"
	"fmt"
	"net/http"
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

// NewClient builds a fully-wrapped Client for the given provider spec and
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
func NewClient(ctx context.Context, spec ProviderSpec, providerName, model, apiKey string, opts Options) (Client, error) {
	if model == "" {
		return nil, fmt.Errorf("llm: model must not be empty for provider %q", providerName)
	}

	var adapter Client
	switch spec.Type {
	case ProviderAnthropic:
		aopts := anthropicOptions{
			baseURL:    spec.BaseURL,
			httpClient: opts.HTTPClient,
		}
		// The secret parameter carries either an API key or an OAuth bearer token
		// depending on the provider's auth mode. Route it to the right field so
		// newAnthropicAdapter can select the correct authentication path.
		if spec.Auth == "oauth-token" {
			aopts.authToken = apiKey
		} else {
			aopts.apiKey = apiKey
		}
		adapter = newAnthropicAdapter(model, aopts)
	case ProviderOpenAI:
		caps := openAICapabilities(model)
		if spec.StructuredOutput != nil {
			caps.StructuredOutput = *spec.StructuredOutput
		}
		adapter = newOpenAIAdapter(model, openaiOptions{
			apiKey:     apiKey,
			baseURL:    spec.BaseURL,
			httpClient: opts.HTTPClient,
			provider:   string(ProviderOpenAI),
			caps:       caps,
		})
	case ProviderOpenAICompatible:
		caps := openAICompatibleCapabilities(model)
		// The conservative default for openai-compatible endpoints is
		// StructuredOutput=false; let the config flip it on (e.g. for a
		// MiniMax-style endpoint that supports it). The override is also
		// applied to other providers for symmetry — first-party defaults
		// are already true, so flipping them off is the only meaningful
		// change, and that flows through the same code path.
		if spec.StructuredOutput != nil {
			caps.StructuredOutput = *spec.StructuredOutput
		}
		adapter = newOpenAIAdapter(model, openaiOptions{
			apiKey:     apiKey,
			baseURL:    spec.BaseURL,
			httpClient: opts.HTTPClient,
			provider:   string(ProviderOpenAICompatible),
			caps:       caps,
			// Strict openai-compatible validators (MiniMax) reject object-valued
			// additionalProperties; downgrade it to a boolean on the wire. See
			// coerceBoolAdditionalProperties. First-party OpenAI keeps the subschema.
			requireBoolAdditionalProps: true,
		})
	case ProviderGoogle:
		ga, err := newGoogleAdapter(ctx, model, googleOptions{
			apiKey:     apiKey,
			baseURL:    spec.BaseURL,
			httpClient: opts.HTTPClient,
		})
		if err != nil {
			return nil, err
		}
		adapter = ga
	default:
		return nil, fmt.Errorf("llm: unsupported provider type %q", spec.Type)
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

// openAICapabilities returns the capability profile for first-party OpenAI
// models. Context window is per-model where known, with a 128k default for
// unrecognized models.
func openAICapabilities(model string) Capabilities {
	return Capabilities{
		ContextWindow:     openAIContextWindow(model),
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
//
// The model parameter is accepted for symmetry with other capability
// constructors; no per-model lookup is available for arbitrary endpoints,
// so ContextWindow is left at 0 (unknown).
func openAICompatibleCapabilities(model string) Capabilities {
	return Capabilities{
		ContextWindow:     0, // unknown for arbitrary endpoints
		ParallelToolCalls: false,
		PromptCaching:     false,
		StructuredOutput:  false,
	}
}

// openAIContextWindow returns the known context-window size for a first-party
// OpenAI model. Unrecognized models get the 128k default which covers the
// current GPT-4o family. scaleFinderForContext uses this to avoid context
// overflow on small models.
func openAIContextWindow(model string) int {
	switch model {
	case "gpt-3.5-turbo", "gpt-3.5-turbo-0125":
		return 16_385
	case "gpt-3.5-turbo-16k":
		return 16_385
	case "gpt-4", "gpt-4-0613":
		return 8_192
	case "gpt-4-32k", "gpt-4-32k-0613":
		return 32_768
	case "gpt-4-turbo", "gpt-4-turbo-2024-04-09", "gpt-4-turbo-preview":
		return 128_000
	case "gpt-4o", "gpt-4o-2024-05-13", "gpt-4o-2024-08-06", "gpt-4o-2024-11-20":
		return 128_000
	case "gpt-4o-mini", "gpt-4o-mini-2024-07-18":
		return 128_000
	case "o1", "o1-2024-12-17":
		return 200_000
	case "o1-mini", "o1-mini-2024-09-12":
		return 128_000
	case "o1-preview", "o1-preview-2024-09-12":
		return 128_000
	case "o3", "o3-2025-04-16":
		return 200_000
	case "o3-mini", "o3-mini-2025-01-31":
		return 200_000
	case "o4-mini", "o4-mini-2025-04-16":
		return 200_000
	case "gpt-4.1", "gpt-4.1-2025-04-14":
		return 1_047_576
	case "gpt-4.1-mini", "gpt-4.1-mini-2025-04-14":
		return 1_047_576
	case "gpt-4.1-nano", "gpt-4.1-nano-2025-04-14":
		return 1_047_576
	default:
		return 128_000
	}
}

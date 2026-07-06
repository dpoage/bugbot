package llm

// ProviderType enumerates the supported LLM provider backends.
// It mirrors the values in config.ProviderType but is owned by the llm package
// so llm has no dependency on internal/config.
type ProviderType string

const (
	ProviderAnthropic        ProviderType = "anthropic"
	ProviderOpenAI           ProviderType = "openai"
	ProviderGoogle           ProviderType = "google"
	ProviderOpenAICompatible ProviderType = "openai-compatible"
)

// ProviderSpec is the llm-owned description of a provider endpoint. Callers
// construct it from their own config types and pass it to NewClient. The llm
// package never reads internal/config directly.
//
// Auth selects the credential mode: empty string or "api_key" → standard API
// key (x-api-key header); "oauth-token" → OAuth bearer-token (Anthropic only).
//
// StructuredOutput, when non-nil, overrides the adapter's built-in default.
// nil = use the adapter default (true for first-party providers, false for
// openai-compatible endpoints).
type ProviderSpec struct {
	Type    ProviderType
	BaseURL string // optional; for testing or non-default endpoints
	Auth    string // "" | "api_key" | "oauth-token"

	// StructuredOutput overrides the adapter's StructuredOutput capability.
	// nil = adapter default.
	StructuredOutput *bool
}

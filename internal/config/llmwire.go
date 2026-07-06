package config

import (
	"context"
	"fmt"

	"github.com/dpoage/bugbot/internal/llm"
)

// ProviderSpec converts a config Provider into the llm-owned ProviderSpec.
// This is the only place in the codebase that maps config types to llm types,
// keeping the dependency edge one-directional (config → llm, not llm → config).
func ProviderSpec(p Provider) llm.ProviderSpec {
	return llm.ProviderSpec{
		Type:             llm.ProviderType(p.Type),
		BaseURL:          p.BaseURL,
		Auth:             p.Auth,
		StructuredOutput: p.StructuredOutput,
	}
}

// ResolveRole builds a Client for a pipeline role (finder/verifier/reproducer)
// by mapping the role to its provider+model via config, resolving the provider
// API key from the environment, and constructing the adapter. Roles may map to
// different providers, so a single config can mix Anthropic finders with OpenAI
// verifiers, etc.
//
// The role name tags emitted UsageEvents for per-role spend accounting.
func ResolveRole(ctx context.Context, cfg *Config, role string, opts llm.Options) (llm.Client, error) {
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
	opts.Retry = retryConfigFor(cfg)
	opts.Role = role
	return llm.NewClient(ctx, ProviderSpec(provider), rm.Provider, rm.Model, apiKey, opts)
}

// roleModel returns the RoleModel mapping for the named role.
func roleModel(cfg *Config, role string) (RoleModel, bool) {
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
		return RoleModel{}, false
	}
}

// retryConfigFor builds the llm.RetryConfig for a config, applying the
// per-attempt request timeout when set and using package defaults otherwise.
// Extracted so the mapping logic can be unit-tested independently of the
// full ResolveRole construction path.
func retryConfigFor(cfg *Config) llm.RetryConfig {
	rc := llm.DefaultRetryConfig()
	if cfg.LLM.RequestTimeout > 0 {
		rc.RequestTimeout = cfg.LLM.RequestTimeout
	}
	return rc
}

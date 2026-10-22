package config

import (
	"context"
	"fmt"

	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/provider"
	"github.com/dpoage/llmkit/retry"
)

// ProviderSpec converts a config Provider into the provider-owned Spec.
// This is the only place in the codebase that maps config types to provider
// types, keeping the dependency edge one-directional (config → provider, not
// provider → config). Model and Secret are per-role and set by ResolveRole.
func ProviderSpec(p Provider) provider.Spec {
	spec := provider.Spec{
		Type:    provider.Type(p.Type),
		BaseURL: p.BaseURL,
		Auth:    provider.Auth(p.Auth),
	}
	if p.Auth == "api_key" {
		spec.Auth = provider.AuthAPIKey
	}
	if p.StructuredOutput != nil {
		so := *p.StructuredOutput
		spec.Capabilities = func(c llmkit.Capabilities) llmkit.Capabilities {
			c.StructuredOutput = so
			return c
		}
	}
	return spec
}

// ResolveRole builds a Client for a pipeline role (finder/verifier/reproducer)
// by mapping the role to its provider+model via config, resolving the provider
// API key from the environment, and constructing the adapter. Roles may map to
// different providers, so a single config can mix Anthropic finders with OpenAI
// verifiers, etc.
//
// The client's llmkit.Identity carries the config provider name (the key in
// cfg.Providers) and the model, which spend-ledger observers record.
func ResolveRole(ctx context.Context, cfg *Config, role string, opts provider.Options) (llmkit.Client, error) {
	rm, ok := roleModel(cfg, role)
	if !ok {
		return nil, fmt.Errorf("llm: unknown role %q (want finder, verifier, reproducer, cartographer, or arbiter)", role)
	}

	prov, ok := cfg.Providers[rm.Provider]
	if !ok {
		return nil, fmt.Errorf("llm: role %q references unknown provider %q", role, rm.Provider)
	}

	// Credential resolves the active secret for this provider's auth mode:
	// an API key in api_key mode, or an OAuth bearer token in oauth-token mode.
	apiKey, err := cfg.Credential(rm.Provider)
	if err != nil {
		return nil, err
	}

	// Retry starts from retry.Default so the configured RequestTimeout is the
	// only field overridden.
	opts.Retry = retryConfigFor(cfg)
	opts.Provider = rm.Provider
	spec := ProviderSpec(prov)
	spec.Model = rm.Model
	spec.Secret = apiKey
	return provider.New(ctx, spec, opts)
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

// retryConfigFor builds the retry.Config for a config, applying the
// per-attempt request timeout when set and using package defaults otherwise.
func retryConfigFor(cfg *Config) retry.Config {
	rc := retry.Default()
	if cfg.LLM.RequestTimeout > 0 {
		rc.RequestTimeout = cfg.LLM.RequestTimeout
	}
	return rc
}

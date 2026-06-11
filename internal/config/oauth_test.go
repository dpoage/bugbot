package config

import (
	"strings"
	"testing"
)

// oauthValidYAML is a minimal valid config using oauth-token auth.
const oauthValidYAML = `
providers:
  claude-oauth:
    type: anthropic
    auth: oauth-token
    auth_token_env: CLAUDE_CODE_OAUTH_TOKEN
roles:
  finder:
    provider: claude-oauth
    model: claude-haiku-4-5
  verifier:
    provider: claude-oauth
    model: claude-opus-4-8
  reproducer:
    provider: claude-oauth
    model: claude-sonnet-4-5
budgets:
  per_cycle_tokens: 100000
  per_day_tokens: 1000000
`

// --- validateProviderAuth table tests ---------------------------------------

func TestValidateProviderAuth(t *testing.T) {
	tests := []struct {
		name       string
		provider   Provider
		wantErr    bool
		errContain string // substring that must appear in the error
	}{
		{
			name: "api_key mode empty auth — valid",
			provider: Provider{
				Type:      ProviderAnthropic,
				APIKeyEnv: "ANTHROPIC_API_KEY",
			},
		},
		{
			name: "api_key mode explicit auth — valid",
			provider: Provider{
				Type:      ProviderAnthropic,
				Auth:      "api_key",
				APIKeyEnv: "ANTHROPIC_API_KEY",
			},
		},
		{
			name: "oauth-token on anthropic — valid",
			provider: Provider{
				Type:         ProviderAnthropic,
				Auth:         "oauth-token",
				AuthTokenEnv: "CLAUDE_CODE_OAUTH_TOKEN",
			},
		},
		{
			name: "oauth-token on openai — rejected",
			provider: Provider{
				Type:         ProviderOpenAI,
				Auth:         "oauth-token",
				AuthTokenEnv: "SOME_TOKEN",
			},
			wantErr:    true,
			errContain: "oauth-token",
		},
		{
			name: "oauth-token missing auth_token_env — rejected",
			provider: Provider{
				Type: ProviderAnthropic,
				Auth: "oauth-token",
			},
			wantErr:    true,
			errContain: "auth_token_env",
		},
		{
			name: "oauth-token with api_key_env also set — rejected (both credentials)",
			provider: Provider{
				Type:         ProviderAnthropic,
				Auth:         "oauth-token",
				APIKeyEnv:    "ANTHROPIC_API_KEY",
				AuthTokenEnv: "CLAUDE_CODE_OAUTH_TOKEN",
			},
			wantErr:    true,
			errContain: "both",
		},
		{
			name: "api_key mode with auth_token_env also set — rejected (confusion guard)",
			provider: Provider{
				Type:         ProviderAnthropic,
				Auth:         "api_key",
				APIKeyEnv:    "ANTHROPIC_API_KEY",
				AuthTokenEnv: "CLAUDE_CODE_OAUTH_TOKEN",
			},
			wantErr:    true,
			errContain: "auth_token_env",
		},
		{
			name: "empty auth with auth_token_env set — rejected",
			provider: Provider{
				Type:         ProviderAnthropic,
				APIKeyEnv:    "ANTHROPIC_API_KEY",
				AuthTokenEnv: "CLAUDE_CODE_OAUTH_TOKEN",
			},
			wantErr:    true,
			errContain: "auth_token_env",
		},
		{
			name: "api_key mode missing api_key_env — rejected",
			provider: Provider{
				Type: ProviderAnthropic,
			},
			wantErr:    true,
			errContain: "api_key_env",
		},
		{
			name: "unknown auth value — rejected with valid values listed",
			provider: Provider{
				Type:      ProviderAnthropic,
				Auth:      "magic",
				APIKeyEnv: "ANTHROPIC_API_KEY",
			},
			wantErr:    true,
			errContain: "magic",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateProviderAuth("testprovider", tt.provider)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("validateProviderAuth() = nil error, want error")
				}
				if tt.errContain != "" && !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tt.errContain)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateProviderAuth() error = %v, want nil", err)
			}
		})
	}
}

// --- Validate() integration tests for oauth providers -----------------------

func TestValidate_OAuthProvider(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{
			name:    "oauth-token on anthropic — valid",
			yaml:    oauthValidYAML,
			wantErr: false,
		},
		{
			name: "oauth-token on openai — rejected",
			yaml: `
providers:
  oai:
    type: openai
    auth: oauth-token
    auth_token_env: SOME_TOKEN
roles:
  finder: {provider: oai, model: m}
  verifier: {provider: oai, model: m}
  reproducer: {provider: oai, model: m}
budgets: {per_cycle_tokens: 1, per_day_tokens: 2}
`,
			wantErr: true,
		},
		{
			name: "oauth-token missing auth_token_env — rejected",
			yaml: `
providers:
  c:
    type: anthropic
    auth: oauth-token
roles:
  finder: {provider: c, model: m}
  verifier: {provider: c, model: m}
  reproducer: {provider: c, model: m}
budgets: {per_cycle_tokens: 1, per_day_tokens: 2}
`,
			wantErr: true,
		},
		{
			name: "oauth-token with api_key_env — rejected (both credentials)",
			yaml: `
providers:
  c:
    type: anthropic
    auth: oauth-token
    api_key_env: ANTHROPIC_API_KEY
    auth_token_env: CLAUDE_CODE_OAUTH_TOKEN
roles:
  finder: {provider: c, model: m}
  verifier: {provider: c, model: m}
  reproducer: {provider: c, model: m}
budgets: {per_cycle_tokens: 1, per_day_tokens: 2}
`,
			wantErr: true,
		},
		{
			name: "api_key mode with auth_token_env — rejected",
			yaml: `
providers:
  c:
    type: anthropic
    api_key_env: ANTHROPIC_API_KEY
    auth_token_env: SOME_TOKEN
roles:
  finder: {provider: c, model: m}
  verifier: {provider: c, model: m}
  reproducer: {provider: c, model: m}
budgets: {per_cycle_tokens: 1, per_day_tokens: 2}
`,
			wantErr: true,
		},
		{
			name: "unknown auth value — rejected",
			yaml: `
providers:
  c:
    type: anthropic
    auth: magic
    api_key_env: ANTHROPIC_API_KEY
roles:
  finder: {provider: c, model: m}
  verifier: {provider: c, model: m}
  reproducer: {provider: c, model: m}
budgets: {per_cycle_tokens: 1, per_day_tokens: 2}
`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, tt.yaml)
			_, err := Load(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() = nil error, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

// --- Credential() tests -----------------------------------------------------

func TestCredential(t *testing.T) {
	t.Run("api_key mode reads APIKeyEnv", func(t *testing.T) {
		cfg := Default()
		cfg.Providers["p"] = Provider{
			Type:      ProviderAnthropic,
			APIKeyEnv: "TEST_CRED_API_KEY",
		}
		t.Setenv("TEST_CRED_API_KEY", "sk-ant-my-key")

		got, err := cfg.Credential("p")
		if err != nil {
			t.Fatalf("Credential() error = %v", err)
		}
		if got != "sk-ant-my-key" {
			t.Errorf("Credential() = %q, want sk-ant-my-key", got)
		}
	})

	t.Run("oauth-token mode reads AuthTokenEnv", func(t *testing.T) {
		cfg := Default()
		cfg.Providers["p"] = Provider{
			Type:         ProviderAnthropic,
			Auth:         "oauth-token",
			AuthTokenEnv: "TEST_CRED_OAUTH_TOKEN",
		}
		t.Setenv("TEST_CRED_OAUTH_TOKEN", "claude-oauth-bearer-xyz")

		got, err := cfg.Credential("p")
		if err != nil {
			t.Fatalf("Credential() error = %v", err)
		}
		if got != "claude-oauth-bearer-xyz" {
			t.Errorf("Credential() = %q, want claude-oauth-bearer-xyz", got)
		}
	})

	t.Run("api_key mode unset env — error naming env var", func(t *testing.T) {
		cfg := Default()
		cfg.Providers["p"] = Provider{
			Type:      ProviderAnthropic,
			APIKeyEnv: "DEFINITELY_UNSET_CRED_VAR_88821",
		}
		_, err := cfg.Credential("p")
		if err == nil {
			t.Fatal("Credential() = nil error, want error for unset env")
		}
		if !strings.Contains(err.Error(), "DEFINITELY_UNSET_CRED_VAR_88821") {
			t.Errorf("error must name the env var, got %q", err.Error())
		}
	})

	t.Run("oauth-token mode unset env — error naming env var and mentioning setup-token", func(t *testing.T) {
		cfg := Default()
		cfg.Providers["p"] = Provider{
			Type:         ProviderAnthropic,
			Auth:         "oauth-token",
			AuthTokenEnv: "DEFINITELY_UNSET_OAUTH_VAR_88822",
		}
		_, err := cfg.Credential("p")
		if err == nil {
			t.Fatal("Credential() = nil error, want error for unset env")
		}
		if !strings.Contains(err.Error(), "DEFINITELY_UNSET_OAUTH_VAR_88822") {
			t.Errorf("error must name the env var, got %q", err.Error())
		}
		if !strings.Contains(err.Error(), "setup-token") {
			t.Errorf("oauth error must mention setup-token, got %q", err.Error())
		}
	})

	t.Run("unknown provider — error", func(t *testing.T) {
		cfg := Default()
		_, err := cfg.Credential("nonexistent")
		if err == nil {
			t.Fatal("Credential() = nil error, want error for unknown provider")
		}
	})
}

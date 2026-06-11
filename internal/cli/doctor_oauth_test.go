package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/ingest"
)

// doctorOAuthConfig is a minimal valid config that uses oauth-token auth for
// the single provider. %CFGDIR% is substituted at test time.
const doctorOAuthConfig = `providers:
  claude-oauth:
    type: anthropic
    auth: oauth-token
    auth_token_env: CLAUDE_CODE_OAUTH_TOKEN
roles:
  finder:     {provider: claude-oauth, model: m}
  verifier:   {provider: claude-oauth, model: m}
  reproducer: {provider: claude-oauth, model: m}
budgets:
  per_cycle_tokens: 100000
  per_day_tokens:   1000000
sandbox:
  runtime: podman
  image: docker.io/library/debian:stable-slim
  cpus: 2
  memory_mb: 2048
  timeout_seconds: 60
storage:
  path: %CFGDIR%/state.db
report:
  dir: %CFGDIR%/reports
`

// writeDoctorOAuthConfig writes the oauth config to a temp dir and returns its
// path.
func writeDoctorOAuthConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	yaml := strings.ReplaceAll(doctorOAuthConfig, "%CFGDIR%", dir)
	path := filepath.Join(dir, "bugbot.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write doctor oauth config: %v", err)
	}
	return path
}

// oauthGreenEnv returns a doctorEnv for the oauth config with all checks
// returning success. The caller may override individual fields.
func oauthGreenEnv(t *testing.T, cfgPath string) doctorEnv {
	t.Helper()
	env := allGreenEnv(t, cfgPath)
	// Override lookupEnv so the oauth token env var is set and the api key env
	// is not (the oauth config has no api_key_env field).
	env.lookupEnv = func(key string) string {
		if key == "CLAUDE_CODE_OAUTH_TOKEN" {
			return "claude-test-bearer-token"
		}
		return ""
	}
	// Snapshot just returns Go so LSP checks fire without a real git repo.
	env.snapshot = func(_ context.Context) ([]ingest.Language, error) {
		return []ingest.Language{ingest.LangGo}, nil
	}
	return env
}

// TestDoctor_OAuthProvider_EnvSet verifies that when the oauth token env var
// is set, checkProviders emits a PASS result that names the env var with the
// "(oauth-token)" label. Crucially the token value must never appear in output.
func TestDoctor_OAuthProvider_EnvSet(t *testing.T) {
	cfgPath := writeDoctorOAuthConfig(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	env := oauthGreenEnv(t, cfgPath)

	results := checkProviders(env, cfg)
	if len(results) == 0 {
		t.Fatal("checkProviders returned no results")
	}

	var found bool
	for _, r := range results {
		if strings.HasPrefix(r.Name, "provider claude-oauth") {
			found = true
			if r.Status != statusPass {
				t.Errorf("provider claude-oauth: status = %s, want PASS", r.Status)
			}
			// Must name the env var.
			if !strings.Contains(r.Detail, "CLAUDE_CODE_OAUTH_TOKEN") {
				t.Errorf("detail must name env var, got %q", r.Detail)
			}
			// Must label as oauth-token.
			if !strings.Contains(r.Detail, "oauth-token") {
				t.Errorf("detail must contain oauth-token label, got %q", r.Detail)
			}
			// Must NOT print the token value.
			if strings.Contains(r.Detail, "claude-test-bearer-token") {
				t.Errorf("detail must not contain the token value, got %q", r.Detail)
			}
		}
	}
	if !found {
		t.Error("checkProviders: no result for provider claude-oauth")
	}
}

// TestDoctor_OAuthProvider_EnvUnset verifies that when the oauth token env var
// is absent, checkProviders emits a FAIL result naming the env var and the
// failure is hard.
func TestDoctor_OAuthProvider_EnvUnset(t *testing.T) {
	cfgPath := writeDoctorOAuthConfig(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	env := oauthGreenEnv(t, cfgPath)
	// Return empty to simulate unset token.
	env.lookupEnv = func(_ string) string { return "" }

	results := checkProviders(env, cfg)

	var found bool
	for _, r := range results {
		if strings.HasPrefix(r.Name, "provider claude-oauth") {
			found = true
			if r.Status != statusFail {
				t.Errorf("provider claude-oauth: status = %s, want FAIL", r.Status)
			}
			if !r.hard {
				t.Error("unset oauth token must be a hard failure")
			}
			// Must name the env var.
			if !strings.Contains(r.Detail, "CLAUDE_CODE_OAUTH_TOKEN") {
				t.Errorf("detail must name env var, got %q", r.Detail)
			}
		}
	}
	if !found {
		t.Error("checkProviders: no result for provider claude-oauth")
	}
}

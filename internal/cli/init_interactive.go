package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/sandbox"
	"io"
	"os"
	"strings"
)

// wizardConfig holds every answer gathered during the interactive flow.
type wizardConfig struct {
	// Provider answers
	ProviderName  string // "anthropic" | "openai"
	APIKeyEnv     string // env var name for the provider key
	FinderModel   string
	VerifierModel string
	ReproModel    string

	// Sandbox
	SandboxRuntime string // "podman" | "docker"

	// Scan excludes (newline-joined, each indented for YAML list)
	ExcludeLines string // ready-to-embed YAML list items

	// Feature toggles
	EnableRepro       bool
	EnablePatchProver bool
	EnablePublish     bool
}

// runInteractive drives the interactive wizard. It reads from `in` and writes
// to `out`. The repoDir is used for build-system and directory detection.
// lookupEnv and lookPath are injectable for tests.
//
// It does NOT check isatty — that check lives in the RunE wrapper so this
// function can be unit-tested with a scripted io.Reader.
func runInteractive(
	in io.Reader,
	out io.Writer,
	repoDir string,
	lookupEnv func(string) string,
	lookPath func(string) (string, error),
) (wizardConfig, error) {
	sc := bufio.NewScanner(in)

	prompt := func(question, defaultVal string) (string, error) {
		if defaultVal != "" {
			fmt.Fprintf(out, "%s [%s]: ", question, defaultVal)
		} else {
			fmt.Fprintf(out, "%s: ", question)
		}
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return "", err
			}
			// EOF with no input: use default.
			fmt.Fprintln(out)
			return defaultVal, nil
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			return defaultVal, nil
		}
		return line, nil
	}

	promptBool := func(question string, defaultVal bool) (bool, error) {
		defStr := "y"
		if !defaultVal {
			defStr = "n"
		}
		ans, err := prompt(question+" (y/n)", defStr)
		if err != nil {
			return false, err
		}
		ans = strings.ToLower(strings.TrimSpace(ans))
		return ans == "y" || ans == "yes", nil
	}

	var cfg wizardConfig

	// -----------------------------------------------------------------------
	// Step 1: Provider
	// -----------------------------------------------------------------------
	fmt.Fprintln(out, "\n=== Step 1: LLM Provider ===")
	fmt.Fprintln(out, "Bugbot supports anthropic (Claude) and openai (GPT) providers.")

	defaultProvider := "anthropic"
	defaultKeyEnv := "ANTHROPIC_API_KEY"
	if lookupEnv("ANTHROPIC_API_KEY") == "" && lookupEnv("OPENAI_API_KEY") != "" {
		defaultProvider = "openai"
		defaultKeyEnv = "OPENAI_API_KEY"
	}

	provider, err := prompt("Provider (anthropic|openai)", defaultProvider)
	if err != nil {
		return cfg, err
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != "anthropic" && provider != "openai" {
		provider = defaultProvider
	}
	cfg.ProviderName = provider

	if provider == "openai" {
		defaultKeyEnv = "OPENAI_API_KEY"
	} else {
		defaultKeyEnv = "ANTHROPIC_API_KEY"
	}
	keyEnv, err := prompt("API key env var", defaultKeyEnv)
	if err != nil {
		return cfg, err
	}
	cfg.APIKeyEnv = keyEnv

	// -----------------------------------------------------------------------
	// Step 2: Role models
	// -----------------------------------------------------------------------
	fmt.Fprintln(out, "\n=== Step 2: Role Models ===")
	fmt.Fprintln(out, "Each pipeline role uses a model. Press Enter to accept the defaults.")

	var defaultFinder, defaultVerifier, defaultRepro string
	if provider == "openai" {
		defaultFinder = "gpt-4o-mini"
		defaultVerifier = "gpt-4o"
		defaultRepro = "gpt-4o"
	} else {
		defaultFinder = "claude-haiku-4-5"
		defaultVerifier = "claude-opus-4-8"
		defaultRepro = "claude-sonnet-4-5"
	}

	cfg.FinderModel, err = prompt("Finder model (cheap, broad scan)", defaultFinder)
	if err != nil {
		return cfg, err
	}
	cfg.VerifierModel, err = prompt("Verifier model (strong, confirms findings)", defaultVerifier)
	if err != nil {
		return cfg, err
	}
	cfg.ReproModel, err = prompt("Reproducer model (writes repro tests)", defaultRepro)
	if err != nil {
		return cfg, err
	}

	// -----------------------------------------------------------------------
	// Step 3: Sandbox runtime
	// -----------------------------------------------------------------------
	fmt.Fprintln(out, "\n=== Step 3: Sandbox Runtime ===")

	detectedRuntime, ok := sandbox.Detect()
	if !ok {
		detectedRuntime = "podman"
	}
	cfg.SandboxRuntime, err = prompt("Sandbox runtime (podman|docker)", detectedRuntime)
	if err != nil {
		return cfg, err
	}

	// -----------------------------------------------------------------------
	// Step 4: Scan excludes
	// -----------------------------------------------------------------------
	fmt.Fprintln(out, "\n=== Step 4: Scan Excludes ===")
	fmt.Fprintln(out, "Detecting repo-specific directories to exclude...")

	// Always-exclude globs.
	excludes := []string{`".git/**"`, `"**/*_test.go"`}

	// Probe known vendor/dep dirs.
	vendorDirs := []struct {
		dir  string
		glob string
	}{
		{"vendor", `"vendor/**"`},
		{"node_modules", `"node_modules/**"`},
		{".venv", `".venv/**"`},
		{"target", `"target/**"`},
		{"dist", `"dist/**"`},
		{"build", `"build/**"`},
		{"__pycache__", `"__pycache__/**"`},
	}
	for _, vd := range vendorDirs {
		if _, statErr := os.Stat(repoDir + "/" + vd.dir); statErr == nil {
			excludes = append(excludes, vd.glob)
		}
	}

	// Append build-system-specific excludes.
	buildSystems := ingest.DetectBuildSystems(repoDir)
	for _, bs := range buildSystems {
		switch bs {
		case ingest.BuildSystemBazel:
			excludes = append(excludes, `"bazel-*/**"`)
		case ingest.BuildSystemCargo:
			// cargo puts target/ but we already cover it above
		}
	}

	// Deduplicate.
	seen := make(map[string]bool)
	var deduped []string
	for _, e := range excludes {
		if !seen[e] {
			seen[e] = true
			deduped = append(deduped, e)
		}
	}
	excludes = deduped

	var buf bytes.Buffer
	for _, e := range excludes {
		fmt.Fprintf(&buf, "    - %s\n", e)
	}
	proposedExcludes := strings.TrimRight(buf.String(), "\n")

	fmt.Fprintf(out, "Proposed excludes:\n%s\n", proposedExcludes)
	accept, err := promptBool("Accept proposed excludes?", true)
	if err != nil {
		return cfg, err
	}
	if !accept {
		fmt.Fprintln(out, "Enter excludes one per line (empty line to finish, use glob patterns):")
		var custom []string
		for {
			line, scanErr := prompt("  exclude", "")
			if scanErr != nil {
				return cfg, scanErr
			}
			if line == "" {
				break
			}
			custom = append(custom, fmt.Sprintf("    - %q", line))
		}
		if len(custom) > 0 {
			proposedExcludes = strings.Join(custom, "\n")
		}
	}
	cfg.ExcludeLines = proposedExcludes

	// -----------------------------------------------------------------------
	// Step 5: Feature toggles
	// -----------------------------------------------------------------------
	fmt.Fprintln(out, "\n=== Step 5: Feature Toggles ===")

	cfg.EnableRepro, err = promptBool(
		"Enable repro (generate self-contained reproduction tests — uses sandbox tokens)",
		false,
	)
	if err != nil {
		return cfg, err
	}

	cfg.EnablePatchProver, err = promptBool(
		"Enable patch_prover (verify AI patches pass repro test before reporting — uses sandbox tokens)",
		false,
	)
	if err != nil {
		return cfg, err
	}

	cfg.EnablePublish, err = promptBool(
		"Enable publish (open GitHub issues/PRs for confirmed findings — requires gh CLI)",
		false,
	)
	if err != nil {
		return cfg, err
	}

	return cfg, nil
}

// renderConfig turns a wizardConfig into a YAML string derived from the
// commented StarterYAML template so every comment is preserved.
func renderConfig(cfg wizardConfig) (string, error) {
	// We render by direct string building rather than text/template on
	// StarterYAML because the template contains Go template syntax characters
	// such as `{` that would need escaping. Instead we construct the YAML
	// substituting the detected/answered values at the well-known positions.

	var sb strings.Builder

	// Header and secrets notice.
	sb.WriteString("# Bugbot configuration.\n")
	sb.WriteString("#\n")
	sb.WriteString("# Secrets are NEVER stored in this file. Each provider names an environment\n")
	sb.WriteString("# variable (api_key_env) that holds its API key; Bugbot reads the value from the\n")
	sb.WriteString("# process environment at run time.\n\n")

	// Provider section.
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("# providers: named LLM backends.\n")
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("providers:\n")
	if cfg.ProviderName == "openai" {
		sb.WriteString("  openai:\n")
		sb.WriteString("    type: openai\n")
		fmt.Fprintf(&sb, "    api_key_env: %s\n", cfg.APIKeyEnv)
	} else {
		sb.WriteString("  anthropic:\n")
		sb.WriteString("    type: anthropic\n")
		fmt.Fprintf(&sb, "    api_key_env: %s\n", cfg.APIKeyEnv)
	}
	sb.WriteString("  # Example self-hosted / gateway endpoint:\n")
	sb.WriteString("  # local:\n")
	sb.WriteString("  #   type: openai-compatible\n")
	sb.WriteString("  #   base_url: http://localhost:11434/v1\n")
	sb.WriteString("  #   api_key_env: LOCAL_API_KEY\n\n")

	// Roles section.
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("# roles: map each pipeline role to a provider+model.\n")
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("roles:\n")
	sb.WriteString("  finder:\n")
	fmt.Fprintf(&sb, "    provider: %s\n", cfg.ProviderName)
	fmt.Fprintf(&sb, "    model: %s\n", cfg.FinderModel)
	sb.WriteString("  verifier:\n")
	fmt.Fprintf(&sb, "    provider: %s\n", cfg.ProviderName)
	fmt.Fprintf(&sb, "    model: %s\n", cfg.VerifierModel)
	sb.WriteString("  reproducer:\n")
	fmt.Fprintf(&sb, "    provider: %s\n", cfg.ProviderName)
	fmt.Fprintf(&sb, "    model: %s\n\n", cfg.ReproModel)

	// Budgets section — verbatim from starter.
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("# budgets: token spend caps.\n")
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("budgets:\n")
	sb.WriteString("  per_cycle_tokens: 200000\n")
	sb.WriteString("  per_day_tokens: 5000000\n")
	sb.WriteString("  cache_read_weight: 0.1\n")
	sb.WriteString("  finder_budget_share: 0.7\n")
	sb.WriteString("  finder_token_claim: 1000000\n")
	sb.WriteString("  verifier_token_claim: 1000000\n\n")

	// Scan section with dynamic excludes.
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("# scan: path globs selecting files to consider.\n")
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("scan:\n")
	sb.WriteString("  include:\n")
	sb.WriteString("    - \"**/*\"\n")
	sb.WriteString("  exclude:\n")
	sb.WriteString(cfg.ExcludeLines)
	sb.WriteString("\n")
	sb.WriteString("  cartographer: true\n\n")

	// Sandbox section.
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("# sandbox: isolated execution environment.\n")
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("sandbox:\n")
	sb.WriteString("  backend: cli\n")
	fmt.Fprintf(&sb, "  runtime: %s\n", cfg.SandboxRuntime)
	sb.WriteString("  image: docker.io/library/debian:stable-slim\n")
	sb.WriteString("  cpus: 2\n")
	sb.WriteString("  memory_mb: 2048\n")
	sb.WriteString("  timeout_seconds: 600\n")
	sb.WriteString("  idle_timeout_seconds: 120\n")
	sb.WriteString("  network: none\n")
	sb.WriteString("  dep_strategy: off\n\n")

	// Repro section.
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("# repro: reproduction-test generation stage.\n")
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	if cfg.EnableRepro {
		sb.WriteString("repro:\n")
		sb.WriteString("  enabled: true\n")
		sb.WriteString("  max_attempts: 3\n")
		sb.WriteString("  timeout_seconds: 600\n\n")
	} else {
		sb.WriteString("# repro:\n")
		sb.WriteString("#   enabled: true\n")
		sb.WriteString("#   max_attempts: 3\n")
		sb.WriteString("#   timeout_seconds: 600\n\n")
	}

	// verify section.
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("# verify: LLM-assisted patch-verification stage.\n")
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	if cfg.EnablePatchProver {
		sb.WriteString("verify:\n")
		sb.WriteString("  enabled: true\n")
		sb.WriteString("  timeout_seconds: 300\n\n")
	} else {
		sb.WriteString("# verify:\n")
		sb.WriteString("#   enabled: true\n")
		sb.WriteString("#   timeout_seconds: 300\n\n")
	}

	// Report section.
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("# report: where findings are emitted.\n")
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("report:\n")
	sb.WriteString("  dir: .bugbot/reports\n")
	sb.WriteString("  sinks:\n")
	if cfg.EnablePublish {
		sb.WriteString("    - fs\n")
		sb.WriteString("    - github\n\n")
	} else {
		sb.WriteString("    - fs\n\n")
	}

	// LLM section.
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("# llm: shared LLM client wrapper settings.\n")
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("llm:\n")
	sb.WriteString("  request_timeout: 5m\n\n")

	// Daemon section.
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("# daemon: continuous-run scheduler timing.\n")
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("daemon:\n")
	sb.WriteString("  poll_interval: 60s\n")
	sb.WriteString("  sweep_interval: 6h\n")
	sb.WriteString("  idle_backoff: 5m\n\n")

	// Storage section.
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("# storage: embedded SQLite state.\n")
	sb.WriteString("# ---------------------------------------------------------------------------\n")
	sb.WriteString("storage:\n")
	sb.WriteString("  path: .bugbot/state.db\n")

	return sb.String(), nil
}

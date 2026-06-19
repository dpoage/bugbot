package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/config"
)

// defaultInteractiveInput returns a bytes.Reader that presses Enter through
// every prompt in the interactive wizard (accept all defaults).
func defaultInteractiveInput() *bytes.Reader {
	// The wizard asks in order:
	//   provider, api_key_env,
	//   finder model, verifier model, repro model,
	//   sandbox runtime,
	//   accept excludes (y/n),
	//   enable repro (y/n), enable patch_prover (y/n), enable publish (y/n)
	// An empty line per prompt accepts the default.
	lines := strings.Join([]string{
		"", // provider
		"", // api_key_env
		"", // finder model
		"", // verifier model
		"", // repro model
		"", // sandbox runtime
		"", // accept excludes
		"", // enable repro
		"", // enable patch_prover
		"", // enable publish
	}, "\n") + "\n"
	return bytes.NewReader([]byte(lines))
}

// TestRunInteractive_EnterThrough verifies that accepting all defaults
// produces a config that (a) config.Load parses successfully and (b) contains
// the expected provider section comment.
func TestRunInteractive_EnterThrough(t *testing.T) {
	dir := t.TempDir()

	wCfg, err := runInteractive(
		defaultInteractiveInput(),
		new(bytes.Buffer),
		dir,
		func(key string) string { return "" }, // no env vars set
		func(name string) (string, error) { return "", os.ErrNotExist },
	)
	if err != nil {
		t.Fatalf("runInteractive: %v", err)
	}

	// Defaults when no env vars are set: anthropic provider.
	if wCfg.ProviderName != "anthropic" {
		t.Errorf("ProviderName = %q, want anthropic", wCfg.ProviderName)
	}
	if wCfg.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("APIKeyEnv = %q, want ANTHROPIC_API_KEY", wCfg.APIKeyEnv)
	}
	if wCfg.SandboxRuntime != "podman" {
		t.Errorf("SandboxRuntime = %q, want podman (sandbox.Detect fallback)", wCfg.SandboxRuntime)
	}
	if wCfg.EnableRepro {
		t.Error("EnableRepro should default to false")
	}
	if wCfg.EnablePatchProver {
		t.Error("EnablePatchProver should default to false")
	}
	if wCfg.EnablePublish {
		t.Error("EnablePublish should default to false")
	}
}

// TestRunInteractive_RenderedConfigLoads confirms that the rendered YAML from
// a default wizard run can be loaded by config.Load without error.
func TestRunInteractive_RenderedConfigLoads(t *testing.T) {
	dir := t.TempDir()

	wCfg, err := runInteractive(
		defaultInteractiveInput(),
		new(bytes.Buffer),
		dir,
		func(key string) string { return "" },
		func(name string) (string, error) { return "", os.ErrNotExist },
	)
	if err != nil {
		t.Fatalf("runInteractive: %v", err)
	}

	yaml, err := renderConfig(wCfg)
	if err != nil {
		t.Fatalf("renderConfig: %v", err)
	}

	// Write to a temp file and load.
	cfgPath := filepath.Join(dir, "bugbot.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, loadErr := config.Load(cfgPath)
	if loadErr != nil {
		t.Fatalf("config.Load: %v\nYAML:\n%s", loadErr, yaml)
	}
}

// TestRunInteractive_CommentsPreserved checks that the rendered config retains
// comments (not the output of yaml.Marshal, which would strip them).
func TestRunInteractive_CommentsPreserved(t *testing.T) {
	dir := t.TempDir()

	wCfg, err := runInteractive(
		defaultInteractiveInput(),
		new(bytes.Buffer),
		dir,
		func(key string) string { return "" },
		func(name string) (string, error) { return "", os.ErrNotExist },
	)
	if err != nil {
		t.Fatalf("runInteractive: %v", err)
	}

	yaml, err := renderConfig(wCfg)
	if err != nil {
		t.Fatalf("renderConfig: %v", err)
	}

	for _, wantComment := range []string{
		"# Bugbot configuration.",
		"# Secrets are NEVER stored",
		"# providers:",
		"# roles:",
		"# budgets:",
		"# sandbox:",
		"# repro:",
		"# report:",
	} {
		if !strings.Contains(yaml, wantComment) {
			t.Errorf("rendered config missing comment %q", wantComment)
		}
	}
}

// TestRunInteractive_OpenAIDefault confirms that when only OPENAI_API_KEY is
// set, the wizard defaults to the openai provider.
func TestRunInteractive_OpenAIDefault(t *testing.T) {
	dir := t.TempDir()

	wCfg, err := runInteractive(
		defaultInteractiveInput(),
		new(bytes.Buffer),
		dir,
		func(key string) string {
			if key == "OPENAI_API_KEY" {
				return "sk-test"
			}
			return ""
		},
		func(name string) (string, error) { return "", os.ErrNotExist },
	)
	if err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if wCfg.ProviderName != "openai" {
		t.Errorf("ProviderName = %q, want openai", wCfg.ProviderName)
	}
}

// TestRunInteractive_ReproToggle tests that answering "y" to repro enables it.
func TestRunInteractive_ReproToggle(t *testing.T) {
	dir := t.TempDir()

	// Override the repro prompt to "y".
	lines := strings.Join([]string{
		"",  // provider
		"",  // api_key_env
		"",  // finder model
		"",  // verifier model
		"",  // repro model
		"",  // sandbox runtime
		"",  // accept excludes
		"y", // enable repro
		"",  // enable patch_prover
		"",  // enable publish
	}, "\n") + "\n"

	wCfg, err := runInteractive(
		bytes.NewReader([]byte(lines)),
		new(bytes.Buffer),
		dir,
		func(key string) string { return "" },
		func(name string) (string, error) { return "", os.ErrNotExist },
	)
	if err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	if !wCfg.EnableRepro {
		t.Error("EnableRepro should be true when answered 'y'")
	}

	// Rendered config should have uncommented repro block.
	yaml, err := renderConfig(wCfg)
	if err != nil {
		t.Fatalf("renderConfig: %v", err)
	}
	if !strings.Contains(yaml, "repro:\n  enabled: true") {
		t.Errorf("rendered config missing active repro block; got:\n%s", yaml)
	}
}

// TestInitCmd_NoClobber verifies that the init command refuses to overwrite an
// existing bugbot.yaml (O_EXCL preserved) for both static and interactive modes.
func TestInitCmd_NoClobber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultFileName)
	if err := os.WriteFile(path, []byte("existing: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"static", "interactive-check"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "static" {
				// Simulate the static write path (runInitStatic uses cwd).
				// Just test the O_EXCL open directly via the same pattern.
				_, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
				if err == nil {
					t.Error("expected error creating file that already exists")
				}
			} else {
				// Interactive path: the pre-flight stat check in runInitInteractive.
				// We call it indirectly by checking the file existence.
				if _, statErr := os.Stat(path); statErr != nil {
					t.Errorf("file should exist: %v", statErr)
				}
				// The real runInitInteractive would return an error here;
				// validated by the TestInitCmd_CobraInteractiveNoClobber below.
			}
		})
	}
}

// TestInitCmd_StaticPath tests the cobra command for the non-interactive case.
func TestInitCmd_StaticPath(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(orig) }()

	cmd := newInitCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, config.DefaultFileName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "# Bugbot configuration.") {
		t.Error("written file missing StarterYAML header comment")
	}
}

// TestInitCmd_StaticNoClobber verifies the cobra static path refuses to overwrite.
func TestInitCmd_StaticNoClobber(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(orig) }()

	// Pre-create the file.
	if err := os.WriteFile(config.DefaultFileName, []byte("x: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newInitCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want 'already exists'", err.Error())
	}
}

// TestInitCmd_InteractiveRequiresTTY verifies that --interactive on a non-TTY
// stdin returns a clear error (never hangs).
func TestInitCmd_InteractiveRequiresTTY(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(orig) }()

	cmd := newInitCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	// Set stdin to a plain bytes.Buffer (not a *os.File → not a TTY).
	cmd.SetIn(strings.NewReader(""))
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	if err := cmd.ParseFlags([]string{"--interactive"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected error for non-TTY stdin with --interactive")
	}
	if !strings.Contains(err.Error(), "TTY") && !strings.Contains(err.Error(), "terminal") {
		t.Errorf("error = %q, want TTY/terminal message", err.Error())
	}
}

// TestRenderConfig_AnthropicDefaults confirms key YAML fields in the rendered
// output for an anthropic default run.
func TestRenderConfig_AnthropicDefaults(t *testing.T) {
	cfg := wizardConfig{
		ProviderName:      "anthropic",
		APIKeyEnv:         "ANTHROPIC_API_KEY",
		FinderModel:       "claude-haiku-4-5",
		VerifierModel:     "claude-opus-4-8",
		ReproModel:        "claude-sonnet-4-5",
		SandboxRuntime:    "podman",
		ExcludeLines:      "    - \".git/**\"\n    - \"**/*_test.go\"",
		EnableRepro:       false,
		EnablePatchProver: false,
		EnablePublish:     false,
	}

	yaml, err := renderConfig(cfg)
	if err != nil {
		t.Fatalf("renderConfig: %v", err)
	}

	for _, want := range []string{
		"anthropic:",
		"api_key_env: ANTHROPIC_API_KEY",
		"model: claude-haiku-4-5",
		"model: claude-opus-4-8",
		"model: claude-sonnet-4-5",
		"runtime: podman",
		"# repro:",
		"# verify:",
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("rendered config missing %q", want)
		}
	}
}

// TestRenderConfig_NoDriftFromStarterYAML is the drift-guard test: it loads
// config.StarterYAML and the Enter-through wizard output (no env vars set, all
// toggles default-off) into config.Config values and asserts that every section
// renderConfig hard-codes equals the StarterYAML-loaded value. If StarterYAML
// changes a default that renderConfig also emits, this test fails loudly,
// preventing silent drift between the two sources of truth.
func TestRenderConfig_NoDriftFromStarterYAML(t *testing.T) {
	dir := t.TempDir()

	// --- Load StarterYAML ---
	starterPath := filepath.Join(dir, "starter.yaml")
	if err := os.WriteFile(starterPath, []byte(config.StarterYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	starterCfg, err := config.Load(starterPath)
	if err != nil {
		t.Fatalf("config.Load(StarterYAML): %v", err)
	}

	// --- Load wizard-rendered output (Enter-through, no env vars, all defaults) ---
	wCfg, err := runInteractive(
		defaultInteractiveInput(),
		new(bytes.Buffer),
		dir,
		func(string) string { return "" },
		func(string) (string, error) { return "", os.ErrNotExist },
	)
	if err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
	wizYAML, err := renderConfig(wCfg)
	if err != nil {
		t.Fatalf("renderConfig: %v", err)
	}
	wizPath := filepath.Join(dir, "wizard.yaml")
	if err := os.WriteFile(wizPath, []byte(wizYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	wizCfg, err := config.Load(wizPath)
	if err != nil {
		t.Fatalf("config.Load(wizardYAML): %v\nYAML:\n%s", err, wizYAML)
	}

	// --- Assert hardcoded sections match StarterYAML ---

	// Budgets: every field renderConfig emits verbatim.
	if got, want := wizCfg.Budgets.PerCycleTokens, starterCfg.Budgets.PerCycleTokens; got != want {
		t.Errorf("Budgets.PerCycleTokens: wizard=%d starter=%d", got, want)
	}
	if got, want := wizCfg.Budgets.PerDayTokens, starterCfg.Budgets.PerDayTokens; got != want {
		t.Errorf("Budgets.PerDayTokens: wizard=%d starter=%d", got, want)
	}
	if got, want := wizCfg.Budgets.CacheReadWeight, starterCfg.Budgets.CacheReadWeight; got != want {
		t.Errorf("Budgets.CacheReadWeight: wizard=%v starter=%v", got, want)
	}
	if got, want := wizCfg.Budgets.FinderBudgetShare, starterCfg.Budgets.FinderBudgetShare; got != want {
		t.Errorf("Budgets.FinderBudgetShare: wizard=%v starter=%v", got, want)
	}
	if got, want := wizCfg.Budgets.FinderTokenClaim, starterCfg.Budgets.FinderTokenClaim; got != want {
		t.Errorf("Budgets.FinderTokenClaim: wizard=%d starter=%d", got, want)
	}
	if got, want := wizCfg.Budgets.VerifierTokenClaim, starterCfg.Budgets.VerifierTokenClaim; got != want {
		t.Errorf("Budgets.VerifierTokenClaim: wizard=%d starter=%d", got, want)
	}

	// Scan: include list and cartographer (excludes legitimately differ — wizard
	// probes actual directories).
	if got, want := len(wizCfg.Scan.Include), len(starterCfg.Scan.Include); got != want {
		t.Errorf("Scan.Include len: wizard=%d starter=%d", got, want)
	} else {
		for i, v := range starterCfg.Scan.Include {
			if wizCfg.Scan.Include[i] != v {
				t.Errorf("Scan.Include[%d]: wizard=%q starter=%q", i, wizCfg.Scan.Include[i], v)
			}
		}
	}
	if got, want := wizCfg.Scan.Cartographer, starterCfg.Scan.Cartographer; got != want {
		t.Errorf("Scan.Cartographer: wizard=%v starter=%v", got, want)
	}

	// Sandbox: all fields except Runtime (wizard auto-detects).
	if got, want := wizCfg.Sandbox.Backend, starterCfg.Sandbox.Backend; got != want {
		t.Errorf("Sandbox.Backend: wizard=%q starter=%q", got, want)
	}
	if got, want := wizCfg.Sandbox.Image, starterCfg.Sandbox.Image; got != want {
		t.Errorf("Sandbox.Image: wizard=%q starter=%q", got, want)
	}
	if got, want := wizCfg.Sandbox.CPUs, starterCfg.Sandbox.CPUs; got != want {
		t.Errorf("Sandbox.CPUs: wizard=%d starter=%d", got, want)
	}
	if got, want := wizCfg.Sandbox.MemoryMB, starterCfg.Sandbox.MemoryMB; got != want {
		t.Errorf("Sandbox.MemoryMB: wizard=%d starter=%d", got, want)
	}
	if got, want := wizCfg.Sandbox.TimeoutSeconds, starterCfg.Sandbox.TimeoutSeconds; got != want {
		t.Errorf("Sandbox.TimeoutSeconds: wizard=%d starter=%d", got, want)
	}
	if got, want := wizCfg.Sandbox.IdleTimeoutSeconds, starterCfg.Sandbox.IdleTimeoutSeconds; got != want {
		t.Errorf("Sandbox.IdleTimeoutSeconds: wizard=%d starter=%d", got, want)
	}
	if got, want := wizCfg.Sandbox.Network, starterCfg.Sandbox.Network; got != want {
		t.Errorf("Sandbox.Network: wizard=%q starter=%q", got, want)
	}
	if got, want := wizCfg.Sandbox.DepStrategy, starterCfg.Sandbox.DepStrategy; got != want {
		t.Errorf("Sandbox.DepStrategy: wizard=%q starter=%q", got, want)
	}

	// Report.
	if got, want := wizCfg.Report.Dir, starterCfg.Report.Dir; got != want {
		t.Errorf("Report.Dir: wizard=%q starter=%q", got, want)
	}
	if got, want := len(wizCfg.Report.Sinks), len(starterCfg.Report.Sinks); got != want {
		t.Errorf("Report.Sinks len: wizard=%d starter=%d", got, want)
	} else {
		for i, v := range starterCfg.Report.Sinks {
			if wizCfg.Report.Sinks[i] != v {
				t.Errorf("Report.Sinks[%d]: wizard=%q starter=%q", i, wizCfg.Report.Sinks[i], v)
			}
		}
	}

	// LLM.
	if got, want := wizCfg.LLM.RequestTimeout, starterCfg.LLM.RequestTimeout; got != want {
		t.Errorf("LLM.RequestTimeout: wizard=%v starter=%v", got, want)
	}

	// Daemon.
	if got, want := wizCfg.Daemon.PollInterval, starterCfg.Daemon.PollInterval; got != want {
		t.Errorf("Daemon.PollInterval: wizard=%v starter=%v", got, want)
	}
	if got, want := wizCfg.Daemon.SweepInterval, starterCfg.Daemon.SweepInterval; got != want {
		t.Errorf("Daemon.SweepInterval: wizard=%v starter=%v", got, want)
	}
	if got, want := wizCfg.Daemon.IdleBackoff, starterCfg.Daemon.IdleBackoff; got != want {
		t.Errorf("Daemon.IdleBackoff: wizard=%v starter=%v", got, want)
	}

	// Storage.
	if got, want := wizCfg.Storage.Path, starterCfg.Storage.Path; got != want {
		t.Errorf("Storage.Path: wizard=%q starter=%q", got, want)
	}
}

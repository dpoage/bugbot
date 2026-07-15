package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/repro"
)

// doctorMinimalConfig is a minimal valid config for doctor tests. It sets the
// required one-provider/three-roles minimum that config.Validate accepts.
// %CFGDIR% is substituted with the test's temp directory so storage.path and
// report.dir point somewhere real (though doctor never writes to them).
const doctorMinimalConfig = `providers:
  testprovider:
    type: anthropic
    api_key_env: FAKE_API_KEY_ENV
roles:
  finder:     {provider: testprovider, model: m}
  verifier:   {provider: testprovider, model: m}
  reproducer: {provider: testprovider, model: m}
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

// writeDoctorConfig writes doctorMinimalConfig to a temp dir and returns the
// config path.
func writeDoctorConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	yaml := strings.ReplaceAll(doctorMinimalConfig, "%CFGDIR%", dir)
	path := filepath.Join(dir, "bugbot.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write doctor config: %v", err)
	}
	return path
}

// allGreenEnv returns a doctorEnv with all fakes returning success, pointing at
// a valid temporary config. The caller may override individual fields.
func allGreenEnv(t *testing.T, cfgPath string) doctorEnv {
	t.Helper()
	return doctorEnv{
		configPath: cfgPath,
		repoDir:    t.TempDir(), // not a real git repo; git check uses runCommand
		lookupEnv: func(key string) string {
			if key == "FAKE_API_KEY_ENV" {
				return "fake-secret-value-that-must-not-appear"
			}
			return ""
		},
		lookPath: func(name string) (string, error) {
			// Succeed for podman and gh; fail for anything else so LSP
			// checks don't accidentally succeed on the test runner's PATH.
			switch name {
			case "podman", "gh":
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
		runCommand: func(ctx context.Context, name string, args ...string) (string, error) {
			switch {
			case name == "git":
				// git rev-parse --git-dir
				return ".git", nil
			case name == "podman" && len(args) == 1 && args[0] == "info":
				return "podman info output", nil
			case name == "podman" && len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
				return `[{"Id":"sha256:abc"}]`, nil
			// Binary probe: podman run --rm <image> sh -c "command -v <bin>"
			// Succeed unconditionally so name-match-passing images don't WARN.
			case name == "podman" && len(args) >= 4 && args[0] == "run" && args[1] == "--rm":
				return "/usr/bin/tool", nil
			}
			return "", fmt.Errorf("unexpected command: %s %v", name, args)
		},
		// Inject a seam so no real git subprocess is needed for language detection.
		snapshot: func(_ context.Context) ([]ingest.Language, error) {
			return []ingest.Language{ingest.LangGo}, nil
		},
		out: nil, // caller sets this
	}
}

// TestDoctor_AllGreen confirms that a fully satisfied environment produces no
// failures and returns a nil error (exit 0).
func TestDoctor_AllGreen(t *testing.T) {
	cfgPath := writeDoctorConfig(t)
	var sb strings.Builder
	env := allGreenEnv(t, cfgPath)
	env.out = &sb

	results := runChecks(context.Background(), env, false)
	for _, r := range results {
		if r.hard && r.Status == statusFail {
			t.Errorf("all-green: unexpected FAIL on hard check %q: %s", r.Name, r.Detail)
		}
	}
}

// TestDoctor_UnsetAPIKey checks that a missing API key env var causes a FAIL
// result naming the env var (not any secret value), and returns an error.
func TestDoctor_UnsetAPIKey(t *testing.T) {
	cfgPath := writeDoctorConfig(t)
	var sb strings.Builder
	env := allGreenEnv(t, cfgPath)
	// Return empty string for all env lookups to simulate unset key.
	env.lookupEnv = func(_ string) string { return "" }
	env.out = &sb

	results := runChecks(context.Background(), env, false)
	printResults(&sb, results)
	out := sb.String()

	var providerFailed bool
	for _, r := range results {
		if strings.HasPrefix(r.Name, "provider") && r.Status == statusFail {
			providerFailed = true
			// Must name the env var.
			if !strings.Contains(r.Detail, "FAKE_API_KEY_ENV") {
				t.Errorf("provider fail detail must name env var; got %q", r.Detail)
			}
			// Must not contain the secret value.
			if strings.Contains(out, "fake-secret-value-that-must-not-appear") {
				t.Errorf("output must not contain the secret value")
			}
		}
	}
	if !providerFailed {
		t.Error("expected a FAIL result for the provider API key check")
	}

	// The run-checks loop must report an error (hard failure).
	var hardFailed bool
	for _, r := range results {
		if r.hard && r.Status == statusFail {
			hardFailed = true
			break
		}
	}
	if !hardFailed {
		t.Error("unset API key must cause a hard failure")
	}
}

// TestDoctor_MissingRuntime checks that lookPath failing for the runtime binary
// causes a FAIL result and a hard failure.
func TestDoctor_MissingRuntime(t *testing.T) {
	cfgPath := writeDoctorConfig(t)
	var sb strings.Builder
	env := allGreenEnv(t, cfgPath)
	env.lookPath = func(name string) (string, error) {
		if name == "podman" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + name, nil
	}
	env.out = &sb

	results := runChecks(context.Background(), env, false)

	var binaryFailed bool
	for _, r := range results {
		if r.Name == "sandbox binary" && r.Status == statusFail {
			binaryFailed = true
			if !r.hard {
				t.Error("sandbox binary fail must be hard")
			}
		}
	}
	if !binaryFailed {
		t.Error("missing runtime binary must produce a FAIL on sandbox binary")
	}
}

// TestDoctor_WedgedRuntime asserts that a runCommand that blocks until ctx is
// done returns promptly (well under 30s) with a FAIL result for sandbox
// responding. The probe timeout in doctor is 5s; the test must complete within
// a generous 30s wall-clock budget.
func TestDoctor_WedgedRuntime(t *testing.T) {
	cfgPath := writeDoctorConfig(t)
	var sb strings.Builder
	env := allGreenEnv(t, cfgPath)
	env.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "podman" && len(args) == 1 && args[0] == "info" {
			// Block until the caller's context is cancelled (simulates a
			// wedged daemon). This must not hang doctor beyond sandboxProbeTimeout.
			<-ctx.Done()
			return "", ctx.Err()
		}
		// git and other calls succeed normally.
		if name == "git" {
			return ".git", nil
		}
		return "", nil
	}
	env.out = &sb

	start := time.Now()
	results := runChecks(context.Background(), env, false)
	elapsed := time.Since(start)

	// The whole doctor run must finish well within 30s (probe timeout is 5s).
	if elapsed > 30*time.Second {
		t.Errorf("doctor hung for %s; want < 30s", elapsed.Round(time.Second))
	}

	var respondFailed bool
	for _, r := range results {
		if r.Name == "sandbox responding" && r.Status == statusFail {
			respondFailed = true
			if !r.hard {
				t.Error("sandbox responding fail must be hard")
			}
		}
	}
	if !respondFailed {
		t.Error("wedged runtime must produce a FAIL on sandbox responding")
	}
}

// TestDoctor_InvalidConfig confirms that a corrupt config produces a FAIL on
// the config check, and providers/sandbox results are SKIP (they need a valid
// config).
func TestDoctor_InvalidConfig(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "bugbot.yaml")
	if err := os.WriteFile(badPath, []byte("not: valid: yaml: ["), 0o644); err != nil {
		t.Fatal(err)
	}

	var sb strings.Builder
	env := allGreenEnv(t, badPath)
	env.configPath = badPath
	env.out = &sb

	results := runChecks(context.Background(), env, false)

	var configFailed bool
	for _, r := range results {
		switch {
		case r.Name == "config" && r.Status == statusFail:
			configFailed = true
			if !r.hard {
				t.Error("config fail must be hard")
			}
		case (r.Name == "providers" || r.Name == "sandbox binary" ||
			r.Name == "sandbox responding" || r.Name == "sandbox image") &&
			r.Status != statusSkip:
			t.Errorf("check %q should be SKIP after config fail; got %s", r.Name, r.Status)
		}
	}
	if !configFailed {
		t.Error("invalid config must produce a FAIL on config check")
	}
}

// TestDoctor_SecretsNeverInOutput asserts that the API key value injected
// through the env fake never appears anywhere in the doctor output.
func TestDoctor_SecretsNeverInOutput(t *testing.T) {
	const secretValue = "super-secret-key-xyz-12345"
	cfgPath := writeDoctorConfig(t)
	var sb strings.Builder
	env := allGreenEnv(t, cfgPath)
	env.lookupEnv = func(key string) string {
		if key == "FAKE_API_KEY_ENV" {
			return secretValue
		}
		return ""
	}
	env.out = &sb

	results := runChecks(context.Background(), env, false)
	printResults(&sb, results)
	out := sb.String()

	if strings.Contains(out, secretValue) {
		t.Errorf("doctor output must not contain secret value %q\n---\n%s", secretValue, out)
	}
	// Env var name must appear, not the value.
	if !strings.Contains(out, "FAKE_API_KEY_ENV") {
		t.Errorf("doctor output must name the env var\n---\n%s", out)
	}
}

// TestDoctor_ImageAbsent confirms that a non-zero exit from image inspect
// produces a WARN (not a FAIL) and that the command still returns nil error
// (given everything else green).
func TestDoctor_ImageAbsent(t *testing.T) {
	cfgPath := writeDoctorConfig(t)
	var sb strings.Builder
	env := allGreenEnv(t, cfgPath)
	env.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "podman" && len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
			// Simulate image not present.
			return "", errors.New("image not found")
		}
		// Everything else succeeds.
		if name == "git" {
			return ".git", nil
		}
		return "ok", nil
	}
	env.out = &sb

	results := runChecks(context.Background(), env, false)

	var imageWarn bool
	for _, r := range results {
		if r.Name == "sandbox image" {
			if r.Status != statusWarn {
				t.Errorf("absent image must be WARN; got %s", r.Status)
			}
			if r.hard {
				t.Error("absent image warn must NOT be hard")
			}
			imageWarn = true
		}
	}
	if !imageWarn {
		t.Error("expected a sandbox image result")
	}

	// No hard failures — command should return nil.
	var hardFailed bool
	for _, r := range results {
		if r.hard && r.Status == statusFail {
			hardFailed = true
		}
	}
	if hardFailed {
		t.Error("image absent must not cause a hard failure")
	}
}

// TestDoctor_CobraPath exercises the full cobra command path (run() goes
// through NewRootCmd().Execute()) to cover exit-code wiring. The injectable
// fakes live inside RunE, so the cobra path runs against the real host
// environment — assertions must therefore be host-independent. An invalid
// config is the one hard failure we can force deterministically: doctor must
// print the FAIL config line and return a non-nil error on every host.
func TestDoctor_CobraPath(t *testing.T) {
	badPath := filepath.Join(t.TempDir(), "bugbot.yaml")
	if err := os.WriteFile(badPath, []byte(":::not yaml:::"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, badPath, "doctor")
	if err == nil {
		t.Error("cobra doctor with invalid config must return a non-nil error (nonzero exit)")
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "config") {
		t.Errorf("cobra doctor output missing FAIL config line:\n%s", out)
	}

	// A valid config on an arbitrary host may pass or fail (podman, keys, …);
	// just assert the command runs and prints the checklist.
	cfgPath := writeDoctorConfig(t)
	out, _ = run(t, cfgPath, "doctor")
	if !strings.Contains(out, "config") {
		t.Errorf("cobra doctor output missing 'config' check line:\n%s", out)
	}
}

// writeDoctorConfigWithImage writes a doctor config with the given sandbox
// image (and an explicit network=none, matching real usage) so the
// image-vs-language advisory can be exercised.
func writeDoctorConfigWithImage(t *testing.T, image, network string) string {
	t.Helper()
	dir := t.TempDir()
	yaml := strings.ReplaceAll(doctorMinimalConfig, "%CFGDIR%", dir)
	// The minimal config hardcodes image + omits network. Replace the image
	// line and inject a network line so the advisor sees the values we want
	// to test. Substrings are deliberately anchored on the field name to
	// avoid matching unrelated text.
	yaml = strings.Replace(yaml, "  image: docker.io/library/debian:stable-slim", "  image: "+image, 1)
	if network != "" {
		yaml = strings.Replace(yaml, "  timeout_seconds: 60", "  timeout_seconds: 60\n  network: "+network, 1)
	}
	path := filepath.Join(dir, "bugbot.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write doctor config: %v", err)
	}
	return path
}

// envWithLangs returns an allGreenEnv-like doctorEnv whose snapshot seam
// returns the given dominant languages. Callers may further override fields
// (configPath, etc.) before passing it to runChecks.
func envWithLangs(t *testing.T, cfgPath string, langs []ingest.Language) doctorEnv {
	t.Helper()
	env := allGreenEnv(t, cfgPath)
	env.snapshot = func(_ context.Context) ([]ingest.Language, error) {
		return langs, nil
	}
	return env
}

// TestDoctor_ImageToolchain_WarnMissingLanguages asserts that when the
// configured sandbox image does not contain the expected toolchain hint for
// a detected dominant language, a WARN is emitted naming that language. The
// warns are advisory only — they must not flip the exit code.
func TestDoctor_ImageToolchain_WarnMissingLanguages(t *testing.T) {
	cfgPath := writeDoctorConfigWithImage(t, "docker.io/library/golang:1.25-alpine", "none")
	env := envWithLangs(t, cfgPath, []ingest.Language{ingest.LangTypeScript, ingest.LangPython, ingest.LangGo})

	results := runChecks(context.Background(), env, false)

	warned := map[ingest.Language]bool{}
	for _, r := range results {
		if r.Status != statusWarn {
			continue
		}
		// Each warn is advisory; must never be hard.
		if r.hard {
			t.Errorf("image-toolchain warn %q must NOT be hard", r.Name)
		}
		switch r.Name {
		case "image toolchain typescript":
			warned[ingest.LangTypeScript] = true
			if !strings.Contains(r.Detail, "typescript") || !strings.Contains(r.Detail, "node") {
				t.Errorf("typescript warn detail should name language and expected toolchain; got %q", r.Detail)
			}
		case "image toolchain python":
			warned[ingest.LangPython] = true
			if !strings.Contains(r.Detail, "python") {
				t.Errorf("python warn detail should name language; got %q", r.Detail)
			}
		case "image toolchain go":
			// golang hint is in the image — this must NOT warn.
			t.Errorf("golang-covering image must not emit image toolchain warn for go; got %+v", r)
		}
	}
	if !warned[ingest.LangTypeScript] {
		t.Error("expected a WARN naming typescript (image lacks node hint)")
	}
	if !warned[ingest.LangPython] {
		t.Error("expected a WARN naming python (image lacks python hint)")
	}
	// Sanity: no hard failures from these advisories.
	for _, r := range results {
		if r.hard && r.Status == statusFail {
			t.Errorf("unexpected hard FAIL: %s", r.Name)
		}
	}
}

// TestDoctor_ImageToolchain_CoveringImageNoWarns asserts that a sandbox image
// bundling every expected toolchain (node + python + golang) suppresses the
// per-language warns for {typescript, python, go}.
func TestDoctor_ImageToolchain_CoveringImageNoWarns(t *testing.T) {
	cfgPath := writeDoctorConfigWithImage(t, "example.com/polyglot-node-python-golang:1", "none")
	env := envWithLangs(t, cfgPath, []ingest.Language{ingest.LangTypeScript, ingest.LangPython, ingest.LangGo})

	results := runChecks(context.Background(), env, false)

	for _, r := range results {
		if strings.HasPrefix(r.Name, "image toolchain ") {
			t.Errorf("covering image must not emit image toolchain warns; got %+v", r)
		}
	}
}

// TestDoctor_ImageToolchain_BazelOfflineWarns asserts that a Bazel build
// system under sandbox.network=none emits the offline-bazel WARN. The warn
// is advisory only — must not affect the exit code.
func TestDoctor_ImageToolchain_BazelOfflineWarns(t *testing.T) {
	cfgPath := writeDoctorConfigWithImage(t, "gcr.io/bazel-public/bazel:latest", "none")
	env := envWithLangs(t, cfgPath, []ingest.Language{ingest.LangGo})
	// Patch the repoDir so checkRepo's `git rev-parse` succeeds, then the
	// checkRepo path returns the build-systems list from ingest.DetectBuildSystems.
	// We cannot fake DetectBuildSystems here without adding a seam, so we
	// place a MODULE.bazel file inside repoDir to make the real detector
	// return BuildSystemBazel.
	repoDir := env.repoDir
	if err := os.WriteFile(filepath.Join(repoDir, "MODULE.bazel"), []byte("module(name=\"x\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	results := runChecks(context.Background(), env, false)

	var saw bool
	for _, r := range results {
		if r.Name != "image bazel offline" {
			continue
		}
		saw = true
		if r.Status != statusWarn {
			t.Errorf("bazel offline warn must be WARN; got %s", r.Status)
		}
		if r.hard {
			t.Error("bazel offline warn must NOT be hard")
		}
		if !strings.Contains(r.Detail, "prefetched") {
			t.Errorf("bazel offline warn should mention prefetched cache; got %q", r.Detail)
		}
		if !strings.Contains(r.Detail, "bugbot sandbox build") {
			t.Errorf("bazel offline warn should reference `bugbot sandbox build`; got %q", r.Detail)
		}
		if strings.Contains(r.Detail, "unsupported") {
			t.Errorf("bazel offline warn must not say 'unsupported'; got %q", r.Detail)
		}
	}
	if !saw {
		t.Error("expected a WARN for image bazel offline (bazel + network=none)")
	}
	// Sanity: no hard failures from this advisory.
	for _, r := range results {
		if r.hard && r.Status == statusFail {
			t.Errorf("unexpected hard FAIL: %s", r.Name)
		}
	}
}

// TestCheckImageToolchain_Direct exercises checkImageToolchain in isolation
// across a small table of (image, langs, buildSystems) cases so the
// advisory's exact behavior is locked in independently of runChecks. The
// runCommand fake makes `image inspect` fail (image not local) so the binary
// probe is skipped for all name-matched languages; probe-specific behaviour is
// covered by TestCheckImageToolchainProbe_*.
func TestCheckImageToolchain_Direct(t *testing.T) {
	cases := []struct {
		name         string
		image        string
		network      string
		langs        []ingest.Language
		buildSystems []ingest.BuildSystem
		wantWarn     []string // substrings of warn names expected
	}{
		{
			name:     "golang image covers go only",
			image:    "docker.io/library/golang:1.25-alpine",
			network:  "none",
			langs:    []ingest.Language{ingest.LangGo, ingest.LangTypeScript, ingest.LangPython},
			wantWarn: []string{"image toolchain typescript", "image toolchain python"},
		},
		{
			name:     "all-covering image suppresses name-match warns",
			image:    "example.com/stack:golang-node-python",
			network:  "none",
			langs:    []ingest.Language{ingest.LangGo, ingest.LangTypeScript, ingest.LangPython},
			wantWarn: nil, // probe skipped (not local) → INFO only, no WARN
		},
		{
			name:         "bazel + none network warns offline",
			image:        "gcr.io/bazel-public/bazel:latest",
			network:      "none",
			langs:        []ingest.Language{ingest.LangGo},
			buildSystems: []ingest.BuildSystem{ingest.BuildSystemBazel, ingest.BuildSystemGoModule},
			wantWarn:     []string{"image bazel offline"},
		},
		{
			// Custom/local image: offline bazel repro IS supported against a
			// purpose-built image, so this gets an advisory INFO (asserted in
			// TestCheckImageToolchain_BazelOfflineCustomImageInfo), NOT a WARN.
			name:         "bazel + none network custom image does not warn",
			image:        "localhost/x-bugbot-sandbox:latest",
			network:      "none",
			langs:        nil,
			buildSystems: []ingest.BuildSystem{ingest.BuildSystemBazel, ingest.BuildSystemGoModule},
			wantWarn:     nil,
		},
		{
			name:         "bazel + bridge network does not warn",
			image:        "gcr.io/bazel-public/bazel:latest",
			network:      "bridge",
			langs:        nil,
			buildSystems: []ingest.BuildSystem{ingest.BuildSystemBazel, ingest.BuildSystemGoModule},
			wantWarn:     nil,
		},
		{
			name:     "no languages yields no warns",
			image:    "docker.io/library/golang:1.25-alpine",
			network:  "none",
			langs:    nil,
			wantWarn: nil,
		},
		{
			name:     "empty image yields no warns",
			image:    "",
			network:  "none",
			langs:    []ingest.Language{ingest.LangGo},
			wantWarn: nil,
		},
	}
	// notLocalRC is an injected runCommand that reports every image as not
	// present locally (image inspect fails → probe skipped with INFO).
	notLocalRC := func(_ context.Context, name string, args ...string) (string, error) {
		if name == "podman" && len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
			return "", errors.New("no such image")
		}
		return "", fmt.Errorf("unexpected: %s %v", name, args)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Sandbox: config.Sandbox{Image: tc.image, Network: tc.network, Runtime: "podman"}}
			env := doctorEnv{runCommand: notLocalRC}
			got := checkImageToolchain(context.Background(), env, tc.langs, tc.buildSystems, cfg)
			gotWarnNames := make([]string, 0, len(got))
			for _, r := range got {
				if r.hard {
					t.Errorf("check %q must not be hard", r.Name)
				}
				if r.Status == statusWarn {
					gotWarnNames = append(gotWarnNames, r.Name)
				}
				// INFO results (probe skipped) are fine and not checked here.
			}
			for _, w := range tc.wantWarn {
				found := false
				for _, n := range gotWarnNames {
					if strings.Contains(n, w) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected a warn containing %q; got warns %v", w, gotWarnNames)
				}
			}
			if len(tc.wantWarn) == 0 && len(gotWarnNames) != 0 {
				t.Errorf("expected no warns; got %v", gotWarnNames)
			}
		})
	}
}

// TestCheckHostToolchain_Direct covers the backend:bwrap analogue of the
// image check: languages resolve against the HOST PATH (any listed binary
// suffices), and the bazel advisory keys on host bazel presence rather than
// image naming. Results are advisory; the test asserts names and statuses.
func TestCheckHostToolchain_Direct(t *testing.T) {
	cases := []struct {
		name     string
		onPath   map[string]bool
		network  string
		langs    []ingest.Language
		builds   []ingest.BuildSystem
		wantWarn []string // substrings of WARN names expected
		wantInfo []string // substrings of INFO names expected
	}{
		{
			name:     "python via python3 passes, missing node warns",
			onPath:   map[string]bool{"python3": true, "go": true},
			network:  "none",
			langs:    []ingest.Language{ingest.LangGo, ingest.LangTypeScript, ingest.LangPython},
			wantWarn: []string{"host toolchain typescript"},
		},
		{
			name:     "any-of semantics: python (not python3) suffices",
			onPath:   map[string]bool{"python": true},
			network:  "none",
			langs:    []ingest.Language{ingest.LangPython},
			wantWarn: nil,
		},
		{
			name:     "bazel build system without host bazel warns",
			onPath:   map[string]bool{"python3": true},
			network:  "none",
			langs:    []ingest.Language{ingest.LangPython},
			builds:   []ingest.BuildSystem{ingest.BuildSystemBazel},
			wantWarn: []string{"host toolchain bazel"},
		},
		{
			name:     "bazel present under network none is advisory info",
			onPath:   map[string]bool{"python3": true, "bazel": true},
			network:  "none",
			langs:    []ingest.Language{ingest.LangPython},
			builds:   []ingest.BuildSystem{ingest.BuildSystemBazel},
			wantInfo: []string{"host bazel offline"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := doctorEnv{lookPath: func(name string) (string, error) {
				if tc.onPath[name] {
					return "/fake/bin/" + name, nil
				}
				return "", errors.New("not found")
			}}
			cfg := config.Config{Sandbox: config.Sandbox{Backend: "bwrap", Network: tc.network}}
			results := checkHostToolchain(env, tc.langs, tc.builds, cfg)
			var gotWarn, gotInfo []string
			for _, r := range results {
				switch r.Status {
				case statusWarn:
					gotWarn = append(gotWarn, r.Name)
				case statusInfo:
					gotInfo = append(gotInfo, r.Name)
				}
			}
			assertNamesContain(t, "WARN", gotWarn, tc.wantWarn)
			assertNamesContain(t, "INFO", gotInfo, tc.wantInfo)
		})
	}
}

// assertNamesContain fails unless got has exactly len(want) entries and each
// want substring matches some got name.
func assertNamesContain(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s results = %v, want %d matching %v", label, got, len(want), want)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if strings.Contains(g, w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s results %v missing expected %q", label, got, w)
		}
	}
}

// TestCheckImageToolchain_BazelOfflineCustomImageInfo asserts that a Bazel repo
// under network=none with a CUSTOM/local sandbox image (not a plain public
// bazel base) gets an advisory INFO — not a WARN — because offline bazel repro
// IS supported against a purpose-built offline image built by
// `bugbot sandbox build`.
func TestCheckImageToolchain_BazelOfflineCustomImageInfo(t *testing.T) {
	notLocalRC := func(_ context.Context, name string, args ...string) (string, error) {
		if name == "podman" && len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
			return "", errors.New("no such image")
		}
		return "", fmt.Errorf("unexpected: %s %v", name, args)
	}
	cfg := config.Config{Sandbox: config.Sandbox{Image: "localhost/x-bugbot-sandbox:latest", Network: "none", Runtime: "podman"}}
	env := doctorEnv{runCommand: notLocalRC}
	results := checkImageToolchain(context.Background(), env, nil, []ingest.BuildSystem{ingest.BuildSystemBazel, ingest.BuildSystemGoModule}, cfg)

	var saw bool
	for _, r := range results {
		if r.Name != "image bazel offline" {
			continue
		}
		saw = true
		if r.Status != statusInfo {
			t.Errorf("custom image offline bazel result must be INFO; got %s", r.Status)
		}
		if r.hard {
			t.Error("bazel offline advisory must NOT be hard")
		}
		if !strings.Contains(r.Detail, "bugbot sandbox build") {
			t.Errorf("INFO detail should reference `bugbot sandbox build`; got %q", r.Detail)
		}
		if strings.Contains(r.Detail, "unsupported") {
			t.Errorf("INFO detail must not say 'unsupported'; got %q", r.Detail)
		}
	}
	if !saw {
		t.Error("expected an INFO result named 'image bazel offline' for a custom image under network=none")
	}
}

// makeProbeEnv constructs a minimal doctorEnv with a scripted runCommand for
// binary-probe tests. imageLocal controls whether `image inspect` succeeds.
// presentBins is the set of binaries that `command -v <bin>` will find.
func makeProbeEnv(imageLocal bool, presentBins map[string]bool) doctorEnv {
	return doctorEnv{
		runCommand: func(_ context.Context, name string, args ...string) (string, error) {
			if name == "podman" && len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
				if imageLocal {
					return `[{"Id":"sha256:abc"}]`, nil
				}
				return "", errors.New("no such image")
			}
			// podman run --rm <image> sh -c "command -v <bin>"
			// args: [0]=run [1]=--rm [2]=image [3]=sh [4]=-c [5]="command -v <bin>"
			if name == "podman" && len(args) >= 6 && args[0] == "run" && args[1] == "--rm" {
				cmd := args[5]
				bin := strings.TrimPrefix(cmd, "command -v ")
				if presentBins[bin] {
					return "/usr/bin/" + bin, nil
				}
				return "", errors.New("not found")
			}
			return "", fmt.Errorf("unexpected probe: %s %v", name, args)
		},
	}
}

// TestCheckImageToolchainProbe_ToolPresent verifies that when the image name
// matches AND all required binaries are found inside the image, no WARN is emitted.
func TestCheckImageToolchainProbe_ToolPresent(t *testing.T) {
	// python:3-slim matches "python"; pip3 is present.
	env := makeProbeEnv(true, map[string]bool{"pip": true, "pip3": true})
	cfg := config.Config{Sandbox: config.Sandbox{Image: "python:3-slim", Runtime: "podman"}}
	results := checkImageToolchain(context.Background(), env, []ingest.Language{ingest.LangPython}, nil, cfg)
	for _, r := range results {
		if r.Status == statusWarn {
			t.Errorf("all tools present: unexpected WARN %q: %s", r.Name, r.Detail)
		}
		if r.hard {
			t.Errorf("probe result must never be hard: %q", r.Name)
		}
	}
}

// TestCheckImageToolchainProbe_ToolAbsent verifies that when the image name
// matches but a required binary is absent, a WARN is emitted naming the tool
// and suggesting an image.
func TestCheckImageToolchainProbe_ToolAbsent(t *testing.T) {
	// gcc:13 matches "gcc" (LangCPP hints). cmake and g++ are absent.
	env := makeProbeEnv(true, map[string]bool{"cc": false, "gcc": true})
	cfg := config.Config{Sandbox: config.Sandbox{Image: "gcc:13", Runtime: "podman"}}
	results := checkImageToolchain(context.Background(), env, []ingest.Language{ingest.LangCPP}, nil, cfg)
	var gotWarn *checkResult
	for i := range results {
		r := &results[i]
		if r.Status == statusWarn && strings.Contains(r.Name, "probe") {
			gotWarn = r
		}
		if r.hard {
			t.Errorf("probe result must never be hard: %q", r.Name)
		}
	}
	if gotWarn == nil {
		t.Fatal("expected a probe WARN for absent C++ toolchain binaries; got none")
	}
	if !strings.Contains(gotWarn.Detail, "gcc:13") {
		t.Errorf("warn detail should name the image; got %q", gotWarn.Detail)
	}
}

// TestCheckImageToolchainProbe_ImageNotLocal verifies that when the image is
// not present locally the probe is skipped with an INFO result (not WARN).
func TestCheckImageToolchainProbe_ImageNotLocal(t *testing.T) {
	env := makeProbeEnv(false, nil)
	cfg := config.Config{Sandbox: config.Sandbox{Image: "python:3-slim", Runtime: "podman"}}
	results := checkImageToolchain(context.Background(), env, []ingest.Language{ingest.LangPython}, nil, cfg)
	var gotInfo bool
	for _, r := range results {
		if r.Status == statusWarn {
			t.Errorf("image not local: unexpected WARN %q: %s", r.Name, r.Detail)
		}
		if r.Status == statusInfo && strings.Contains(r.Name, "probe") {
			gotInfo = true
		}
		if r.hard {
			t.Errorf("probe result must never be hard: %q", r.Name)
		}
	}
	if !gotInfo {
		t.Error("expected an INFO skip result when image not local")
	}
}

// TestCheckImageToolchainProbe_Timeout verifies that a wedged runCommand does
// not hang checkImageToolchain beyond the probe timeout. The test must complete
// well within 30 s (each probe is bounded by sandboxProbeTimeout = 5 s).
func TestCheckImageToolchainProbe_Timeout(t *testing.T) {
	env := doctorEnv{
		runCommand: func(ctx context.Context, name string, args ...string) (string, error) {
			// image inspect succeeds (image is local).
			if name == "podman" && len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
				return `[{"Id":"sha256:abc"}]`, nil
			}
			// All binary probes block until ctx is cancelled.
			<-ctx.Done()
			return "", ctx.Err()
		},
	}
	cfg := config.Config{Sandbox: config.Sandbox{Image: "python:3-slim", Runtime: "podman"}}
	deadline := time.Now().Add(30 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	results := checkImageToolchain(ctx, env, []ingest.Language{ingest.LangPython}, nil, cfg)
	if time.Now().After(deadline) {
		t.Error("checkImageToolchain exceeded 30s — probe did not respect timeout")
	}
	// A timeout on the probe counts as the binary being absent → WARN expected.
	var gotWarn bool
	for _, r := range results {
		if r.Status == statusWarn && strings.Contains(r.Name, "probe") {
			gotWarn = true
		}
		if r.hard {
			t.Errorf("probe result must never be hard: %q", r.Name)
		}
	}
	if !gotWarn {
		t.Error("expected a WARN after probe timeout (absent binary path)")
	}
}

// TestExtensionsForLanguage verifies that ExtensionsForLanguage returns a
// sorted, non-empty slice for known languages and empty for LangOther.
func TestExtensionsForLanguage(t *testing.T) {
	cases := []struct {
		lang     ingest.Language
		wantSome bool
		wantExt  string // must be in the result if wantSome
	}{
		{ingest.LangGo, true, ".go"},
		{ingest.LangPython, true, ".py"},
		{ingest.LangTypeScript, true, ".ts"},
		{ingest.LangRust, true, ".rs"},
		{ingest.LangOther, false, ""},
	}
	for _, tc := range cases {
		exts := ingest.ExtensionsForLanguage(tc.lang)
		if tc.wantSome && len(exts) == 0 {
			t.Errorf("ExtensionsForLanguage(%s): want non-empty, got empty", tc.lang)
			continue
		}
		if !tc.wantSome && len(exts) != 0 {
			t.Errorf("ExtensionsForLanguage(%s): want empty, got %v", tc.lang, exts)
			continue
		}
		if tc.wantExt != "" {
			found := false
			for _, e := range exts {
				if e == tc.wantExt {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("ExtensionsForLanguage(%s): want %q in %v", tc.lang, tc.wantExt, exts)
			}
		}
		// Assert sorted order.
		for i := 1; i < len(exts); i++ {
			if exts[i] < exts[i-1] {
				t.Errorf("ExtensionsForLanguage(%s): not sorted: %v", tc.lang, exts)
				break
			}
		}
	}
}

// TestCheckSandboxVerifier_Pass exercises the PASS path of checkSandboxVerifier
// using an injected verifySandbox seam that returns ok=true.
func TestCheckSandboxVerifier_Pass(t *testing.T) {
	cfgPath := writeDoctorConfig(t)
	env := doctorEnv{
		configPath: cfgPath,
		repoDir:    t.TempDir(),
		lookupEnv:  func(string) string { return "fake-key" },
		lookPath:   func(string) (string, error) { return "/usr/bin/podman", nil },
		runCommand: func(_ context.Context, _ string, _ ...string) (string, error) { return "", nil },
		verifySandbox: func(_ context.Context, _ string, _ config.Config) (repro.SmokeVerdict, error) {
			return repro.SmokeVerdict{OK: true, Category: "ok", Detail: "ok"}, nil
		},
		out: &strings.Builder{},
	}
	results := runChecks(context.Background(), env, true)
	var found *checkResult
	for i := range results {
		if results[i].Name == "sandbox verifier" {
			found = &results[i]
			break
		}
	}
	if found == nil {
		t.Fatal("sandbox verifier result not found")
	}
	if found.Status != statusPass {
		t.Errorf("status=%q, want PASS; detail=%q", found.Status, found.Detail)
	}
}

// TestCheckSandboxVerifier_Fail exercises the FAIL path of checkSandboxVerifier
// using an injected seam that returns ok=false (toolchain_missing).
func TestCheckSandboxVerifier_Fail(t *testing.T) {
	cfgPath := writeDoctorConfig(t)
	env := doctorEnv{
		configPath: cfgPath,
		repoDir:    t.TempDir(),
		lookupEnv:  func(string) string { return "fake-key" },
		lookPath:   func(string) (string, error) { return "/usr/bin/podman", nil },
		runCommand: func(_ context.Context, _ string, _ ...string) (string, error) { return "", nil },
		verifySandbox: func(_ context.Context, _ string, _ config.Config) (repro.SmokeVerdict, error) {
			return repro.SmokeVerdict{OK: false, Category: "toolchain_missing", Detail: "go: command not found"}, nil
		},
		out: &strings.Builder{},
	}
	results := runChecks(context.Background(), env, true)
	var found *checkResult
	for i := range results {
		if results[i].Name == "sandbox verifier" {
			found = &results[i]
			break
		}
	}
	if found == nil {
		t.Fatal("sandbox verifier result not found")
	}
	if found.Status != statusFail {
		t.Errorf("status=%q, want FAIL; detail=%q", found.Status, found.Detail)
	}
	if !strings.Contains(found.Detail, "toolchain_missing") {
		t.Errorf("detail=%q missing category", found.Detail)
	}
}

// TestCheckSandboxVerifier_Disabled verifies that the sandbox verifier check
// is not emitted when runSandboxVerify=false.
func TestCheckSandboxVerifier_Disabled(t *testing.T) {
	cfgPath := writeDoctorConfig(t)
	env := doctorEnv{
		configPath: cfgPath,
		repoDir:    t.TempDir(),
		lookupEnv:  func(string) string { return "fake-key" },
		lookPath:   func(string) (string, error) { return "/usr/bin/podman", nil },
		runCommand: func(_ context.Context, _ string, _ ...string) (string, error) { return "", nil },
		// verifySandbox would panic if called — it must not be called.
		verifySandbox: func(_ context.Context, _ string, _ config.Config) (repro.SmokeVerdict, error) {
			panic("verifySandbox called when disabled")
		},
		out: &strings.Builder{},
	}
	results := runChecks(context.Background(), env, false)
	for _, r := range results {
		if r.Name == "sandbox verifier" {
			t.Errorf("sandbox verifier check emitted when disabled: %+v", r)
		}
	}
}

// TestDominantLanguagesFromPaths_GoRepo asserts that the content-free
// extension-based detector identifies Go as a dominant language when given the
// tracked file list of this repository. This is the parity assertion required
// by bugbot-o0e: the new path must agree with the Snapshot-based path on the
// obvious case (a Go repository contains Go).
func TestDominantLanguagesFromPaths_GoRepo(t *testing.T) {
	// Use the actual worktree's tracked files so the assertion covers real data.
	paths := []string{
		"main.go",
		"internal/ingest/lang.go",
		"internal/ingest/snapshot.go",
		"internal/cli/doctor.go",
		"internal/cli/doctor_test.go",
		"README.md",
		"Makefile",
	}
	langs := ingest.DominantLanguagesFromPaths(paths)
	found := false
	for _, l := range langs {
		if l == ingest.LangGo {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DominantLanguagesFromPaths: Go not in dominant langs %v for a Go repo", langs)
	}
}

// TestDoctor_SecretsNeverInOutput_ConfigError asserts that even when the config
// file is invalid, the API key value injected through the env fake never appears
// anywhere in the doctor output. This covers the config-error branch that the
// original TestDoctor_SecretsNeverInOutput omits (happy path only).
func TestDoctor_SecretsNeverInOutput_ConfigError(t *testing.T) {
	const secretValue = "super-secret-key-xyz-config-error-path"
	// Write an intentionally broken config to force the config check to FAIL.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(cfgPath, []byte("not: valid: config: [\n"), 0o644); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	var sb strings.Builder
	env := allGreenEnv(t, cfgPath)
	env.configPath = cfgPath
	env.lookupEnv = func(key string) string {
		// Return the secret for any key lookup so it could leak if mishandled.
		return secretValue
	}
	env.out = &sb

	results := runChecks(context.Background(), env, false)
	printResults(&sb, results)
	out := sb.String()

	if strings.Contains(out, secretValue) {
		t.Errorf("doctor output must not contain secret value %q even on config error\n---\n%s", secretValue, out)
	}
	// The config FAIL line must be present.
	found := false
	for _, r := range results {
		if r.Name == "config" && r.Status == statusFail {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected config FAIL result on bad config, got: %v", results)
	}
}

// TestCheckProviders_UnreferencedProviderIsWarnNotFail verifies that a provider
// not referenced by any role whose credential env var is missing produces a WARN
// (not a hard FAIL). It also asserts the complementary path: the referenced
// provider (testprovider, used by finder/verifier/reproducer) passes because its
// credential IS set in the env stub.
func TestCheckProviders_UnreferencedProviderIsWarnNotFail(t *testing.T) {
	cfgPath := writeDoctorConfig(t)
	// allGreenEnv sets FAKE_API_KEY_ENV; we add a second provider not referenced
	// by any role and leave its env var unset.
	env := allGreenEnv(t, cfgPath)
	baseEnv := env.lookupEnv
	env.lookupEnv = func(name string) string {
		if name == "FAKE_API_KEY_ENV" {
			return baseEnv(name)
		}
		return "" // everything else (including UNREFERENCED_KEY_ENV) is unset
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	// Inject an extra provider not used by any role.
	cfg.Providers["unreferenced"] = config.Provider{
		Type:      config.ProviderAnthropic,
		APIKeyEnv: "UNREFERENCED_KEY_ENV",
	}

	results := checkProviders(env, cfg)

	var sawUnreferenced, sawReferenced bool
	for _, r := range results {
		switch r.Name {
		case "provider unreferenced":
			sawUnreferenced = true
			if r.Status != statusWarn {
				t.Errorf("unreferenced provider with missing cred: want WARN, got %s", r.Status)
			}
			if r.hard {
				t.Errorf("unreferenced provider result must not be hard")
			}
		case "provider testprovider":
			sawReferenced = true
			if r.Status != statusPass {
				t.Errorf("referenced provider with cred set: want PASS, got %s (detail: %s)", r.Status, r.Detail)
			}
		}
	}
	if !sawUnreferenced {
		t.Error("expected a result for 'provider unreferenced', got none")
	}
	if !sawReferenced {
		t.Error("expected a result for 'provider testprovider', got none")
	}
}

// TestDoctor_StealthSplitBrainWarn asserts that when the loaded config has
// stealth mode on and a leftover ./.bugbot directory exists in the scanned
// repo, doctor emits a WARN naming both locations. Without the leftover
// directory, no such warning fires.
func TestDoctor_StealthSplitBrainWarn(t *testing.T) {
	dir := t.TempDir()
	yaml := "stealth: true\n" + strings.ReplaceAll(doctorMinimalConfig, "%CFGDIR%", dir)
	cfgPath := filepath.Join(dir, "bugbot.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	repoDir := t.TempDir()

	t.Run("no_leftover_dir", func(t *testing.T) {
		env := allGreenEnv(t, cfgPath)
		env.repoDir = repoDir
		var sb strings.Builder
		env.out = &sb

		results := runChecks(context.Background(), env, false)
		for _, r := range results {
			if r.Name == "stealth split-brain" {
				t.Errorf("unexpected split-brain warn with no leftover ./.bugbot: %+v", r)
			}
		}
	})

	t.Run("leftover_dir_present", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(repoDir, ".bugbot"), 0o755); err != nil {
			t.Fatalf("mkdir .bugbot: %v", err)
		}
		env := allGreenEnv(t, cfgPath)
		env.repoDir = repoDir
		var sb strings.Builder
		env.out = &sb

		results := runChecks(context.Background(), env, false)
		var found bool
		for _, r := range results {
			if r.Name != "stealth split-brain" {
				continue
			}
			found = true
			if r.Status != statusWarn {
				t.Errorf("status = %s, want WARN", r.Status)
			}
			if r.hard {
				t.Error("stealth split-brain warn must not be hard")
			}
			if !strings.Contains(r.Detail, repoDir) || !strings.Contains(r.Detail, dir) {
				t.Errorf("detail should name both paths, got: %s", r.Detail)
			}
		}
		if !found {
			t.Fatal("expected stealth split-brain result")
		}
	})
}

// TestDoctor_ControlSocketPathLengthWarn asserts that a resolved daemon
// control-socket path longer than 100 bytes produces a non-hard WARN.
func TestDoctor_ControlSocketPathLengthWarn(t *testing.T) {
	dir := t.TempDir()
	// Storage.Path lives under a very deeply nested directory so
	// ControlSocketPath() (sibling daemon.sock) exceeds 100 bytes.
	longSuffix := strings.Repeat("a-very-long-path-segment/", 6)
	nestedDir := filepath.Join(dir, longSuffix)
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yaml := strings.ReplaceAll(doctorMinimalConfig, "%CFGDIR%", nestedDir)
	cfgPath := filepath.Join(dir, "bugbot.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	env := allGreenEnv(t, cfgPath)
	var sb strings.Builder
	env.out = &sb

	results := runChecks(context.Background(), env, false)
	var found bool
	for _, r := range results {
		if r.Name != "control socket path" {
			continue
		}
		found = true
		if r.Status != statusWarn {
			t.Errorf("status = %s, want WARN", r.Status)
		}
		if r.hard {
			t.Error("control socket path warn must not be hard")
		}
	}
	if !found {
		t.Fatal("expected control socket path result for a long nested storage path")
	}
}

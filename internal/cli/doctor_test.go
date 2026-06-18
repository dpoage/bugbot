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

	results := runChecks(context.Background(), env)
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

	results := runChecks(context.Background(), env)
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

	results := runChecks(context.Background(), env)

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
	results := runChecks(context.Background(), env)
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

	results := runChecks(context.Background(), env)

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

	results := runChecks(context.Background(), env)
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

	results := runChecks(context.Background(), env)

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

	results := runChecks(context.Background(), env)

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

	results := runChecks(context.Background(), env)

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

	results := runChecks(context.Background(), env)

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
// advisory's exact behavior is locked in independently of runChecks.
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
			name:     "all-covering image suppresses warns",
			image:    "example.com/stack:golang-node-python",
			network:  "none",
			langs:    []ingest.Language{ingest.LangGo, ingest.LangTypeScript, ingest.LangPython},
			wantWarn: nil,
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
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Sandbox: config.Sandbox{Image: tc.image, Network: tc.network}}
			got := checkImageToolchain(tc.langs, tc.buildSystems, cfg)
			gotNames := make([]string, 0, len(got))
			for _, r := range got {
				if r.hard {
					t.Errorf("check %q must not be hard", r.Name)
				}
				if r.Status != statusWarn {
					t.Errorf("check %q should be WARN; got %s", r.Name, r.Status)
				}
				gotNames = append(gotNames, r.Name)
			}
			for _, w := range tc.wantWarn {
				found := false
				for _, n := range gotNames {
					if strings.Contains(n, w) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected a warn containing %q; got %v", w, gotNames)
				}
			}
			if len(tc.wantWarn) == 0 && len(got) != 0 {
				t.Errorf("expected no warns; got %v", gotNames)
			}
		})
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

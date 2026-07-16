package sandbox

import (
	"context"
	"strings"
	"testing"

	ecoreg "github.com/dpoage/bugbot/internal/ecosystem"
)

// probeByName returns the ProbeEntry with the given name from
// ecoreg.ProbeEntries, or panics (test setup failure).
func probeByName(t *testing.T, name string) ecoreg.ProbeEntry {
	t.Helper()
	for _, e := range ecoreg.ProbeEntries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("probeByName: no ProbeEntry named %q in ecoreg.ProbeEntries", name)
	panic("unreachable")
}

// toProbeResult converts a sandbox.Result to an ecoreg.ProbeResult for tests.
func toProbeResult(r Result) ecoreg.ProbeResult {
	return ecoreg.ProbeResult{ExitCode: r.ExitCode, Stdout: r.Stdout}
}

// TestGoProbeInterpret tests the Go capability probe's interpret function
// directly, covering base-presence ("present", bugbot-bslx), cgo-present,
// cgo-absent, and probe-error cases.
func TestGoProbeInterpret(t *testing.T) {
	probe := probeByName(t, "go")

	t.Run("present_and_race_available_when_both_tokens_emitted", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: "go\nrace\n"}))
		if !modes["present"] {
			t.Errorf("want present=true when the go token is emitted, got %v", modes)
		}
		if !modes["race"] {
			t.Errorf("want race=true when the race token is emitted, got %v", modes)
		}
	})

	t.Run("present_true_race_false_when_only_go_token", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: "go\n"}))
		if !modes["present"] {
			t.Errorf("want present=true, got %v", modes)
		}
		if modes["race"] {
			t.Errorf("want race=false when CGO_ENABLED != 1 / no C compiler, got %v", modes)
		}
	})

	t.Run("both_unavailable_when_nonzero_exit_no_tokens", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 1, Stdout: ""}))
		if modes["present"] || modes["race"] {
			t.Errorf("want present=false race=false when exit non-zero and no tokens, got %v", modes)
		}
	})

	t.Run("both_unavailable_empty_stdout", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: ""}))
		if modes["present"] || modes["race"] {
			t.Errorf("want present=false race=false when stdout empty, got %v", modes)
		}
	})
}

// TestProbeCapabilitiesMock tests ProbeCapabilities using sandbox.NewMock,
// covering: go present, cgo present, cgo absent, Exec error → unavailable,
// and cache hit.
func TestProbeCapabilitiesMock(t *testing.T) {
	t.Run("present_and_race_available_when_go_and_race_tokens", func(t *testing.T) {
		InvalidateCapabilityCache("test-image-race-available")
		mock := NewMock(MockResponse{Result: Result{ExitCode: 0, Stdout: "go\nrace\n"}})
		cs := ProbeCapabilities(context.Background(), mock, "test-image-race-available", t.TempDir(), nil, nil)
		if !cs.Available("go", "present") {
			t.Errorf("want present available, got %v", cs)
		}
		if !cs.Available("go", "race") {
			t.Errorf("want race available, got %v", cs)
		}
	})

	t.Run("race_unavailable_when_cgo_disabled", func(t *testing.T) {
		InvalidateCapabilityCache("test-image-race-unavailable")
		mock := NewMock(MockResponse{Result: Result{ExitCode: 0, Stdout: "go\n"}})
		cs := ProbeCapabilities(context.Background(), mock, "test-image-race-unavailable", t.TempDir(), nil, nil)
		if !cs.Available("go", "present") {
			t.Errorf("want present available, got %v", cs)
		}
		if cs.Available("go", "race") {
			t.Errorf("want race unavailable when CGO_ENABLED=0, got %v", cs)
		}
	})

	t.Run("present_unavailable_when_go_missing", func(t *testing.T) {
		InvalidateCapabilityCache("test-image-go-missing")
		mock := NewMock(MockResponse{Result: Result{ExitCode: 127, Stdout: ""}})
		cs := ProbeCapabilities(context.Background(), mock, "test-image-go-missing", t.TempDir(), nil, nil)
		if cs.Available("go", "present") {
			t.Errorf("want present unavailable when the probe emits no go token, got %v", cs)
		}
	})

	t.Run("both_unavailable_on_exec_error", func(t *testing.T) {
		InvalidateCapabilityCache("test-image-exec-error")
		mock := NewMock(MockResponse{Err: errProbeTest})
		cs := ProbeCapabilities(context.Background(), mock, "test-image-exec-error", t.TempDir(), nil, nil)
		if cs.Available("go", "present") {
			t.Errorf("want present unavailable on exec error, got %v", cs)
		}
		if cs.Available("go", "race") {
			t.Errorf("want race unavailable on exec error, got %v", cs)
		}
	})

	t.Run("cache_hit_returns_same_result", func(t *testing.T) {
		InvalidateCapabilityCache("test-image-cache-hit")
		mock := NewMock(MockResponse{Result: Result{ExitCode: 0, Stdout: "go\nrace\n"}})
		cs1 := ProbeCapabilities(context.Background(), mock, "test-image-cache-hit", t.TempDir(), nil, nil)
		cs2 := ProbeCapabilities(context.Background(), mock, "test-image-cache-hit", t.TempDir(), nil, nil)
		if cs1["go"]["race"] != cs2["go"]["race"] {
			t.Errorf("cache hit must return same result: cs1=%v cs2=%v", cs1, cs2)
		}
	})

	t.Run("nil_sandbox_returns_unavailable", func(t *testing.T) {
		InvalidateCapabilityCache("any")
		cs := ProbeCapabilities(context.Background(), nil, "any", "", nil, nil)
		if cs.Available("go", "race") {
			t.Errorf("want race unavailable for nil sandbox, got %v", cs)
		}
	})

	t.Run("empty_repoDir_returns_unavailable", func(t *testing.T) {
		InvalidateCapabilityCache("any")
		mock := NewMock(MockResponse{Result: Result{ExitCode: 0, Stdout: "go\nrace\n"}})
		cs := ProbeCapabilities(context.Background(), mock, "any", "", nil, nil)
		if cs.Available("go", "race") {
			t.Errorf("want race unavailable for empty repoDir, got %v", cs)
		}
	})
}

// TestProbeCapabilities_MountsThreadedIntoEveryProbe verifies that mounts and
// env are attached to every probe Spec, and that they change the returned
// CapabilitySet — this is what makes a host-mounted toolchain (bugbot-14g0
// fix A) show up as available in the probe results (acceptance 4).
func TestProbeCapabilities_MountsThreadedIntoEveryProbe(t *testing.T) {
	InvalidateCapabilityCache("test-image-toolchain-mount")
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0, Stdout: "node\nnode_test\n"}})
	mounts := []ROMount{{HostPath: "/host/node", ContainerPath: "/opt/bugbot-toolchains/node", Shared: true}}
	env := []string{"PATH=/opt/bugbot-toolchains/node/bin:/usr/bin"}

	cs := ProbeCapabilities(context.Background(), mock, "test-image-toolchain-mount", t.TempDir(), mounts, env)
	if !cs.Available("js", "node") {
		t.Errorf("want js/node available once the mocked probe reports it, got %v", cs)
	}

	for _, c := range mock.Calls() {
		if len(c.Spec.ROMounts) != 1 || c.Spec.ROMounts[0].HostPath != "/host/node" {
			t.Errorf("probe call missing the host toolchain mount: %+v", c.Spec.ROMounts)
		}
		found := false
		for _, e := range c.Spec.Env {
			if e == env[0] {
				found = true
			}
		}
		if !found {
			t.Errorf("probe call missing the toolchain PATH env, got %v", c.Spec.Env)
		}
	}
}

// TestProbeCapabilities_CacheKeyDependsOnMounts verifies that two calls for
// the SAME image but DIFFERENT mounts do not share a cache entry — otherwise
// a probe run before a toolchain was mounted would poison the result for
// every later call with the mount attached.
func TestProbeCapabilities_CacheKeyDependsOnMounts(t *testing.T) {
	InvalidateCapabilityCache("test-image-mount-cache-key")
	mock := NewMock(MockResponse{Result: Result{ExitCode: 1}}) // no node without the mount
	mock.ResponseFunc = func(_ int, spec Spec) (Result, error) {
		if len(spec.ROMounts) > 0 {
			return Result{ExitCode: 0, Stdout: "node\nnode_test\n"}, nil
		}
		return Result{ExitCode: 1}, nil
	}

	without := ProbeCapabilities(context.Background(), mock, "test-image-mount-cache-key", t.TempDir(), nil, nil)
	if without.Available("js", "node") {
		t.Fatalf("without a mount, js/node should be unavailable, got %v", without)
	}

	mounts := []ROMount{{HostPath: "/host/node", ContainerPath: "/opt/bugbot-toolchains/node", Shared: true}}
	with := ProbeCapabilities(context.Background(), mock, "test-image-mount-cache-key", t.TempDir(), mounts, nil)
	if !with.Available("js", "node") {
		t.Errorf("with the mount, js/node should be available (must not reuse the mount-less cache entry), got %v", with)
	}
}

// errProbeTest is a sentinel error for probe-failure tests.
var errProbeTest = probeTestErr("simulated exec error")

type probeTestErr string

func (e probeTestErr) Error() string { return string(e) }

// TestCapabilitySetAvailable tests the CapabilitySet.Available helper.
func TestCapabilitySetAvailable(t *testing.T) {
	cs := CapabilitySet{
		"go": {"race": true},
	}
	if !cs.Available("go", "race") {
		t.Error("Available(go, race) = false, want true")
	}
	if cs.Available("go", "missing") {
		t.Error("Available(go, missing) = true, want false")
	}
	if cs.Available("missing", "race") {
		t.Error("Available(missing, race) = true, want false")
	}
	var nilCS CapabilitySet
	if nilCS.Available("go", "race") {
		t.Error("nil.Available = true, want false")
	}
}

// TestGoCapabilityProbeSpec verifies the probe is using /bin/sh and the
// correct command to test cgo + C compiler availability.
func TestGoCapabilityProbeSpec(t *testing.T) {
	probe := probeByName(t, "go")
	if len(probe.Probe) == 0 {
		t.Fatal("go probe Probe must be non-empty")
	}
	if probe.Probe[0] != "/bin/sh" {
		t.Errorf("Probe[0] = %q, want /bin/sh", probe.Probe[0])
	}
	if probe.Name != "go" {
		t.Errorf("probe Name = %q, want go", probe.Name)
	}
}

// TestCppProbeInterpret tests the C++ capability probe's interpret function
// directly, covering full-available, partial-available, and probe-error cases.
func TestCppProbeInterpret(t *testing.T) {
	probe := probeByName(t, "cpp")

	t.Run("asan_and_tsan_available_ubsan_absent", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: "address\nthread\n"}))
		if !modes["asan"] {
			t.Errorf("want asan=true, got false")
		}
		if !modes["tsan"] {
			t.Errorf("want tsan=true, got false")
		}
		if modes["ubsan"] {
			t.Errorf("want ubsan=false, got true")
		}
	})

	t.Run("all_modes_unavailable_on_nonzero_empty_stdout", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 1, Stdout: ""}))
		if modes["asan"] || modes["tsan"] || modes["ubsan"] {
			t.Errorf("want all false on non-zero exit with empty stdout, got %v", modes)
		}
		for _, k := range []string{"asan", "tsan", "ubsan"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("key %q missing from interpret result", k)
			}
		}
	})

	t.Run("full_key_set_always_returned", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 1, Stdout: ""}))
		for _, k := range []string{"asan", "tsan", "ubsan"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("allFalse requires key %q but it is missing", k)
			}
		}
	})

	t.Run("ubsan_only", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: "undefined\n"}))
		if modes["asan"] {
			t.Errorf("want asan=false")
		}
		if modes["tsan"] {
			t.Errorf("want tsan=false")
		}
		if !modes["ubsan"] {
			t.Errorf("want ubsan=true")
		}
	})
}

// TestCppCapabilityProbeSpec verifies the probe uses /bin/sh and is named "cpp".
func TestCppCapabilityProbeSpec(t *testing.T) {
	probe := probeByName(t, "cpp")
	if len(probe.Probe) == 0 {
		t.Fatal("cpp probe Probe must be non-empty")
	}
	if probe.Probe[0] != "/bin/sh" {
		t.Errorf("Probe[0] = %q, want /bin/sh", probe.Probe[0])
	}
	if probe.Name != "cpp" {
		t.Errorf("probe Name = %q, want cpp", probe.Name)
	}
}

// TestRustProbeInterpret tests the Rust capability probe's interpret function.
func TestRustProbeInterpret(t *testing.T) {
	probe := probeByName(t, "rust")

	t.Run("cargo_and_miri_available", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: "cargo\nmiri\n"}))
		if !modes["cargo"] {
			t.Errorf("want cargo=true, got false")
		}
		if !modes["miri"] {
			t.Errorf("want miri=true, got false")
		}
	})

	t.Run("cargo_only_miri_absent", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: "cargo\n"}))
		if !modes["cargo"] {
			t.Errorf("want cargo=true, got false")
		}
		if modes["miri"] {
			t.Errorf("want miri=false, got true")
		}
	})

	t.Run("all_modes_unavailable_on_nonzero_empty_stdout", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 1, Stdout: ""}))
		if modes["cargo"] || modes["miri"] {
			t.Errorf("want all false on non-zero exit with empty stdout, got %v", modes)
		}
		for _, k := range []string{"cargo", "miri"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("key %q missing from interpret result", k)
			}
		}
	})

	t.Run("full_key_set_always_returned", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 1, Stdout: ""}))
		for _, k := range []string{"cargo", "miri"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("allFalse requires key %q but it is missing", k)
			}
		}
	})
}

// TestRustCapabilityProbeSpec verifies the probe uses /bin/sh and is named "rust".
func TestRustCapabilityProbeSpec(t *testing.T) {
	probe := probeByName(t, "rust")
	if len(probe.Probe) == 0 {
		t.Fatal("rust probe Probe must be non-empty")
	}
	if probe.Probe[0] != "/bin/sh" {
		t.Errorf("Probe[0] = %q, want /bin/sh", probe.Probe[0])
	}
	if probe.Name != "rust" {
		t.Errorf("probe Name = %q, want rust", probe.Name)
	}
}

// TestJsProbeInterpret tests the JS capability probe's interpret function.
func TestJsProbeInterpret(t *testing.T) {
	probe := probeByName(t, "js")

	t.Run("node_and_node_test_available", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: "node\nnode_test\n"}))
		if !modes["node"] {
			t.Errorf("want node=true, got false")
		}
		if !modes["node_test"] {
			t.Errorf("want node_test=true, got false")
		}
	})

	t.Run("node_only_node_test_absent", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: "node\n"}))
		if !modes["node"] {
			t.Errorf("want node=true, got false")
		}
		if modes["node_test"] {
			t.Errorf("want node_test=false, got true")
		}
	})

	t.Run("all_modes_unavailable_on_nonzero_empty_stdout", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 1, Stdout: ""}))
		if modes["node"] || modes["node_test"] {
			t.Errorf("want all false on non-zero exit with empty stdout, got %v", modes)
		}
		for _, k := range []string{"node", "node_test"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("key %q missing from interpret result", k)
			}
		}
	})

	t.Run("full_key_set_always_returned", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 1, Stdout: ""}))
		for _, k := range []string{"node", "node_test"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("allFalse requires key %q but it is missing", k)
			}
		}
	})
}

// TestJsCapabilityProbeSpec verifies the probe uses /bin/sh and is named "js".
func TestJsCapabilityProbeSpec(t *testing.T) {
	probe := probeByName(t, "js")
	if len(probe.Probe) == 0 {
		t.Fatal("js probe Probe must be non-empty")
	}
	if probe.Probe[0] != "/bin/sh" {
		t.Errorf("Probe[0] = %q, want /bin/sh", probe.Probe[0])
	}
	if probe.Name != "js" {
		t.Errorf("probe Name = %q, want js", probe.Name)
	}
}

// TestBazelCapabilityProbeSpec verifies the probe uses /bin/sh, is named
// "bazel", EXECUTES the launchers (not command -v — a cold-cache bazelisk
// under network=none must not read as available, bugbot-4z7m), and reports
// per-launcher-name tokens so the gate and prompt can speak the exact argv
// that works (a bazelisk-only PATH must not advertise a working `bazel`).
func TestBazelCapabilityProbeSpec(t *testing.T) {
	probe := probeByName(t, "bazel")
	if len(probe.Probe) == 0 {
		t.Fatal("bazel probe Probe must be non-empty")
	}
	if probe.Probe[0] != "/bin/sh" {
		t.Errorf("Probe[0] = %q, want /bin/sh", probe.Probe[0])
	}
	if probe.Name != "bazel" {
		t.Errorf("probe Name = %q, want bazel", probe.Name)
	}
	script := probe.Probe[len(probe.Probe)-1]
	for _, want := range []string{"bazel version", "bazelisk version"} {
		if !strings.Contains(script, want) {
			t.Errorf("probe script %q must execute %q, not merely command -v", script, want)
		}
	}
	cases := []struct {
		name                string
		stdout              string
		wantBazel, wantBisk bool
	}{
		{"both work", "bazel\nbazelisk\n", true, true},
		{"bazel only", "bazel\n", true, false},
		{"bazelisk only", "bazelisk\n", false, true},
		{"neither", "", false, false},
	}
	for _, tc := range cases {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: tc.stdout}))
		if modes["bazel"] != tc.wantBazel || modes["bazelisk"] != tc.wantBisk {
			t.Errorf("%s: modes = %v, want bazel=%v bazelisk=%v", tc.name, modes, tc.wantBazel, tc.wantBisk)
		}
	}
}

// TestPythonProbeInterpret tests the Python capability probe's interpret function.
func TestPythonProbeInterpret(t *testing.T) {
	probe := probeByName(t, "python")

	t.Run("python_and_pytest_available", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: "python\npytest\n"}))
		if !modes["python"] {
			t.Errorf("want python=true, got false")
		}
		if !modes["pytest"] {
			t.Errorf("want pytest=true, got false")
		}
		if modes["pytest_timeout"] {
			t.Errorf("want pytest_timeout=false without its token, got true")
		}
	})

	t.Run("pytest_timeout_plugin_available", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: "python\npytest\npytest_timeout\n"}))
		if !modes["pytest_timeout"] {
			t.Errorf("want pytest_timeout=true when its token is emitted, got false")
		}
	})

	t.Run("python_only_pytest_absent", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: "python\n"}))
		if !modes["python"] {
			t.Errorf("want python=true, got false")
		}
		if modes["pytest"] {
			t.Errorf("want pytest=false, got true")
		}
	})

	t.Run("all_modes_unavailable_on_nonzero_empty_stdout", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 1, Stdout: ""}))
		if modes["python"] || modes["pytest"] {
			t.Errorf("want all false on non-zero exit with empty stdout, got %v", modes)
		}
		for _, k := range []string{"python", "pytest", "pytest_timeout"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("key %q missing from interpret result", k)
			}
		}
	})

	t.Run("full_key_set_always_returned", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 1, Stdout: ""}))
		for _, k := range []string{"python", "pytest", "pytest_timeout"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("allFalse requires key %q but it is missing", k)
			}
		}
	})
}

// TestPythonCapabilityProbeSpec verifies the probe uses /bin/sh and is named "python".
func TestPythonCapabilityProbeSpec(t *testing.T) {
	probe := probeByName(t, "python")
	if len(probe.Probe) == 0 {
		t.Fatal("python probe Probe must be non-empty")
	}
	if probe.Probe[0] != "/bin/sh" {
		t.Errorf("Probe[0] = %q, want /bin/sh", probe.Probe[0])
	}
	if probe.Name != "python" {
		t.Errorf("probe Name = %q, want python", probe.Name)
	}
}

// TestInvalidateCapabilityCache_DeletesComposedKey regression-tests the
// latent bug an oracle review caught: ProbeCapabilities keys its cache on
// image+"|"+mountsEnvCacheKey(...), never on the bare image string, so a
// naive capCache.Delete(image) silently no-ops against every real entry.
// InvalidateCapabilityCache must delete every entry for image regardless of
// which mounts/env combination produced it.
func TestInvalidateCapabilityCache_DeletesComposedKey(t *testing.T) {
	image := "test-image-invalidate-composed-key"
	InvalidateCapabilityCache(image) // clean slate regardless of prior test order

	mockA := NewMock(MockResponse{Result: Result{ExitCode: 1}})
	mockB := NewMock(MockResponse{Result: Result{ExitCode: 0, Stdout: "node\nnode_test\n"}})

	// Two different mount sets against the SAME image populate two distinct
	// composed cache keys (image+"|"+<no mounts>) and (image+"|"+<mounts>).
	without := ProbeCapabilities(context.Background(), mockA, image, t.TempDir(), nil, nil)
	if without.Available("js", "node") {
		t.Fatalf("precondition: expected js/node unavailable without a mount, got %v", without)
	}
	mounts := []ROMount{{HostPath: "/host/node", ContainerPath: "/opt/bugbot-toolchains/node", Shared: true}}
	with := ProbeCapabilities(context.Background(), mockB, image, t.TempDir(), mounts, nil)
	if !with.Available("js", "node") {
		t.Fatalf("precondition: expected js/node available with a mount, got %v", with)
	}

	InvalidateCapabilityCache(image)

	// After invalidation, BOTH composed entries must be gone — re-probing
	// with mockA now (a mock that always reports unavailable) for the
	// previously-available "with mounts" case must reflect the fresh probe,
	// not a stale cached true.
	reprobed := ProbeCapabilities(context.Background(), mockA, image, t.TempDir(), mounts, nil)
	if reprobed.Available("js", "node") {
		t.Errorf("stale cache entry survived InvalidateCapabilityCache: got %v after re-probing with an always-unavailable mock", reprobed)
	}
}

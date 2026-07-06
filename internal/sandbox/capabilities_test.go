package sandbox

import (
	"context"
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
// directly, covering cgo-present, cgo-absent, and probe-error cases.
func TestGoProbeInterpret(t *testing.T) {
	probe := probeByName(t, "go")

	t.Run("race_available_when_exit0_and_CGO_1", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: "1\n"}))
		if !modes["race"] {
			t.Errorf("want race=true when exit 0 and CGO_ENABLED=1, got %v", modes)
		}
	})

	t.Run("race_unavailable_when_exit0_CGO_0", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: "0\n"}))
		if modes["race"] {
			t.Errorf("want race=false when CGO_ENABLED=0, got %v", modes)
		}
	})

	t.Run("race_unavailable_when_nonzero_exit", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 1, Stdout: ""}))
		if modes["race"] {
			t.Errorf("want race=false when exit non-zero, got %v", modes)
		}
	})

	t.Run("race_unavailable_empty_stdout", func(t *testing.T) {
		modes := probe.Interpret(toProbeResult(Result{ExitCode: 0, Stdout: ""}))
		if modes["race"] {
			t.Errorf("want race=false when stdout empty, got %v", modes)
		}
	})
}

// TestProbeCapabilitiesMock tests ProbeCapabilities using sandbox.NewMock,
// covering: cgo present, cgo absent, Exec error → unavailable, and cache hit.
func TestProbeCapabilitiesMock(t *testing.T) {
	t.Run("race_available_when_cgo_enabled", func(t *testing.T) {
		InvalidateCapabilityCache("test-image-race-available")
		mock := NewMock(MockResponse{Result: Result{ExitCode: 0, Stdout: "1\n"}})
		cs := ProbeCapabilities(context.Background(), mock, "test-image-race-available", t.TempDir())
		if !cs.Available("go", "race") {
			t.Errorf("want race available, got %v", cs)
		}
	})

	t.Run("race_unavailable_when_cgo_disabled", func(t *testing.T) {
		InvalidateCapabilityCache("test-image-race-unavailable")
		mock := NewMock(MockResponse{Result: Result{ExitCode: 0, Stdout: "0\n"}})
		cs := ProbeCapabilities(context.Background(), mock, "test-image-race-unavailable", t.TempDir())
		if cs.Available("go", "race") {
			t.Errorf("want race unavailable when CGO_ENABLED=0, got %v", cs)
		}
	})

	t.Run("race_unavailable_on_exec_error", func(t *testing.T) {
		InvalidateCapabilityCache("test-image-exec-error")
		mock := NewMock(MockResponse{Err: errProbeTest})
		cs := ProbeCapabilities(context.Background(), mock, "test-image-exec-error", t.TempDir())
		if cs.Available("go", "race") {
			t.Errorf("want race unavailable on exec error, got %v", cs)
		}
	})

	t.Run("cache_hit_returns_same_result", func(t *testing.T) {
		InvalidateCapabilityCache("test-image-cache-hit")
		mock := NewMock(MockResponse{Result: Result{ExitCode: 0, Stdout: "1\n"}})
		cs1 := ProbeCapabilities(context.Background(), mock, "test-image-cache-hit", t.TempDir())
		cs2 := ProbeCapabilities(context.Background(), mock, "test-image-cache-hit", t.TempDir())
		if cs1["go"]["race"] != cs2["go"]["race"] {
			t.Errorf("cache hit must return same result: cs1=%v cs2=%v", cs1, cs2)
		}
	})

	t.Run("nil_sandbox_returns_all_false", func(t *testing.T) {
		cs := ProbeCapabilities(context.Background(), nil, "any", "")
		if cs.Available("go", "race") {
			t.Errorf("want race unavailable for nil sandbox, got %v", cs)
		}
	})

	t.Run("empty_repoDir_returns_all_false", func(t *testing.T) {
		mock := NewMock(MockResponse{Result: Result{ExitCode: 0, Stdout: "1\n"}})
		cs := ProbeCapabilities(context.Background(), mock, "any", "")
		if cs.Available("go", "race") {
			t.Errorf("want race unavailable for empty repoDir, got %v", cs)
		}
	})
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

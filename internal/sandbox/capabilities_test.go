package sandbox

import (
	"context"
	"testing"
)

// TestGoProbeInterpret tests the Go capability probe's interpret function
// directly, covering cgo-present, cgo-absent, and probe-error cases.
func TestGoProbeInterpret(t *testing.T) {
	t.Run("race_available_when_exit0_and_CGO_1", func(t *testing.T) {
		r := Result{ExitCode: 0, Stdout: "1\n"}
		modes := goCapabilityProbe.interpret(r)
		if !modes["race"] {
			t.Errorf("want race=true when exit 0 and CGO_ENABLED=1, got %v", modes)
		}
	})

	t.Run("race_unavailable_when_exit0_CGO_0", func(t *testing.T) {
		// CGO_ENABLED=0 but command exited 0 (e.g. cc found but cgo disabled).
		r := Result{ExitCode: 0, Stdout: "0\n"}
		modes := goCapabilityProbe.interpret(r)
		if modes["race"] {
			t.Errorf("want race=false when CGO_ENABLED=0, got %v", modes)
		}
	})

	t.Run("race_unavailable_when_nonzero_exit", func(t *testing.T) {
		r := Result{ExitCode: 1, Stdout: ""}
		modes := goCapabilityProbe.interpret(r)
		if modes["race"] {
			t.Errorf("want race=false when exit non-zero, got %v", modes)
		}
	})

	t.Run("race_unavailable_empty_stdout", func(t *testing.T) {
		r := Result{ExitCode: 0, Stdout: ""}
		modes := goCapabilityProbe.interpret(r)
		if modes["race"] {
			t.Errorf("want race=false when stdout empty, got %v", modes)
		}
	})
}

// TestProbeCapabilitiesMock tests ProbeCapabilities using sandbox.NewMock,
// covering: cgo present, cgo absent, Exec error → unavailable, and cache hit.
func TestProbeCapabilitiesMock(t *testing.T) {
	ctx := context.Background()
	repoDir := t.TempDir()

	t.Run("cgo_present", func(t *testing.T) {
		InvalidateCapabilityCache("img-cgo-present")
		m := NewMock(MockResponse{Result: Result{ExitCode: 0, Stdout: "1\n"}})
		cs := ProbeCapabilities(ctx, m, "img-cgo-present", repoDir)
		if !cs.Available("go", "race") {
			t.Errorf("want race=true for cgo-present image, got %v", cs)
		}
		// One probe call per registered ecosystem (go + cpp + rust + js + python).
		if m.CallCount() != 5 {
			t.Errorf("want 5 probe Exec calls (one per ecosystem), got %d", m.CallCount())
		}
	})

	t.Run("cgo_absent_exit1", func(t *testing.T) {
		InvalidateCapabilityCache("img-cgo-absent")
		m := NewMock(MockResponse{Result: Result{ExitCode: 1, Stdout: ""}})
		cs := ProbeCapabilities(ctx, m, "img-cgo-absent", repoDir)
		if cs.Available("go", "race") {
			t.Errorf("want race=false for cgo-absent image, got %v", cs)
		}
	})

	t.Run("exec_error_yields_unavailable", func(t *testing.T) {
		InvalidateCapabilityCache("img-exec-error")
		m := NewMock(MockResponse{Err: errProbeTest})
		cs := ProbeCapabilities(ctx, m, "img-exec-error", repoDir)
		if cs.Available("go", "race") {
			t.Errorf("want race=false when Exec errors, got %v", cs)
		}
	})

	t.Run("cache_hit_no_second_exec", func(t *testing.T) {
		InvalidateCapabilityCache("img-cache-hit")
		m := NewMock(MockResponse{Result: Result{ExitCode: 0, Stdout: "1\n"}})
		cs1 := ProbeCapabilities(ctx, m, "img-cache-hit", repoDir)
		cs2 := ProbeCapabilities(ctx, m, "img-cache-hit", repoDir)
		// Must be the same CapabilitySet (pointer equality from sync.Map cache).
		if cs1["go"]["race"] != cs2["go"]["race"] {
			t.Errorf("cache returned different results: %v vs %v", cs1, cs2)
		}
		// Only one round of Exec calls (5 probes) should have fired despite two
		// ProbeCapabilities calls — the second call is a cache hit.
		if m.CallCount() != 5 {
			t.Errorf("want 5 Exec calls (one per ecosystem, cache hit on 2nd ProbeCapabilities), got %d", m.CallCount())
		}
	})

	t.Run("nil_sandbox_returns_empty", func(t *testing.T) {
		cs := ProbeCapabilities(ctx, nil, "img-nil", repoDir)
		if cs.Available("go", "race") {
			t.Errorf("want empty CapabilitySet for nil sandbox")
		}
	})

	t.Run("empty_repoDir_returns_empty", func(t *testing.T) {
		m := NewMock(MockResponse{Result: Result{ExitCode: 0, Stdout: "1\n"}})
		cs := ProbeCapabilities(ctx, m, "img-norepo", "")
		if cs.Available("go", "race") {
			t.Errorf("want empty CapabilitySet for empty repoDir")
		}
		if m.CallCount() != 0 {
			t.Errorf("want 0 Exec calls for empty repoDir, got %d", m.CallCount())
		}
	})
}

// errProbeTest is a sentinel error for probe-failure tests.
var errProbeTest = probeTestErr("simulated exec error")

type probeTestErr string

func (e probeTestErr) Error() string { return string(e) }

// TestCapabilitySetAvailable tests the CapabilitySet.Available helper.
func TestCapabilitySetAvailable(t *testing.T) {
	var nilCS CapabilitySet
	if nilCS.Available("go", "race") {
		t.Error("nil CapabilitySet.Available should return false")
	}

	cs := CapabilitySet{"go": {"race": true}}
	if !cs.Available("go", "race") {
		t.Error("Available(go, race) should be true")
	}
	if cs.Available("go", "unknown") {
		t.Error("Available for unknown mode should be false")
	}
	if cs.Available("python", "race") {
		t.Error("Available for unknown ecosystem should be false")
	}
}

// TestGoCapabilityProbeSpec verifies the probe is using /bin/sh and the
// correct command to test cgo + C compiler availability.
func TestGoCapabilityProbeSpec(t *testing.T) {
	if len(goCapabilityProbe.probe) == 0 {
		t.Fatal("goCapabilityProbe.probe must be non-empty")
	}
	if goCapabilityProbe.probe[0] != "/bin/sh" {
		t.Errorf("probe[0] = %q, want /bin/sh", goCapabilityProbe.probe[0])
	}
	if goCapabilityProbe.name != "go" {
		t.Errorf("probe name = %q, want go", goCapabilityProbe.name)
	}
}

// TestCppProbeInterpret tests the C++ capability probe's interpret function
// directly, covering full-available, partial-available, and probe-error cases.
// Mirrors TestGoProbeInterpret.
func TestCppProbeInterpret(t *testing.T) {
	t.Run("asan_and_tsan_available_ubsan_absent", func(t *testing.T) {
		r := Result{ExitCode: 0, Stdout: "address\nthread\n"}
		modes := cppCapabilityProbe.interpret(r)
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
		r := Result{ExitCode: 1, Stdout: ""}
		modes := cppCapabilityProbe.interpret(r)
		if modes["asan"] || modes["tsan"] || modes["ubsan"] {
			t.Errorf("want all false on non-zero exit with empty stdout, got %v", modes)
		}
		// Full key set must be present.
		for _, k := range []string{"asan", "tsan", "ubsan"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("key %q missing from interpret result", k)
			}
		}
	})

	t.Run("full_key_set_always_returned", func(t *testing.T) {
		// allFalse calls interpret(Result{ExitCode:1}); the returned map must
		// carry all three keys so allFalse can enumerate them.
		r := Result{ExitCode: 1, Stdout: ""}
		modes := cppCapabilityProbe.interpret(r)
		for _, k := range []string{"asan", "tsan", "ubsan"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("allFalse requires key %q but it is missing", k)
			}
		}
	})

	t.Run("ubsan_only", func(t *testing.T) {
		r := Result{ExitCode: 0, Stdout: "undefined\n"}
		modes := cppCapabilityProbe.interpret(r)
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
	if len(cppCapabilityProbe.probe) == 0 {
		t.Fatal("cppCapabilityProbe.probe must be non-empty")
	}
	if cppCapabilityProbe.probe[0] != "/bin/sh" {
		t.Errorf("probe[0] = %q, want /bin/sh", cppCapabilityProbe.probe[0])
	}
	if cppCapabilityProbe.name != "cpp" {
		t.Errorf("probe name = %q, want cpp", cppCapabilityProbe.name)
	}
}

// TestRustProbeInterpret tests the Rust capability probe's interpret function
// Mirrors TestCppProbeInterpret.
func TestRustProbeInterpret(t *testing.T) {
	t.Run("cargo_and_miri_available", func(t *testing.T) {
		r := Result{ExitCode: 0, Stdout: "cargo\nmiri\n"}
		modes := rustCapabilityProbe.interpret(r)
		if !modes["cargo"] {
			t.Errorf("want cargo=true, got false")
		}
		if !modes["miri"] {
			t.Errorf("want miri=true, got false")
		}
	})

	t.Run("cargo_only_miri_absent", func(t *testing.T) {
		r := Result{ExitCode: 0, Stdout: "cargo\n"}
		modes := rustCapabilityProbe.interpret(r)
		if !modes["cargo"] {
			t.Errorf("want cargo=true, got false")
		}
		if modes["miri"] {
			t.Errorf("want miri=false, got true")
		}
	})

	t.Run("all_modes_unavailable_on_nonzero_empty_stdout", func(t *testing.T) {
		r := Result{ExitCode: 1, Stdout: ""}
		modes := rustCapabilityProbe.interpret(r)
		if modes["cargo"] || modes["miri"] {
			t.Errorf("want all false on non-zero exit with empty stdout, got %v", modes)
		}
		// Full key set must be present.
		for _, k := range []string{"cargo", "miri"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("key %q missing from interpret result", k)
			}
		}
	})

	t.Run("full_key_set_always_returned", func(t *testing.T) {
		// allFalse calls interpret(Result{ExitCode:1}); the returned map must
		// carry both keys so allFalse can enumerate them.
		r := Result{ExitCode: 1, Stdout: ""}
		modes := rustCapabilityProbe.interpret(r)
		for _, k := range []string{"cargo", "miri"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("allFalse requires key %q but it is missing", k)
			}
		}
	})
}

// TestRustCapabilityProbeSpec verifies the probe uses /bin/sh and is named "rust".
func TestRustCapabilityProbeSpec(t *testing.T) {
	if len(rustCapabilityProbe.probe) == 0 {
		t.Fatal("rustCapabilityProbe.probe must be non-empty")
	}
	if rustCapabilityProbe.probe[0] != "/bin/sh" {
		t.Errorf("probe[0] = %q, want /bin/sh", rustCapabilityProbe.probe[0])
	}
	if rustCapabilityProbe.name != "rust" {
		t.Errorf("probe name = %q, want rust", rustCapabilityProbe.name)
	}
}

// TestJsProbeInterpret tests the JS capability probe's interpret function
// directly, covering full-available, partial-available, and probe-error cases.
// Mirrors TestCppProbeInterpret.
func TestJsProbeInterpret(t *testing.T) {
	t.Run("node_and_node_test_available", func(t *testing.T) {
		r := Result{ExitCode: 0, Stdout: "node\nnode_test\n"}
		modes := jsCapabilityProbe.interpret(r)
		if !modes["node"] {
			t.Errorf("want node=true, got false")
		}
		if !modes["node_test"] {
			t.Errorf("want node_test=true, got false")
		}
	})

	t.Run("node_only_node_test_absent", func(t *testing.T) {
		r := Result{ExitCode: 0, Stdout: "node\n"}
		modes := jsCapabilityProbe.interpret(r)
		if !modes["node"] {
			t.Errorf("want node=true, got false")
		}
		if modes["node_test"] {
			t.Errorf("want node_test=false, got true")
		}
	})

	t.Run("all_modes_unavailable_on_nonzero_empty_stdout", func(t *testing.T) {
		r := Result{ExitCode: 1, Stdout: ""}
		modes := jsCapabilityProbe.interpret(r)
		if modes["node"] || modes["node_test"] {
			t.Errorf("want all false on non-zero exit with empty stdout, got %v", modes)
		}
		// Full key set must be present.
		for _, k := range []string{"node", "node_test"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("key %q missing from interpret result", k)
			}
		}
	})

	t.Run("full_key_set_always_returned", func(t *testing.T) {
		// allFalse calls interpret(Result{ExitCode:1}); the returned map must
		// carry both keys so allFalse can enumerate them.
		r := Result{ExitCode: 1, Stdout: ""}
		modes := jsCapabilityProbe.interpret(r)
		for _, k := range []string{"node", "node_test"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("allFalse requires key %q but it is missing", k)
			}
		}
	})
}

// TestJsCapabilityProbeSpec verifies the probe uses /bin/sh and is named "js".
func TestJsCapabilityProbeSpec(t *testing.T) {
	if len(jsCapabilityProbe.probe) == 0 {
		t.Fatal("jsCapabilityProbe.probe must be non-empty")
	}
	if jsCapabilityProbe.probe[0] != "/bin/sh" {
		t.Errorf("probe[0] = %q, want /bin/sh", jsCapabilityProbe.probe[0])
	}
	if jsCapabilityProbe.name != "js" {
		t.Errorf("probe name = %q, want js", jsCapabilityProbe.name)
	}
}

// TestPythonProbeInterpret tests the Python capability probe's interpret
// function directly, covering full-available, partial-available, and
// probe-error cases. Mirrors TestCppProbeInterpret.
func TestPythonProbeInterpret(t *testing.T) {
	t.Run("python_and_pytest_available", func(t *testing.T) {
		r := Result{ExitCode: 0, Stdout: "python\npytest\n"}
		modes := pythonCapabilityProbe.interpret(r)
		if !modes["python"] {
			t.Errorf("want python=true, got false")
		}
		if !modes["pytest"] {
			t.Errorf("want pytest=true, got false")
		}
	})

	t.Run("python_only_pytest_absent", func(t *testing.T) {
		r := Result{ExitCode: 0, Stdout: "python\n"}
		modes := pythonCapabilityProbe.interpret(r)
		if !modes["python"] {
			t.Errorf("want python=true, got false")
		}
		if modes["pytest"] {
			t.Errorf("want pytest=false, got true")
		}
	})

	t.Run("all_modes_unavailable_on_nonzero_empty_stdout", func(t *testing.T) {
		r := Result{ExitCode: 1, Stdout: ""}
		modes := pythonCapabilityProbe.interpret(r)
		if modes["python"] || modes["pytest"] {
			t.Errorf("want all false on non-zero exit with empty stdout, got %v", modes)
		}
		// Full key set must be present.
		for _, k := range []string{"python", "pytest"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("key %q missing from interpret result", k)
			}
		}
	})

	t.Run("full_key_set_always_returned", func(t *testing.T) {
		// allFalse calls interpret(Result{ExitCode:1}); the returned map must
		// carry both keys so allFalse can enumerate them.
		r := Result{ExitCode: 1, Stdout: ""}
		modes := pythonCapabilityProbe.interpret(r)
		for _, k := range []string{"python", "pytest"} {
			if _, ok := modes[k]; !ok {
				t.Errorf("allFalse requires key %q but it is missing", k)
			}
		}
	})
}

// TestPythonCapabilityProbeSpec verifies the probe uses /bin/sh and is named "python".
func TestPythonCapabilityProbeSpec(t *testing.T) {
	if len(pythonCapabilityProbe.probe) == 0 {
		t.Fatal("pythonCapabilityProbe.probe must be non-empty")
	}
	if pythonCapabilityProbe.probe[0] != "/bin/sh" {
		t.Errorf("probe[0] = %q, want /bin/sh", pythonCapabilityProbe.probe[0])
	}
	if pythonCapabilityProbe.name != "python" {
		t.Errorf("probe name = %q, want python", pythonCapabilityProbe.name)
	}
}

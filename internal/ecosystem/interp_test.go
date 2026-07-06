package ecosystem_test

import (
	"testing"

	"github.com/dpoage/bugbot/internal/ecosystem"
)

// allKnownEcosystems lists every sandbox.Ecosystem constant that should have an
// entry in InterpTable. This list is the sync point — if you add a new
// Ecosystem constant, add it here AND add an InterpRules entry to InterpTable.
var allKnownEcosystems = []string{
	ecosystem.EcosystemGo,
	ecosystem.EcosystemPython,
	ecosystem.EcosystemRust,
	ecosystem.EcosystemJS,
	ecosystem.EcosystemCpp,
	ecosystem.EcosystemBazel,
	ecosystem.EcosystemUnknown,
}

// TestInterpTable_Completeness asserts every known Ecosystem has an entry in
// InterpTable. Adding an Ecosystem without a table entry will fail here.
func TestInterpTable_Completeness(t *testing.T) {
	idx := make(map[string]bool, len(ecosystem.InterpTable))
	for _, e := range ecosystem.InterpTable {
		idx[e.Name] = true
	}
	for _, name := range allKnownEcosystems {
		if !idx[name] {
			t.Errorf("Ecosystem %q has no InterpTable entry; add one to internal/ecosystem/interp.go", name)
		}
	}
}

// TestInterpTable_GoFirst asserts the Go entry is at index 0 (required by
// interpIndex's defensive default).
func TestInterpTable_GoFirst(t *testing.T) {
	if len(ecosystem.InterpTable) == 0 {
		t.Fatal("InterpTable is empty")
	}
	if ecosystem.InterpTable[0].Name != ecosystem.EcosystemGo {
		t.Errorf("InterpTable[0].Name = %q, want %q (Go must be first for legacy-verdict compatibility)",
			ecosystem.InterpTable[0].Name, ecosystem.EcosystemGo)
	}
}

// TestDetectEcosystem_GoModule asserts Go test commands map to EcosystemGo.
func TestDetectEcosystem_GoModule(t *testing.T) {
	cases := [][]string{
		{"go", "test", "./..."},
		{"go", "test", "-race", "./..."},
		{"go", "vet", "./..."},
	}
	for _, argv := range cases {
		e := ecosystem.DetectEcosystem(argv)
		if e.Name != ecosystem.EcosystemGo {
			t.Errorf("DetectEcosystem(%v) = %q, want %q", argv, e.Name, ecosystem.EcosystemGo)
		}
	}
}

// TestDetectEcosystem_Python asserts pytest variants map to EcosystemPython.
func TestDetectEcosystem_Python(t *testing.T) {
	cases := [][]string{
		{"pytest", "tests/"},
		{"py.test", "-v"},
		{"python", "-m", "pytest"},
		{"python3", "-m", "pytest"},
	}
	for _, argv := range cases {
		e := ecosystem.DetectEcosystem(argv)
		if e.Name != ecosystem.EcosystemPython {
			t.Errorf("DetectEcosystem(%v) = %q, want %q", argv, e.Name, ecosystem.EcosystemPython)
		}
	}
}

// TestDetectEcosystem_Rust asserts cargo commands map to EcosystemRust.
func TestDetectEcosystem_Rust(t *testing.T) {
	e := ecosystem.DetectEcosystem([]string{"cargo", "test"})
	if e.Name != ecosystem.EcosystemRust {
		t.Errorf("DetectEcosystem(cargo test) = %q, want rust", e.Name)
	}
}

// TestDetectEcosystem_JS asserts npm/jest/vitest variants map to EcosystemJS.
func TestDetectEcosystem_JS(t *testing.T) {
	cases := [][]string{
		{"npm", "test"},
		{"yarn", "test"},
		{"jest", "--runInBand"},
		{"vitest", "run"},
	}
	for _, argv := range cases {
		e := ecosystem.DetectEcosystem(argv)
		if e.Name != ecosystem.EcosystemJS {
			t.Errorf("DetectEcosystem(%v) = %q, want js", argv, e.Name)
		}
	}
}

// TestDetectEcosystem_Cpp asserts cmake/ctest/compiler variants map to EcosystemCpp.
func TestDetectEcosystem_Cpp(t *testing.T) {
	cases := [][]string{
		{"cmake", "--build", "build"},
		{"ctest", "--test-dir", "build"},
		{"gcc", "-o", "test", "test.c"},
		{"clang++", "-o", "test", "test.cpp"},
	}
	for _, argv := range cases {
		e := ecosystem.DetectEcosystem(argv)
		if e.Name != ecosystem.EcosystemCpp {
			t.Errorf("DetectEcosystem(%v) = %q, want cpp", argv, e.Name)
		}
	}
}

// TestDetectEcosystem_Bazel asserts bazel/bazelisk map to EcosystemBazel.
func TestDetectEcosystem_Bazel(t *testing.T) {
	for _, argv := range [][]string{{"bazel", "test", "//..."}, {"bazelisk", "test", "//..."}} {
		e := ecosystem.DetectEcosystem(argv)
		if e.Name != ecosystem.EcosystemBazel {
			t.Errorf("DetectEcosystem(%v) = %q, want bazel", argv, e.Name)
		}
	}
}

// TestDetectEcosystem_UnwrapShell asserts bash/sh -c and -lc wrappers are
// unwrapped before ecosystem detection.
func TestDetectEcosystem_UnwrapShell(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"bash", "-c", "go test ./..."}, ecosystem.EcosystemGo},
		{[]string{"bash", "-lc", "cmake -B build && ctest"}, ecosystem.EcosystemCpp},
		{[]string{"sh", "-lc", "g++ t.cpp -o t && ./t"}, ecosystem.EcosystemCpp},
		{[]string{"bash", "-c", "cargo test"}, ecosystem.EcosystemRust},
	}
	for _, tc := range cases {
		e := ecosystem.DetectEcosystem(tc.argv)
		if e.Name != tc.want {
			t.Errorf("DetectEcosystem(%v) = %q, want %q", tc.argv, e.Name, tc.want)
		}
	}
}

// TestDetectEcosystem_Unknown asserts unrecognised launchers fall back to
// EcosystemUnknown.
func TestDetectEcosystem_Unknown(t *testing.T) {
	e := ecosystem.DetectEcosystem([]string{"./run_tests"})
	if e.Name != ecosystem.EcosystemUnknown {
		t.Errorf("DetectEcosystem(unknown) = %q, want unknown", e.Name)
	}
}

// TestHasRanEvidence asserts the Go ran-marker matches test failure output.
func TestHasRanEvidence_Go(t *testing.T) {
	var goRules *ecosystem.InterpRules
	for i := range ecosystem.InterpTable {
		if ecosystem.InterpTable[i].Name == ecosystem.EcosystemGo {
			goRules = &ecosystem.InterpTable[i]
			break
		}
	}
	if goRules == nil {
		t.Fatal("no Go entry in InterpTable")
	}
	if !goRules.HasRanEvidence("--- FAIL: TestDivide (0.00s)\nFAIL\t...") {
		t.Error("HasRanEvidence should match '--- FAIL:'")
	}
	if goRules.HasRanEvidence("build failed") {
		t.Error("HasRanEvidence should NOT match build-only output")
	}
}

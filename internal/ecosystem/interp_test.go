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
// TestDetectEcosystem_LauncherNormalization covers bugbot-ds90: launcher
// shapes that used to land EcosystemUnknown because DetectEcosystem only
// looked at a bare argv[0] — bare node, bare python3 (no pytest -m),
// python3 -m unittest, a "timeout <dur>" wrapper, an absolute toolchain
// path, and a bash -c 'export ...; exec ...' compound command.
func TestDetectEcosystem_LauncherNormalization(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"node --test", []string{"node", "--test", "x.test.js"}, ecosystem.EcosystemJS},
		{"node script.js", []string{"node", "script.js"}, ecosystem.EcosystemJS},
		{"nodejs alias", []string{"nodejs", "script.js"}, ecosystem.EcosystemJS},
		{"bare python3 script", []string{"python3", "repro.py"}, ecosystem.EcosystemPython},
		{"python3 -m unittest", []string{"python3", "-m", "unittest", "tests.test_foo"}, ecosystem.EcosystemPython},
		{"python3 -m pytest still works", []string{"python3", "-m", "pytest", "tests/"}, ecosystem.EcosystemPython},
		{"timeout-wrapped python3", []string{"timeout", "60", "python3", "repro.py"}, ecosystem.EcosystemPython},
		{"timeout with duration suffix", []string{"timeout", "30s", "node", "--test", "x.test.js"}, ecosystem.EcosystemJS},
		{"absolute node path", []string{"/opt/bugbot-toolchains/node/bin/node", "--test", "x.test.js"}, ecosystem.EcosystemJS},
		{"absolute python path", []string{"/usr/local/bin/python3", "repro.py"}, ecosystem.EcosystemPython},
		{
			"bash -c export-then-exec compound command",
			[]string{"bash", "-c", "export PYTHONPATH=/repo; exec python3 -m pytest tests/"},
			ecosystem.EcosystemPython,
		},
		{
			"env-var-assignment wrapper",
			[]string{"env", "FOO=bar", "python3", "repro.py"},
			ecosystem.EcosystemPython,
		},
		{
			"nice wrapper",
			[]string{"nice", "-n", "10", "node", "--test", "x.test.js"},
			ecosystem.EcosystemJS,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := ecosystem.DetectEcosystem(tc.argv)
			if e.Name != tc.want {
				t.Errorf("DetectEcosystem(%v) = %q, want %q", tc.argv, e.Name, tc.want)
			}
		})
	}
}

// TestDetectEcosystem_GoRegression asserts the normalization pipeline never
// disturbs an already-correct Go verdict, including a subpackage path that
// happens to contain "python" in it (bugbot-ds90 regression guard: a naive
// "contains python" heuristic would have misclassified this).
func TestDetectEcosystem_GoRegression(t *testing.T) {
	e := ecosystem.DetectEcosystem([]string{"go", "test", "./python/..."})
	if e.Name != ecosystem.EcosystemGo {
		t.Errorf("DetectEcosystem(go test ./python/...) = %q, want %q", e.Name, ecosystem.EcosystemGo)
	}
}

// TestDetectEcosystem_BareUnknownLaunchersStillUnknown asserts launcher
// tokens with no recognized ecosystem still fall back to EcosystemUnknown
// after normalization (basename/benign-wrapper-strip must not accidentally
// invent a match).
func TestDetectEcosystem_BareUnknownLaunchersStillUnknown(t *testing.T) {
	cases := [][]string{
		{"./run_tests"},
		{"ruby", "repro.rb"},
		{"make", "test"},
		{"timeout", "60", "./custom_check"},
		{"/opt/tools/bin/custom-runner", "--flag"},
	}
	for _, argv := range cases {
		e := ecosystem.DetectEcosystem(argv)
		if e.Name != ecosystem.EcosystemUnknown {
			t.Errorf("DetectEcosystem(%v) = %q, want %q", argv, e.Name, ecosystem.EcosystemUnknown)
		}
	}
}

// TestUnwrapShell_NonShellArgvUntouched asserts argv that doesn't match the
// bash/sh -c shell-wrapper shape passes through DetectEcosystem unmodified
// by the shell-unwrap step (still gated by the benign-wrapper/basename
// steps, which apply regardless of a shell wrapper).
func TestUnwrapShell_NonShellArgvUntouched(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"go", "test", "./..."}, ecosystem.EcosystemGo},
		{[]string{"pytest", "tests/"}, ecosystem.EcosystemPython},
		{[]string{"bash"}, ecosystem.EcosystemUnknown},           // too short to be a -c wrapper
		{[]string{"bash", "script.sh"}, ecosystem.EcosystemUnknown}, // no -c flag
	}
	for _, tc := range cases {
		e := ecosystem.DetectEcosystem(tc.argv)
		if e.Name != tc.want {
			t.Errorf("DetectEcosystem(%v) = %q, want %q", tc.argv, e.Name, tc.want)
		}
	}
}

// TestHasRanEvidence_JS_NodeTest covers bugbot-ds90: node:test TAP failures
// ("not ok " at line start) and node's assertion-error text
// ("AssertionError [ERR_ASSERTION]") must both classify as ran-evidence for
// EcosystemJS, so a node --test demonstration reaches the ran-evidence gate
// in interpret() instead of falling through to not_demonstrated.
func TestHasRanEvidence_JS_NodeTest(t *testing.T) {
	var jsRules *ecosystem.InterpRules
	for i := range ecosystem.InterpTable {
		if ecosystem.InterpTable[i].Name == ecosystem.EcosystemJS {
			jsRules = &ecosystem.InterpTable[i]
			break
		}
	}
	if jsRules == nil {
		t.Fatal("no JS entry in InterpTable")
	}
	tapOutput := "TAP version 13\n# Subtest: sync fail\nnot ok 1 - sync fail\n  ---\n  ...\n"
	if !jsRules.HasRanEvidence(tapOutput) {
		t.Error("HasRanEvidence should match line-anchored 'not ok ' from node:test TAP output")
	}
	assertOutput := "AssertionError [ERR_ASSERTION]: Expected values to be strictly equal"
	if !jsRules.HasRanEvidence(assertOutput) {
		t.Error("HasRanEvidence should match node's 'AssertionError [ERR_ASSERTION]' text")
	}
	if jsRules.HasRanEvidence("this line mentions ok, but not ok is not here") {
		// "not ok " (with trailing space) does not appear anywhere here at
		// all, so this must not match regardless of line-anchoring.
		t.Error("HasRanEvidence should NOT match unrelated text without 'not ok ' or an assertion marker")
	}
	// A free ("not ok ") that is NOT at the start of a line must not match —
	// this is exactly the false-positive risk the line-anchoring exists to
	// avoid ("# duration_ms not ok at all" style prose in an unrelated log).
	if jsRules.HasRanEvidence("some prose containing the phrase not ok  mid-sentence") {
		t.Error("HasRanEvidence should NOT match 'not ok ' when it is not at the start of a line")
	}
}

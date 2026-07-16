package ecosystem_test

import (
	"testing"

	"github.com/dpoage/bugbot/internal/ecosystem"
)

// allKnownProbeEcosystems lists the ecosystem names that should appear in
// ProbeEntries. This list is the sync point — if you add a new probe, add its
// name here. Currently: go, cpp, rust, js, python, bazel.
var allKnownProbeEcosystems = []string{"go", "cpp", "rust", "js", "python", "bazel"}

// TestProbeEntries_Completeness asserts every known probe ecosystem has an
// entry in ProbeEntries.
func TestProbeEntries_Completeness(t *testing.T) {
	idx := make(map[string]bool, len(ecosystem.ProbeEntries))
	for _, e := range ecosystem.ProbeEntries {
		idx[e.Name] = true
	}
	for _, name := range allKnownProbeEcosystems {
		if !idx[name] {
			t.Errorf("ProbeEntries missing entry for ecosystem %q; add it to internal/ecosystem/capabilities.go", name)
		}
	}
}

// TestProbeEntries_NonEmpty asserts every ProbeEntry has a non-empty Probe
// argv and a non-nil Interpret func.
func TestProbeEntries_NonEmpty(t *testing.T) {
	for _, e := range ecosystem.ProbeEntries {
		if len(e.Probe) == 0 {
			t.Errorf("ProbeEntry %q: Probe is empty", e.Name)
		}
		if e.Interpret == nil {
			t.Errorf("ProbeEntry %q: Interpret is nil", e.Name)
		}
	}
}

// TestProbeEntries_InterpretFullKeySet asserts every probe's Interpret function
// returns the full key set (all modes, even those that are false) when called
// with a failure result. allFalse in sandbox/capabilities.go relies on this.
func TestProbeEntries_InterpretFullKeySet(t *testing.T) {
	for _, e := range ecosystem.ProbeEntries {
		// A non-zero exit result should produce all-false with full key set.
		modes := e.Interpret(ecosystem.ProbeResult{ExitCode: 1, Stdout: ""})
		if len(modes) == 0 {
			t.Errorf("ProbeEntry %q: Interpret(failure) returned empty map; must return full key set", e.Name)
		}
		// All values must be false for an exit-1 result.
		for k, v := range modes {
			if v {
				t.Errorf("ProbeEntry %q: Interpret(failure) returned %q=true; all modes must be false on failure", e.Name, k)
			}
		}
	}
}

// TestGoProbeInterpret_ViaEcosystem tests the Go probe interpret via the
// exported ProbeEntries to ensure the data matches what sandbox uses.
func TestGoProbeInterpret_ViaEcosystem(t *testing.T) {
	var probe *ecosystem.ProbeEntry
	for i := range ecosystem.ProbeEntries {
		if ecosystem.ProbeEntries[i].Name == "go" {
			probe = &ecosystem.ProbeEntries[i]
			break
		}
	}
	if probe == nil {
		t.Fatal("no 'go' probe in ProbeEntries")
	}
	if probe.Probe[0] != "/bin/sh" {
		t.Errorf("go probe Probe[0] = %q, want /bin/sh", probe.Probe[0])
	}
	modes := probe.Interpret(ecosystem.ProbeResult{ExitCode: 0, Stdout: "go\nrace\n"})
	if !modes["present"] {
		t.Errorf("go probe: Interpret(exit0, \"go\\nrace\") should give present=true, got %v", modes)
	}
	if !modes["race"] {
		t.Errorf("go probe: Interpret(exit0, \"go\\nrace\") should give race=true, got %v", modes)
	}
	// go present but no C compiler / cgo disabled: only "go" token emitted.
	presentOnly := probe.Interpret(ecosystem.ProbeResult{ExitCode: 0, Stdout: "go\n"})
	if !presentOnly["present"] || presentOnly["race"] {
		t.Errorf("go probe: Interpret(exit0, \"go\") = %v, want present=true race=false", presentOnly)
	}
	// go binary missing entirely: no tokens at all (bugbot-bslx negative signal).
	absent := probe.Interpret(ecosystem.ProbeResult{ExitCode: 127, Stdout: ""})
	if absent["present"] || absent["race"] {
		t.Errorf("go probe: Interpret(exit127, \"\") = %v, want present=false race=false", absent)
	}
}

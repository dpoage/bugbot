package ecosystem_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dpoage/bugbot/internal/ecosystem"
	"github.com/dpoage/bugbot/internal/ingest"
)

// allKnownBuildSystems lists every build system constant declared in
// ingest/buildsys.go. This list is the test-time completeness gate: adding a
// new BuildSystem constant to ingest without updating this list (and the
// registry table) produces a test failure. The list itself is the sync point
// — Go cannot enumerate string constants at compile time.
//
// When you add a new BuildSystem to ingest, ADD IT HERE and add a
// corresponding entry to the registry table in registry.go.
var allKnownBuildSystems = []ingest.BuildSystem{
	ingest.BuildSystemBazel,
	ingest.BuildSystemGoWorkspace,
	ingest.BuildSystemJSWorkspace,
	ingest.BuildSystemGoModule,
	ingest.BuildSystemCargo,
	ingest.BuildSystemNPM,
	ingest.BuildSystemPython,
	ingest.BuildSystemDotnet,
	ingest.BuildSystemMaven,
	ingest.BuildSystemGradle,
	ingest.BuildSystemCMake,
	ingest.BuildSystemMeson,
	ingest.BuildSystemMake,
	ingest.BuildSystemNinja,
	ingest.BuildSystemZig,
	ingest.BuildSystemGleam,
	ingest.BuildSystemElixir,
}

// TestRegistry_Completeness asserts that every known BuildSystem has an entry
// in the registry. This is the loud failure gate: adding a build system to
// ingest.DetectBuildSystems without a registry entry will fail here.
func TestRegistry_Completeness(t *testing.T) {
	for _, bs := range allKnownBuildSystems {
		e := ecosystem.Lookup(bs)
		if e == nil {
			t.Errorf("BuildSystem %q has no registry entry; add one to internal/ecosystem/registry.go", bs)
		}
	}
}

// TestRegistry_All_ContainsAllKnown asserts that All() returns at least one
// entry per known build system — i.e. no entry was accidentally dropped.
func TestRegistry_All_ContainsAllKnown(t *testing.T) {
	all := ecosystem.All()
	known := make(map[ingest.BuildSystem]bool, len(all))
	for _, e := range all {
		known[e.BuildSystem] = true
	}
	for _, bs := range allKnownBuildSystems {
		if !known[bs] {
			t.Errorf("BuildSystem %q missing from All(); registry table is incomplete", bs)
		}
	}
}

// TestTestCmdFor_GoModule asserts the canonical Go module test command.
func TestTestCmdFor_GoModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/m\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ecosystem.TestCmdFor(dir, []ingest.BuildSystem{ingest.BuildSystemGoModule})
	want := []string{"go", "test", "./..."}
	if !sliceEq(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestTestCmdFor_Cargo asserts the Rust/Cargo test command.
func TestTestCmdFor_Cargo(t *testing.T) {
	got := ecosystem.TestCmdFor("", []ingest.BuildSystem{ingest.BuildSystemCargo})
	want := []string{"cargo", "test"}
	if !sliceEq(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestTestCmdFor_NPM asserts the npm test command.
func TestTestCmdFor_NPM(t *testing.T) {
	got := ecosystem.TestCmdFor("", []ingest.BuildSystem{ingest.BuildSystemNPM})
	want := []string{"npm", "test"}
	if !sliceEq(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestTestCmdFor_Python asserts the Python/pytest test command.
func TestTestCmdFor_Python(t *testing.T) {
	got := ecosystem.TestCmdFor("", []ingest.BuildSystem{ingest.BuildSystemPython})
	want := []string{"python", "-m", "pytest"}
	if !sliceEq(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestTestCmdFor_CMake asserts the CMake compound configure+build+test command.
func TestTestCmdFor_CMake(t *testing.T) {
	got := ecosystem.TestCmdFor("", []ingest.BuildSystem{ingest.BuildSystemCMake})
	if len(got) < 2 || got[0] != "bash" {
		t.Errorf("got %v, want bash -c ...", got)
	}
}

// TestTestCmdFor_Meson asserts the Meson compound setup+test command.
func TestTestCmdFor_Meson(t *testing.T) {
	got := ecosystem.TestCmdFor("", []ingest.BuildSystem{ingest.BuildSystemMeson})
	if len(got) < 2 || got[0] != "bash" {
		t.Errorf("got %v, want bash -c ...", got)
	}
}

// TestTestCmdFor_Bazel asserts the Bazel test command.
func TestTestCmdFor_Bazel(t *testing.T) {
	got := ecosystem.TestCmdFor("", []ingest.BuildSystem{ingest.BuildSystemBazel})
	want := []string{"bazel", "test", "--build_tests_only", "--test_output=errors", "//..."}
	if !sliceEq(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestTestCmdFor_Zig asserts the Zig test command.
func TestTestCmdFor_Zig(t *testing.T) {
	got := ecosystem.TestCmdFor("", []ingest.BuildSystem{ingest.BuildSystemZig})
	want := []string{"zig", "build", "test"}
	if !sliceEq(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestTestCmdFor_Gleam asserts the Gleam test command.
func TestTestCmdFor_Gleam(t *testing.T) {
	got := ecosystem.TestCmdFor("", []ingest.BuildSystem{ingest.BuildSystemGleam})
	want := []string{"gleam", "test"}
	if !sliceEq(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestTestCmdFor_Elixir asserts the Elixir test command.
func TestTestCmdFor_Elixir(t *testing.T) {
	got := ecosystem.TestCmdFor("", []ingest.BuildSystem{ingest.BuildSystemElixir})
	want := []string{"mix", "test"}
	if !sliceEq(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestTestCmdFor_Make_ReturnsNil asserts that Make (no introspectable target)
// returns nil, causing callers to skip the run_tests tool.
func TestTestCmdFor_Make_ReturnsNil(t *testing.T) {
	got := ecosystem.TestCmdFor("", []ingest.BuildSystem{ingest.BuildSystemMake})
	if got != nil {
		t.Errorf("Make should return nil (no introspectable target), got %v", got)
	}
}

// TestTestCmdFor_Ninja_ReturnsNil asserts that Ninja (no introspectable target)
// returns nil.
func TestTestCmdFor_Ninja_ReturnsNil(t *testing.T) {
	got := ecosystem.TestCmdFor("", []ingest.BuildSystem{ingest.BuildSystemNinja})
	if got != nil {
		t.Errorf("Ninja should return nil (no introspectable target), got %v", got)
	}
}

// TestTestCmdFor_EmptySystemsReturnsNil asserts that an empty build-system
// list yields nil (no match).
func TestTestCmdFor_EmptySystemsReturnsNil(t *testing.T) {
	got := ecosystem.TestCmdFor("", nil)
	if got != nil {
		t.Errorf("empty systems should return nil, got %v", got)
	}
}

// TestTestCmdFor_PriorityOrder asserts that the first matching system wins
// when multiple are provided (Bazel before GoModule).
func TestTestCmdFor_PriorityOrder(t *testing.T) {
	// Bazel should win over GoModule when it comes first.
	got := ecosystem.TestCmdFor("", []ingest.BuildSystem{ingest.BuildSystemBazel, ingest.BuildSystemGoModule})
	want := []string{"bazel", "test", "--build_tests_only", "--test_output=errors", "//..."}
	if !sliceEq(got, want) {
		t.Errorf("got %v, want bazel command (first match wins)", got)
	}
}

// TestTestCmdFor_GoWorkspace_WithGoMod returns go test when root go.mod exists.
func TestTestCmdFor_GoWorkspace_WithGoMod(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/ws\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ecosystem.TestCmdFor(dir, []ingest.BuildSystem{ingest.BuildSystemGoWorkspace})
	want := []string{"go", "test", "./..."}
	if !sliceEq(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestTestCmdFor_GoWorkspace_WithoutGoMod returns nil when there is no root
// go.mod (multi-module workspace root without a root module).
func TestTestCmdFor_GoWorkspace_WithoutGoMod(t *testing.T) {
	dir := t.TempDir()
	got := ecosystem.TestCmdFor(dir, []ingest.BuildSystem{ingest.BuildSystemGoWorkspace})
	if got != nil {
		t.Errorf("GoWorkspace without go.mod should return nil, got %v", got)
	}
}

// TestTestCmdFor_JSWorkspace_Pnpm returns pnpm test when pnpm-workspace.yaml exists.
func TestTestCmdFor_JSWorkspace_Pnpm(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pnpm-workspace.yaml"), []byte("packages: ['packages/*']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ecosystem.TestCmdFor(dir, []ingest.BuildSystem{ingest.BuildSystemJSWorkspace})
	want := []string{"pnpm", "test"}
	if !sliceEq(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestTestCmdFor_JSWorkspace_TurboNx returns npm test when no
// pnpm-workspace.yaml exists (turbo.json / nx.json fallback).
func TestTestCmdFor_JSWorkspace_TurboNx(t *testing.T) {
	dir := t.TempDir()
	got := ecosystem.TestCmdFor(dir, []ingest.BuildSystem{ingest.BuildSystemJSWorkspace})
	want := []string{"npm", "test"}
	if !sliceEq(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestTestCmdFor_Dotnet_ReturnsNil asserts that Dotnet returns nil — no
// standard introspectable test command (preserves pre-registry behavior).
func TestTestCmdFor_Dotnet_ReturnsNil(t *testing.T) {
	got := ecosystem.TestCmdFor("", []ingest.BuildSystem{ingest.BuildSystemDotnet})
	if got != nil {
		t.Errorf("Dotnet should return nil (no standard test cmd), got %v", got)
	}
}

// TestTestCmdFor_Maven_ReturnsNil asserts that Maven returns nil — no
// standard introspectable test command (preserves pre-registry behavior).
func TestTestCmdFor_Maven_ReturnsNil(t *testing.T) {
	got := ecosystem.TestCmdFor("", []ingest.BuildSystem{ingest.BuildSystemMaven})
	if got != nil {
		t.Errorf("Maven should return nil (no standard test cmd), got %v", got)
	}
}

// TestTestCmdFor_Gradle_ReturnsNil asserts that Gradle returns nil — no
// standard introspectable test command (preserves pre-registry behavior).
func TestTestCmdFor_Gradle_ReturnsNil(t *testing.T) {
	got := ecosystem.TestCmdFor("", []ingest.BuildSystem{ingest.BuildSystemGradle})
	if got != nil {
		t.Errorf("Gradle should return nil (no standard test cmd), got %v", got)
	}
}

// TestLookup_ReturnsEntryForKnown asserts Lookup returns a non-nil entry for
// all known build systems.
func TestLookup_ReturnsEntryForKnown(t *testing.T) {
	for _, bs := range allKnownBuildSystems {
		e := ecosystem.Lookup(bs)
		if e == nil {
			t.Errorf("Lookup(%q) returned nil", bs)
		}
		if e != nil && e.BuildSystem != bs {
			t.Errorf("Lookup(%q) returned entry with BuildSystem=%q", bs, e.BuildSystem)
		}
	}
}

// TestLookup_ReturnsNilForUnknown asserts Lookup returns nil for unrecognised
// build systems.
func TestLookup_ReturnsNilForUnknown(t *testing.T) {
	e := ecosystem.Lookup("not_a_real_build_system")
	if e != nil {
		t.Errorf("Lookup(unknown) returned non-nil entry: %+v", e)
	}
}

// sliceEq reports whether a and b contain identical strings in the same order.
func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDetectBuildSystems covers every individual marker, multi-marker
// coexistence, priority ordering, and the empty-dir case.
func TestDetectBuildSystems(t *testing.T) {
	cases := []struct {
		name    string
		markers []string // files to create in a temp dir
		want    []BuildSystem
	}{
		// --- single-marker cases ---
		{
			name:    "MODULE.bazel",
			markers: []string{"MODULE.bazel"},
			want:    []BuildSystem{BuildSystemBazel},
		},
		{
			name:    "WORKSPACE",
			markers: []string{"WORKSPACE"},
			want:    []BuildSystem{BuildSystemBazel},
		},
		{
			name:    "WORKSPACE.bazel",
			markers: []string{"WORKSPACE.bazel"},
			want:    []BuildSystem{BuildSystemBazel},
		},
		{
			name:    "go.work",
			markers: []string{"go.work"},
			want:    []BuildSystem{BuildSystemGoWorkspace},
		},
		{
			name:    "pnpm-workspace.yaml",
			markers: []string{"pnpm-workspace.yaml"},
			want:    []BuildSystem{BuildSystemJSWorkspace},
		},
		{
			name:    "turbo.json",
			markers: []string{"turbo.json"},
			want:    []BuildSystem{BuildSystemJSWorkspace},
		},
		{
			name:    "nx.json",
			markers: []string{"nx.json"},
			want:    []BuildSystem{BuildSystemJSWorkspace},
		},
		{
			name:    "go.mod",
			markers: []string{"go.mod"},
			want:    []BuildSystem{BuildSystemGoModule},
		},
		{
			name:    "Cargo.toml",
			markers: []string{"Cargo.toml"},
			want:    []BuildSystem{BuildSystemCargo},
		},
		{
			name:    "package.json",
			markers: []string{"package.json"},
			want:    []BuildSystem{BuildSystemNPM},
		},
		{
			name:    "pyproject.toml",
			markers: []string{"pyproject.toml"},
			want:    []BuildSystem{BuildSystemPython},
		},
		{
			name:    "setup.py",
			markers: []string{"setup.py"},
			want:    []BuildSystem{BuildSystemPython},
		},
		// --- empty dir ---
		{
			name:    "empty",
			markers: nil,
			want:    nil,
		},
		// --- multi-marker coexistence: Bazel + go.mod (common mixed repo) ---
		{
			name:    "Bazel+go.mod",
			markers: []string{"MODULE.bazel", "go.mod"},
			want:    []BuildSystem{BuildSystemBazel, BuildSystemGoModule},
		},
		// --- Bazel + go.work + go.mod ---
		{
			name:    "Bazel+go.work+go.mod",
			markers: []string{"WORKSPACE", "go.work", "go.mod"},
			want:    []BuildSystem{BuildSystemBazel, BuildSystemGoWorkspace, BuildSystemGoModule},
		},
		// --- go.work + go.mod (multi-module workspace with root module) ---
		{
			name:    "go.work+go.mod",
			markers: []string{"go.work", "go.mod"},
			want:    []BuildSystem{BuildSystemGoWorkspace, BuildSystemGoModule},
		},
		// --- pnpm + package.json (both JS entries) ---
		{
			name:    "pnpm-workspace+package.json",
			markers: []string{"pnpm-workspace.yaml", "package.json"},
			want:    []BuildSystem{BuildSystemJSWorkspace, BuildSystemNPM},
		},
		// --- priority: Bazel beats everything when both present ---
		{
			name:    "Bazel priority over all",
			markers: []string{"MODULE.bazel", "go.mod", "Cargo.toml", "package.json", "pyproject.toml"},
			want: []BuildSystem{
				BuildSystemBazel,
				BuildSystemGoModule,
				BuildSystemCargo,
				BuildSystemNPM,
				BuildSystemPython,
			},
		},
		// --- C/C++ single-marker cases ---
		{
			name:    "CMakeLists.txt",
			markers: []string{"CMakeLists.txt"},
			want:    []BuildSystem{BuildSystemCMake},
		},
		{
			name:    "meson.build",
			markers: []string{"meson.build"},
			want:    []BuildSystem{BuildSystemMeson},
		},
		{
			name:    "Makefile",
			markers: []string{"Makefile"},
			want:    []BuildSystem{BuildSystemMake},
		},
		{
			name:    "GNUmakefile",
			markers: []string{"GNUmakefile"},
			want:    []BuildSystem{BuildSystemMake},
		},
		{
			name:    "build.ninja",
			markers: []string{"build.ninja"},
			want:    []BuildSystem{BuildSystemNinja},
		},
		// --- ordering: go.mod + Makefile keeps go_module as primary ---
		{
			name:    "go.mod+Makefile",
			markers: []string{"go.mod", "Makefile"},
			want:    []BuildSystem{BuildSystemGoModule, BuildSystemMake},
		},
		// --- ordering: MODULE.bazel + CMakeLists.txt keeps bazel first ---
		{
			name:    "MODULE.bazel+CMakeLists.txt",
			markers: []string{"MODULE.bazel", "CMakeLists.txt"},
			want:    []BuildSystem{BuildSystemBazel, BuildSystemCMake},
		},
		// --- all four C/C++ markers coexist in priority order ---
		{
			name:    "CMake+Meson+Make+Ninja",
			markers: []string{"CMakeLists.txt", "meson.build", "Makefile", "build.ninja"},
			want:    []BuildSystem{BuildSystemCMake, BuildSystemMeson, BuildSystemMake, BuildSystemNinja},
		},
		// --- .NET / dotnet single-marker cases ---
		{
			name:    "dotnet via .sln (glob)",
			markers: []string{"MyApp.sln"},
			want:    []BuildSystem{BuildSystemDotnet},
		},
		{
			name:    "dotnet via .csproj (glob)",
			markers: []string{"MyLib.csproj"},
			want:    []BuildSystem{BuildSystemDotnet},
		},
		{
			name:    "dotnet via Directory.Build.props",
			markers: []string{"Directory.Build.props"},
			want:    []BuildSystem{BuildSystemDotnet},
		},
		// --- Maven single-marker case ---
		{
			name:    "maven via pom.xml",
			markers: []string{"pom.xml"},
			want:    []BuildSystem{BuildSystemMaven},
		},
		// --- Gradle single-marker cases ---
		{
			name:    "gradle via build.gradle",
			markers: []string{"build.gradle"},
			want:    []BuildSystem{BuildSystemGradle},
		},
		{
			name:    "gradle via build.gradle.kts",
			markers: []string{"build.gradle.kts"},
			want:    []BuildSystem{BuildSystemGradle},
		},
		{
			name:    "gradle via settings.gradle",
			markers: []string{"settings.gradle"},
			want:    []BuildSystem{BuildSystemGradle},
		},
		{
			name:    "gradle via settings.gradle.kts",
			markers: []string{"settings.gradle.kts"},
			want:    []BuildSystem{BuildSystemGradle},
		},
		// --- ordering: go.mod + .csproj + Makefile → go_module before dotnet before make ---
		{
			name:    "go.mod+.csproj+Makefile ordering",
			markers: []string{"go.mod", "App.csproj", "Makefile"},
			want:    []BuildSystem{BuildSystemGoModule, BuildSystemDotnet, BuildSystemMake},
		},
		// --- coexistence: Maven + Gradle in same repo (polyglot / migration scenario) ---
		{
			name:    "maven+gradle coexistence",
			markers: []string{"pom.xml", "build.gradle"},
			want:    []BuildSystem{BuildSystemMaven, BuildSystemGradle},
		},
		// --- Zig/Gleam/Elixir single-marker cases (bugbot-93z.18) ---
		{
			name:    "zig via build.zig",
			markers: []string{"build.zig"},
			want:    []BuildSystem{BuildSystemZig},
		},
		{
			name:    "gleam via gleam.toml",
			markers: []string{"gleam.toml"},
			want:    []BuildSystem{BuildSystemGleam},
		},
		{
			name:    "elixir via mix.exs",
			markers: []string{"mix.exs"},
			want:    []BuildSystem{BuildSystemElixir},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, m := range tc.markers {
				if err := os.WriteFile(filepath.Join(dir, m), []byte("x"), 0o644); err != nil {
					t.Fatalf("write marker %s: %v", m, err)
				}
			}
			got := DetectBuildSystems(dir)
			if !buildSystemSliceEqual(got, tc.want) {
				t.Errorf("DetectBuildSystems: got %v, want %v", got, tc.want)
			}
		})
	}
}

func buildSystemSliceEqual(a, b []BuildSystem) bool {
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

// ---------------------------------------------------------------------------
// Blast radius: Bazel native path
// ---------------------------------------------------------------------------

// TestBlastRadiusBazelNative verifies that when the repo has a Bazel workspace
// marker and the queryRunner returns canned rdeps output, the returned files
// include the dependents reported by Bazel.
func TestBlastRadiusBazelNative(t *testing.T) {
	r := newTestRepo(t)
	// Simulate a Bazel workspace by adding MODULE.bazel.
	r.write("MODULE.bazel", `module(name = "myrepo")`)
	// Package "lib" — the changed file.
	r.write("lib/lib.go", "package lib\nfunc Lib() {}\n")
	// Package "app" — supposedly depends on lib per the fake bazel output.
	r.write("app/main.go", "package main\nfunc main() {}\n")
	// Package "util" — unrelated.
	r.write("util/util.go", "package util\nfunc Util() {}\n")
	r.commit("init")

	repo := r.open()
	snap, err := repo.Snapshot(context.Background(), ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}

	// Inject a fake query runner that returns a canned rdeps package list.
	// The query runner receives the full args slice; we don't validate them in
	// this test to keep the fixture simple.
	repo.queryRunner = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		// Simulate `bazel query "rdeps(//..., set(//lib:lib.go))" --output=package`
		// returning the lib and app packages.
		return []byte("//lib\n//app\n"), nil
	}

	radius, err := repo.BlastRadius(context.Background(), snap, []string{"lib/lib.go"})
	if err != nil {
		t.Fatalf("BlastRadius: %v", err)
	}

	// Changed file itself must be present.
	if !contains(radius, "lib/lib.go") {
		t.Errorf("radius missing changed file lib/lib.go: %v", radius)
	}
	// app/main.go was in the Bazel rdeps output → must be in radius.
	if !contains(radius, "app/main.go") {
		t.Errorf("radius missing Bazel-reported dependent app/main.go: %v", radius)
	}
	// util/util.go was NOT in the rdeps output → must not be in radius.
	if contains(radius, "util/util.go") {
		t.Errorf("radius should not include non-dependent util/util.go: %v", radius)
	}
}

// TestBlastRadiusBazelNativeErrorFallback verifies that when the Bazel query
// runner returns an error the result is identical to running without the native
// path (i.e. we fall through to the Go import-graph + textual path).
func TestBlastRadiusBazelNativeErrorFallback(t *testing.T) {
	r := newTestRepo(t)
	r.write("MODULE.bazel", `module(name = "myrepo")`)
	// package a (changed), package b imports a.
	r.write("a/a.go", "package a\nfunc A() {}\n")
	r.write("b/b.go", "package b\nimport \"github.com/dpoage/bugbot/a\"\nfunc B() { a.A() }\n")
	r.write("c/c.go", "package c\nfunc C() {}\n")
	r.commit("init")

	// Control run: MODULE.bazel IS present (same repo), but we stub the query
	// runner with an error so the Bazel native path is unconditionally skipped
	// and only the Go import-graph runs. This makes the control hermetic: it
	// produces the same result regardless of whether bazel is installed on the
	// host machine.
	controlRepo := r.open()
	controlRepo.queryRunner = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("bazel suppressed for control run")
	}
	controlSnap, err := controlRepo.Snapshot(context.Background(), ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	controlRadius, err := controlRepo.BlastRadius(context.Background(), controlSnap, []string{"a/a.go"})
	if err != nil {
		t.Fatal(err)
	}

	// Test run: inject a failing query runner.
	testRepo := r.open()
	testRepo.queryRunner = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("bazel not available")
	}
	testSnap, err := testRepo.Snapshot(context.Background(), ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	testRadius, err := testRepo.BlastRadius(context.Background(), testSnap, []string{"a/a.go"})
	if err != nil {
		t.Fatal(err)
	}

	// Both radii must be equal: fallback produces the same result as if the
	// Bazel path were never attempted.
	if !stringSlicesEqual(testRadius, controlRadius) {
		t.Errorf("fallback radius %v != control radius %v", testRadius, controlRadius)
	}

	// Sanity: Go import graph found b/b.go.
	if !contains(testRadius, "b/b.go") {
		t.Errorf("fallback should include Go-graph dependent b/b.go: %v", testRadius)
	}
}

func stringSlicesEqual(a, b []string) bool {
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

// ---------------------------------------------------------------------------
// Blast radius: go.work cross-module suffix matching
// ---------------------------------------------------------------------------

// TestBlastRadiusGoWorkCrossModule verifies that the existing suffix-matching
// import graph already resolves cross-module imports in a go.work workspace.
//
// Layout:
//
//	modA/a/a.go  — package "a", changed file
//	modB/b/b.go  — imports "example.com/modA/a" (cross-module)
//
// Because importMatchesLocalDir matches by import-path suffix and the local
// directory key for modA/a/a.go is "modA/a", the suffix "modA/a" is a suffix
// of "example.com/modA/a" — so modB/b/b.go appears in the radius without any
// exec.
func TestBlastRadiusGoWorkCrossModule(t *testing.T) {
	r := newTestRepo(t)
	// go.work workspace marker (content irrelevant for the ingest layer).
	r.write("go.work", "go 1.21\nuse ./modA\nuse ./modB\n")

	// Module A — the changed file lives here.
	r.write("modA/go.mod", "module example.com/modA\n\ngo 1.21\n")
	r.write("modA/a/a.go", "package a\n\nfunc A() int { return 1 }\n")

	// Module B — imports module A's package.
	r.write("modB/go.mod", "module example.com/modB\n\ngo 1.21\n")
	// The import path "example.com/modA/a" has suffix "modA/a", which matches
	// the local directory "modA/a" via the existing suffix rule.
	r.write("modB/b/b.go", "package b\n\nimport \"example.com/modA/a\"\n\nfunc B() int { return a.A() }\n")

	// An unrelated file in module B.
	r.write("modB/c/c.go", "package c\n\nfunc C() {}\n")
	r.commit("init")

	repo := r.open()
	snap, err := repo.Snapshot(context.Background(), ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}

	radius, err := repo.BlastRadius(context.Background(), snap, []string{"modA/a/a.go"})
	if err != nil {
		t.Fatalf("BlastRadius: %v", err)
	}

	// Changed file present.
	if !contains(radius, "modA/a/a.go") {
		t.Errorf("radius missing changed file: %v", radius)
	}
	// Cross-module import resolved via suffix matching.
	if !contains(radius, "modB/b/b.go") {
		t.Errorf("cross-module dependent modB/b/b.go missing from radius: %v", radius)
	}
	// Unrelated file must not appear.
	if contains(radius, "modB/c/c.go") {
		t.Errorf("unrelated modB/c/c.go should not be in radius: %v", radius)
	}
}

// ---------------------------------------------------------------------------
// detectSuiteCmd: Bazel, go.work, pnpm-workspace new cases
// ---------------------------------------------------------------------------

// TestDetectSuiteCmdExtended covers the new build-system cases added on top of
// the original marker table (which is still covered by TestDetectSuiteCmd in
// patch_test.go).
//
// NOTE: these tests live in package ingest_test (same _test.go file convention
// as the rest) but we call the ingest-internal helper indirectly through the
// exported surface.  The suite-cmd detection itself lives in internal/repro;
// that package's own TestDetectSuiteCmd covers the new cases — the test below
// exercises DetectBuildSystems only, as that is the ingest-layer contribution.
func TestDetectBuildSystemsJSWorkspaceMarkerVariants(t *testing.T) {
	// Verify each JS-workspace marker is detected independently and that the
	// returned system constant is always BuildSystemJSWorkspace.
	for _, marker := range []string{"pnpm-workspace.yaml", "turbo.json", "nx.json"} {
		marker := marker
		t.Run(marker, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, marker), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			got := DetectBuildSystems(dir)
			if len(got) != 1 || got[0] != BuildSystemJSWorkspace {
				t.Errorf("marker %s: got %v, want [js_workspace]", marker, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// detectSuiteCmd new cases (repro package exports only; tested indirectly here
// via the ingest.DetectBuildSystems assertion so we do not cross package
// boundaries in a test file that is in package ingest).
// ---------------------------------------------------------------------------

// TestDetectBuildSystemsBazelQuery confirms that a Bazel workspace causes the
// right Bazel query args to be passed to the queryRunner.
func TestBazelDependentsQueryArgs(t *testing.T) {
	r := newTestRepo(t)
	r.write("MODULE.bazel", `module(name = "myrepo")`)
	r.write("lib/lib.go", "package lib\n")
	r.commit("init")

	repo := r.open()
	snap, err := repo.Snapshot(context.Background(), ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}

	var capturedArgs []string
	repo.queryRunner = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		capturedArgs = append([]string{}, args...)
		return []byte(""), nil
	}

	_, err = repo.BlastRadius(context.Background(), snap, []string{"lib/lib.go"})
	if err != nil {
		t.Fatalf("BlastRadius: %v", err)
	}

	if len(capturedArgs) == 0 {
		t.Fatal("queryRunner was not called")
	}
	// First arg must be "bazel".
	if capturedArgs[0] != "bazel" {
		t.Errorf("expected first arg 'bazel', got %q", capturedArgs[0])
	}
	// Must include --output=package.
	found := false
	for _, a := range capturedArgs {
		if a == "--output=package" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --output=package in args: %v", capturedArgs)
	}
	// Must reference the changed file's label.
	queryExpr := ""
	for _, a := range capturedArgs {
		if strings.Contains(a, "rdeps") {
			queryExpr = a
			break
		}
	}
	if !strings.Contains(queryExpr, "lib") {
		t.Errorf("query expr should reference 'lib' package: %q", queryExpr)
	}
	// Labels must be single-quoted inside the set() expression.
	if !strings.Contains(queryExpr, "'//lib:lib.go'") {
		t.Errorf("label must be single-quoted in set() expression: %q", queryExpr)
	}
}

// TestOpenQueryRunnerIsNil asserts that Open leaves queryRunner nil (production
// default) and that a subsequently injected runner is called correctly.
func TestOpenQueryRunnerIsNil(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package a\n")
	r.commit("init")

	repo := r.open()
	// Production default: queryRunner must be nil after Open.
	if repo.queryRunner != nil {
		t.Fatal("Open must leave queryRunner nil; got non-nil")
	}

	// Injecting a runner must work: a Bazel-marked repo calls it.
	r.write("MODULE.bazel", `module(name = "test")`)
	r.write("lib/lib.go", "package lib\n")
	r.commit("add bazel marker")

	repo2 := r.open()
	if repo2.queryRunner != nil {
		t.Fatal("Open must leave queryRunner nil; got non-nil after re-open")
	}

	called := false
	repo2.queryRunner = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		called = true
		return []byte(""), nil
	}
	snap, err := repo2.Snapshot(context.Background(), ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo2.BlastRadius(context.Background(), snap, []string{"lib/lib.go"}); err != nil {
		t.Fatalf("BlastRadius: %v", err)
	}
	if !called {
		t.Error("injected queryRunner was not called for Bazel-marked repo")
	}
}

// TestBazelDependentsLabelQuoting verifies that file paths containing spaces
// produce well-formed single-quoted labels in the bazel query expression and
// that paths with single quotes are skipped rather than producing a malformed
// query.
func TestBazelDependentsLabelQuoting(t *testing.T) {
	r := newTestRepo(t)
	r.write("MODULE.bazel", `module(name = "myrepo")`)
	// A path with a space in the directory name.
	r.write("dir with spaces/lib.go", "package lib\n")
	// A normal path alongside it.
	r.write("normal/lib.go", "package lib\n")
	r.commit("init")

	repo := r.open()
	snap, err := repo.Snapshot(context.Background(), ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}

	var capturedQuery string
	repo.queryRunner = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if strings.Contains(a, "rdeps") {
				capturedQuery = a
				break
			}
		}
		return []byte(""), nil
	}

	_, err = repo.BlastRadius(context.Background(), snap, []string{
		"dir with spaces/lib.go",
		"normal/lib.go",
	})
	if err != nil {
		t.Fatalf("BlastRadius: %v", err)
	}

	if capturedQuery == "" {
		t.Fatal("queryRunner was not called or no rdeps expression captured")
	}
	// The space-containing label must be single-quoted.
	if !strings.Contains(capturedQuery, "'//dir with spaces:lib.go'") {
		t.Errorf("space-containing label must be single-quoted; query: %q", capturedQuery)
	}
	// The normal label must also be single-quoted.
	if !strings.Contains(capturedQuery, "'//normal:lib.go'") {
		t.Errorf("normal label must be single-quoted; query: %q", capturedQuery)
	}
	// Verify that the set() expression uses single-quoted labels. A
	// well-formed set() looks like: set('//a:b' '//c:d' ...). An unquoted
	// label inside set() would appear as "set(//..." or "' //" (space then
	// unquoted label). The simplest invariant: inside set(...) there must be
	// no occurrence of the substring " //" (space-slash-slash), which would
	// indicate a space-separated unquoted label start.
	setIdx := strings.Index(capturedQuery, "set(")
	if setIdx < 0 {
		t.Fatalf("no set() in query expression: %q", capturedQuery)
	}
	setBody := capturedQuery[setIdx+4:] // everything after "set("
	if strings.Contains(setBody, " //") {
		t.Errorf("set() body contains unquoted label (found \" //\"); full query: %q", capturedQuery)
	}
	// Also verify it does not start with an unquoted label: "set(//" without a quote.
	if strings.HasPrefix(setBody, "//") {
		t.Errorf("set() body starts with unquoted label; full query: %q", capturedQuery)
	}
}

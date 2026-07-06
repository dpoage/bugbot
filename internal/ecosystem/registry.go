package ecosystem

// registry.go is the single source of truth for per-ecosystem build/test
// behavior in bugbot. Every package that needs to know "what command runs the
// tests for a Go repo?" reads this file instead of maintaining its own copy.
//
// # Design
//
// The registry is keyed by ingest.BuildSystem (the filesystem-detected
// toolchain marker). One Entry per build system defines the test argv for that
// ecosystem. The two historic copies of this mapping — funnel.detectTestCmd and
// repro.detectSuiteCmdFor — were independently maintained with a comment
// "must stay in sync"; this package eliminates that obligation.
//
// # Adding a new ecosystem
//
// 1. Add a BuildSystem constant in internal/ingest/buildsys.go.
// 2. Add an Entry in the table below (testCmd MUST be non-nil unless the
//    toolchain has no introspectable test target).
// 3. Add a detect marker in DetectBuildSystems.
// 4. Run `go test ./internal/ecosystem/...` — the completeness check will
//    catch a missing entry.

import (
	"os"
	"path/filepath"

	"github.com/dpoage/bugbot/internal/ingest"
)

// Entry holds the per-ecosystem build/test metadata for one build system.
type Entry struct {
	// BuildSystem is the ingest.BuildSystem this entry covers. It is the
	// primary key of the registry table.
	BuildSystem ingest.BuildSystem

	// testCmd returns the full-suite test argv for repoDir, or nil when the
	// toolchain has no standard introspectable test target. The argv is
	// suitable for direct execution inside a sandbox (no shell wrapping needed
	// unless the command itself requires it, e.g. CMake's compound form).
	//
	// repoDir is passed so entries that need to inspect the repo root (e.g.
	// GoWorkspace checks for a root go.mod; JSWorkspace checks for
	// pnpm-workspace.yaml) can do so without scanning the filesystem twice.
	// Entries that don't need the path may ignore it.
	testCmd func(repoDir string) []string
}

// TestCmd returns the full-suite test argv for this entry, or nil when no
// standard test target exists. repoDir is the absolute path to the repository
// root; some entries stat files there to pick the right sub-command.
func (e Entry) TestCmd(repoDir string) []string {
	if e.testCmd == nil {
		return nil
	}
	return e.testCmd(repoDir)
}

// table is the ordered registry of per-ecosystem entries. Order mirrors
// ingest.DetectBuildSystems priority — the first matching entry wins in
// TestCmdFor, which matters for the rare case where a repo has multiple
// detected build systems and the caller wants the primary one's test command.
//
// Note: Make and Ninja are included with a nil testCmd because their targets
// are not introspectable; they are present so the completeness check in
// registry_test.go can enumerate all known build systems.
var table = []Entry{
	{
		BuildSystem: ingest.BuildSystemBazel,
		testCmd: func(_ string) []string {
			return []string{"bazel", "test", "--build_tests_only", "--test_output=errors", "//..."}
		},
	},
	{
		BuildSystem: ingest.BuildSystemGoWorkspace,
		// A go.work-only repo spans multiple modules; `go test ./...` at the
		// workspace root only works when there is also a root go.mod (i.e.
		// there is a package in the root module). Without a root go.mod the
		// correct approach is per-module invocations, which is out of scope —
		// return nil and let a lower-priority build system match.
		testCmd: func(repoDir string) []string {
			if _, err := os.Stat(filepath.Join(repoDir, "go.mod")); err == nil {
				return []string{"go", "test", "./..."}
			}
			return nil
		},
	},
	{
		BuildSystem: ingest.BuildSystemJSWorkspace,
		// pnpm workspaces have a canonical `pnpm test` command; turbo.json /
		// nx.json repos fall back to `npm test` as the closest portable default.
		testCmd: func(repoDir string) []string {
			if _, err := os.Stat(filepath.Join(repoDir, "pnpm-workspace.yaml")); err == nil {
				return []string{"pnpm", "test"}
			}
			return []string{"npm", "test"}
		},
	},
	{
		BuildSystem: ingest.BuildSystemGoModule,
		testCmd:     func(_ string) []string { return []string{"go", "test", "./..."} },
	},
	{
		BuildSystem: ingest.BuildSystemCargo,
		testCmd:     func(_ string) []string { return []string{"cargo", "test"} },
	},
	{
		BuildSystem: ingest.BuildSystemNPM,
		testCmd:     func(_ string) []string { return []string{"npm", "test"} },
	},
	{
		BuildSystem: ingest.BuildSystemPython,
		testCmd:     func(_ string) []string { return []string{"python", "-m", "pytest"} },
	},
	{
		// Dotnet: no standard test command supported yet — `dotnet test` requires
		// project-specific discovery that the sandbox doesn't perform. Main returned
		// nil for Dotnet/Maven/Gradle; preserving that to avoid silently enabling
		// run_tests tooling, patch-prover suite runs, and smoke probes for these repos.
		BuildSystem: ingest.BuildSystemDotnet,
		testCmd:     nil,
	},
	{
		// Maven: no standard introspectable test command; nil preserves main behavior.
		BuildSystem: ingest.BuildSystemMaven,
		testCmd:     nil,
	},
	{
		// Gradle: no standard introspectable test command; nil preserves main behavior.
		BuildSystem: ingest.BuildSystemGradle,
		testCmd:     nil,
	},
	{
		BuildSystem: ingest.BuildSystemCMake,
		testCmd: func(_ string) []string {
			return []string{"bash", "-c", "cmake -S . -B build -DCMAKE_BUILD_TYPE=Debug && cmake --build build --parallel 4 && ctest --test-dir build --output-on-failure --no-tests=ignore"}
		},
	},
	{
		BuildSystem: ingest.BuildSystemMeson,
		testCmd: func(_ string) []string {
			return []string{"bash", "-c", "meson setup build && meson test -C build --print-errorlogs"}
		},
	},
	{
		// Make targets are not introspectable; no standard test command.
		BuildSystem: ingest.BuildSystemMake,
		testCmd:     nil,
	},
	{
		// Ninja targets are not introspectable; no standard test command.
		BuildSystem: ingest.BuildSystemNinja,
		testCmd:     nil,
	},
	{
		BuildSystem: ingest.BuildSystemZig,
		testCmd:     func(_ string) []string { return []string{"zig", "build", "test"} },
	},
	{
		BuildSystem: ingest.BuildSystemGleam,
		testCmd:     func(_ string) []string { return []string{"gleam", "test"} },
	},
	{
		BuildSystem: ingest.BuildSystemElixir,
		testCmd:     func(_ string) []string { return []string{"mix", "test"} },
	},
}

// byBuildSystem is a lookup map built once from table for O(1) lookup.
var byBuildSystem map[ingest.BuildSystem]*Entry

func init() {
	byBuildSystem = make(map[ingest.BuildSystem]*Entry, len(table))
	for i := range table {
		byBuildSystem[table[i].BuildSystem] = &table[i]
	}
}

// Lookup returns the Entry for bs, or nil when bs is not in the registry.
func Lookup(bs ingest.BuildSystem) *Entry {
	return byBuildSystem[bs]
}

// All returns the full ordered registry table. The slice is a copy; callers
// must not modify it.
func All() []Entry {
	out := make([]Entry, len(table))
	copy(out, table)
	return out
}

// TestCmdFor returns the full-suite test argv for the first build system in
// systems that has a registered test command, or nil when no match is found.
// repoDir is passed through to entries that need to inspect the repo root.
//
// This is the shared implementation consumed by funnel.detectTestCmd and
// repro.detectSuiteCmdFor; both callers now defer here so the mapping is
// defined exactly once.
func TestCmdFor(repoDir string, systems []ingest.BuildSystem) []string {
	for _, sys := range systems {
		e := byBuildSystem[sys]
		if e == nil {
			continue
		}
		cmd := e.TestCmd(repoDir)
		if cmd != nil {
			return cmd
		}
	}
	return nil
}

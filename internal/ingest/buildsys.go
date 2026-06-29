package ingest

import (
	"os"
	"path/filepath"
)

// BuildSystem identifies a build/test toolchain detected in a repository.
// Multiple build systems may coexist in a single repo (e.g. Bazel wrapping a
// Go module).
type BuildSystem string

const (
	// BuildSystemBazel is detected from MODULE.bazel, WORKSPACE, or WORKSPACE.bazel.
	BuildSystemBazel BuildSystem = "bazel"
	// BuildSystemGoWorkspace is detected from a go.work file at the repo root.
	BuildSystemGoWorkspace BuildSystem = "go_workspace"
	// BuildSystemJSWorkspace is detected from pnpm-workspace.yaml, turbo.json, or nx.json.
	BuildSystemJSWorkspace BuildSystem = "js_workspace"
	// BuildSystemGoModule is detected from a root-level go.mod.
	BuildSystemGoModule BuildSystem = "go_module"
	// BuildSystemCargo is detected from a root-level Cargo.toml.
	BuildSystemCargo BuildSystem = "cargo"
	// BuildSystemNPM is detected from a root-level package.json (no workspace markers).
	BuildSystemNPM BuildSystem = "npm"
	// BuildSystemPython is detected from a root-level pyproject.toml or setup.py.
	BuildSystemPython BuildSystem = "python"
	// BuildSystemDotnet is detected from a root-level *.sln, *.csproj, or
	// Directory.Build.props. Solution and project files are user-named, so
	// detection uses glob matching rather than a fixed name.
	BuildSystemDotnet BuildSystem = "dotnet"
	// BuildSystemMaven is detected from a root-level pom.xml.
	BuildSystemMaven BuildSystem = "maven"
	// BuildSystemGradle is detected from a root-level build.gradle,
	// build.gradle.kts, settings.gradle, or settings.gradle.kts.
	BuildSystemGradle BuildSystem = "gradle"
	// BuildSystemCMake is detected from a root-level CMakeLists.txt.
	// C/C++ repos commonly coexist with other markers (e.g. Bazel), so cmake
	// appears after all language-specific systems to avoid displacing a more
	// specific primary.
	BuildSystemCMake BuildSystem = "cmake"
	// BuildSystemMeson is detected from a root-level meson.build.
	BuildSystemMeson BuildSystem = "meson"
	// BuildSystemMake is detected from a root-level Makefile or GNUmakefile.
	// Heterogeneous make targets are not introspectable, so detection is recorded
	// but repro guidance stays generic for make-only repos.
	BuildSystemMake BuildSystem = "make"
	// BuildSystemNinja is detected from a root-level build.ninja.
	// Like make, ninja targets are not introspectable; detection is informational.
	BuildSystemNinja BuildSystem = "ninja"
	// BuildSystemZig is detected from a root-level build.zig.
	// `zig build test` is the canonical test entry point for a Zig package.
	BuildSystemZig BuildSystem = "zig"
	// BuildSystemGleam is detected from a root-level gleam.toml.
	// `gleam test` is the canonical test entry point for a Gleam project.
	BuildSystemGleam BuildSystem = "gleam"
	// BuildSystemElixir is detected from a root-level mix.exs.
	// `mix test` is the canonical test entry point for an Elixir project.
	BuildSystemElixir BuildSystem = "elixir"
)

// DetectBuildSystems scans the root-level marker files in repoDir and returns
// every matching build system in a deterministic priority order:
//
//  1. Bazel (MODULE.bazel / WORKSPACE / WORKSPACE.bazel) — explicit build graph
//     beats all implicit conventions.
//  2. GoWorkspace (go.work) — multi-module workspace overrides single-module.
//  3. JSWorkspace (pnpm-workspace.yaml, turbo.json, nx.json) — polyrepo JS
//     workspace beats bare npm.
//  4. GoModule (go.mod) — standard single-module Go.
//  5. Cargo (Cargo.toml) — Rust.
//  6. NPM (package.json) — bare npm / single-package JS.
//  7. Python (pyproject.toml, setup.py) — Python.
//  8. Dotnet (*.sln, *.csproj, Directory.Build.props) — C# / .NET.
//  9. Maven (pom.xml) — Java / JVM with Maven.
//  10. Gradle (build.gradle, build.gradle.kts, settings.gradle,
//     settings.gradle.kts) — Java / Kotlin with Gradle.
//  11. CMake (CMakeLists.txt) — C/C++ with cmake+ctest support.
//  12. Meson (meson.build) — C/C++ with meson test support.
//  13. Make (Makefile / GNUmakefile) — detected but heterogeneous; no specific
//     repro guidance is generated (targets are not introspectable).
//  14. Ninja (build.ninja) — same constraint as make.
//  15. Zig (build.zig) — `zig build test`.
//  16. Gleam (gleam.toml) — `gleam test`.
//  17. Elixir (mix.exs) — `mix test`.
//
// CMake/Meson/Make/Ninja (11-14) precede the single-marker language ecosystems
// (15-17), so a Go repo with a convenience Makefile keeps go_module primary and a
// Bazel C++ repo keeps bazel first; a repo pairing build.zig/gleam.toml/mix.exs
// with a root CMakeLists.txt/meson.build resolves to the C/C++ suite command.
//
// Multiple entries may be returned when markers coexist (e.g. Bazel + go.mod
// is common in mixed repos). The slice is always ordered as above; callers that
// want only the "primary" system take index 0.
//
// An empty slice means no recognised marker was found.
func DetectBuildSystems(repoDir string) []BuildSystem {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(repoDir, name))
		return err == nil
	}

	// hasGlob reports whether any root-level file matches the given glob pattern
	// (e.g. "*.sln"). It operates on the immediate directory only — no
	// sub-directory traversal — so it matches user-named project files such as
	// MyApp.sln or MyLib.csproj without requiring a fixed filename.
	hasGlob := func(pattern string) bool {
		matches, err := filepath.Glob(filepath.Join(repoDir, pattern))
		return err == nil && len(matches) > 0
	}

	var out []BuildSystem

	// 1. Bazel: any Bazel workspace root marker.
	if has("MODULE.bazel") || has("WORKSPACE") || has("WORKSPACE.bazel") {
		out = append(out, BuildSystemBazel)
	}

	// 2. Go workspace (multi-module).
	if has("go.work") {
		out = append(out, BuildSystemGoWorkspace)
	}

	// 3. JS workspace: pnpm, Turborepo, or Nx.
	if has("pnpm-workspace.yaml") || has("turbo.json") || has("nx.json") {
		out = append(out, BuildSystemJSWorkspace)
	}

	// 4. Standard single-module Go.
	if has("go.mod") {
		out = append(out, BuildSystemGoModule)
	}

	// 5. Rust / Cargo.
	if has("Cargo.toml") {
		out = append(out, BuildSystemCargo)
	}

	// 6. NPM (bare, no workspace marker already detected).
	if has("package.json") {
		out = append(out, BuildSystemNPM)
	}

	// 7. Python.
	if has("pyproject.toml") || has("setup.py") {
		out = append(out, BuildSystemPython)
	}

	// 8. Dotnet — C# / .NET. Solution and project files are user-named, so *.sln
	// and *.csproj use glob matching; Directory.Build.props is a fixed name.
	if hasGlob("*.sln") || hasGlob("*.csproj") || has("Directory.Build.props") {
		out = append(out, BuildSystemDotnet)
	}

	// 9. Maven — Java / JVM with Maven.
	if has("pom.xml") {
		out = append(out, BuildSystemMaven)
	}

	// 10. Gradle — Java / Kotlin with Gradle.
	if has("build.gradle") || has("build.gradle.kts") || has("settings.gradle") || has("settings.gradle.kts") {
		out = append(out, BuildSystemGradle)
	}

	// 11. CMake — C/C++ with cmake+ctest.
	if has("CMakeLists.txt") {
		out = append(out, BuildSystemCMake)
	}

	// 12. Meson — C/C++ with meson test.
	if has("meson.build") {
		out = append(out, BuildSystemMeson)
	}

	// 13. Make — heterogeneous; detect-and-record only.
	if has("Makefile") || has("GNUmakefile") {
		out = append(out, BuildSystemMake)
	}

	// 14. Ninja — heterogeneous; detect-and-record only.
	if has("build.ninja") {
		out = append(out, BuildSystemNinja)
	}

	// 15. Zig — `zig build test` is the canonical test entry point.
	if has("build.zig") {
		out = append(out, BuildSystemZig)
	}

	// 16. Gleam — `gleam test` is the canonical test entry point.
	if has("gleam.toml") {
		out = append(out, BuildSystemGleam)
	}

	// 17. Elixir — `mix test` is the canonical test entry point.
	if has("mix.exs") {
		out = append(out, BuildSystemElixir)
	}

	return out
}

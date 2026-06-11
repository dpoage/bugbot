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

	return out
}

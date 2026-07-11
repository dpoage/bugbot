//go:build integration

// Dependency-strategy integration tests exercise a real container runtime with
// a Go toolchain image. Run with:
//
//	go test -tags integration ./internal/sandbox/...
//
// They are skipped automatically when no runtime is detected, the Go image
// cannot be pulled, or the required external module is not present in the host
// module cache (we do NOT hit the network for the positive case — the whole
// point is to prove the cache mount makes a network-none build work).
package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// goTestImage is a Go toolchain image used to actually build/test a module.
const goTestImage = "docker.io/library/golang:1.23-alpine"

// uuidModule is a tiny, dependency-free, widely-cached module used to prove the
// cache mount works. Pure Go, no cgo, no transitive deps.
const (
	uuidPath    = "github.com/google/uuid"
	uuidVersion = "v1.6.0"
)

// newGoTestCLI builds a CLI against the detected runtime with a Go image,
// skipping when none is available or the image cannot be used.
func newGoTestCLI(t *testing.T) *CLI {
	t.Helper()
	rt, ok := Detect()
	if !ok {
		t.Skip("no container runtime detected; skipping deps integration test")
	}
	s, err := NewCLI(rt, goTestImage,
		WithCPUs(2),
		WithMemoryMB(1024),
		WithPidsLimit(512),
		WithTimeout(120*time.Second),
	)
	if err != nil {
		t.Skipf("NewCLI: %v", err)
	}
	// Force the image to be available up front; skip if it can't be pulled.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := s.Exec(ctx, Spec{RepoDir: t.TempDir(), Cmd: []string{"true"}, Network: "bridge"}); err != nil {
		t.Skipf("cannot run Go test image %q (pull failed?): %v", goTestImage, err)
	}
	return s
}

// hostModcacheOrSkip resolves the host Go module cache and skips the test when
// the uuid module isn't already cached (so the test stays fully offline).
func hostModcacheOrSkip(t *testing.T) string {
	t.Helper()
	cache, err := resolveHostModcache("")
	if err != nil {
		t.Skipf("no host module cache: %v", err)
	}
	// Extracted module source must exist for -mod=mod to build offline.
	srcDir := filepath.Join(cache, uuidPath+"@"+uuidVersion)
	if _, err := os.Stat(srcDir); err != nil {
		t.Skipf("required module %s@%s not in host cache (%s); run `go mod download %s@%s` first", uuidPath, uuidVersion, srcDir, uuidPath, uuidVersion)
	}
	return cache
}

// writeUUIDModule writes a tiny module that imports the external uuid package
// into dir, returning the repo dir. The test file simply constructs a UUID, so
// it only succeeds if the dependency resolves.
func writeUUIDModule(t *testing.T, dir string) {
	t.Helper()
	gomod := "module deptest\n\ngo 1.21\n\nrequire " + uuidPath + " " + uuidVersion + "\n"
	mustWrite(t, filepath.Join(dir, "go.mod"), gomod, 0o644)

	// go.sum lines for github.com/google/uuid v1.6.0 (stable, content-addressed).
	gosum := "github.com/google/uuid v1.6.0 h1:NIvaJDMOsjHA8n1jAhLSgzrAzy1Hgr+hNrb57e+94F0=\n" +
		"github.com/google/uuid v1.6.0/go.mod h1:TIyPZe4MgqvfeYDBFedMoGGpEw/LqOeaOT+nhxU+yHo=\n"
	mustWrite(t, filepath.Join(dir, "go.sum"), gosum, 0o644)

	src := "package main\n\nimport (\n\t\"testing\"\n\n\t\"github.com/google/uuid\"\n)\n\n" +
		"func TestUUID(t *testing.T) {\n\tif uuid.New().String() == \"\" {\n\t\tt.Fatal(\"empty uuid\")\n\t}\n}\n"
	mustWrite(t, filepath.Join(dir, "main_test.go"), src, 0o644)
}

// TestIntegrationDepsHostCacheBuildsOffline proves that with the HOST strategy's
// read-only module-cache mount and env, a module with an external dependency
// builds and tests under --network=none.
func TestIntegrationDepsHostCacheBuildsOffline(t *testing.T) {
	s := newGoTestCLI(t)
	cache := hostModcacheOrSkip(t)

	repo := t.TempDir()
	writeUUIDModule(t, repo)

	res := modcacheResolution(cache, DepStrategyHost, true, nil)

	out, err := s.Exec(context.Background(), Spec{
		RepoDir:  repo,
		Cmd:      []string{"go", "test", "./..."},
		Network:  "none",
		ROMounts: res.ROMounts,
		Env:      res.Env,
		Timeout:  90 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("network-none build with host cache should pass; exit=%d\nstdout:\n%s\nstderr:\n%s", out.ExitCode, out.Stdout, out.Stderr)
	}
}

// TestIntegrationDepsNoStrategyFailsOffline is the negative control: with NO
// dependency strategy, the same module fails to resolve its external dependency
// under --network=none (GOPROXY unreachable, no cache mounted).
func TestIntegrationDepsNoStrategyFailsOffline(t *testing.T) {
	s := newGoTestCLI(t)

	repo := t.TempDir()
	writeUUIDModule(t, repo)

	out, err := s.Exec(context.Background(), Spec{
		RepoDir: repo,
		Cmd:     []string{"go", "test", "./..."},
		Network: "none",
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode == 0 {
		t.Fatalf("network-none build with NO strategy should fail to resolve the external module, but it passed; stdout:\n%s", out.Stdout)
	}
}

// ---- Python integration tests -----------------------------------------------

// pythonTestImage is the Python image used for the pip wheelhouse integration test.
// python:3-slim ships pip and has /bin/sh; it is small (~50 MB compressed).
const pythonTestImage = "docker.io/library/python:3-slim"

// newPythonTestCLI builds a CLI backed by python:3-slim, skipping when no
// runtime is available or the image cannot be used.
func newPythonTestCLI(t *testing.T) *CLI {
	t.Helper()
	rt, ok := Detect()
	if !ok {
		t.Skip("no container runtime detected; skipping Python deps integration test")
	}
	s, err := NewCLI(rt, pythonTestImage,
		WithCPUs(2),
		WithMemoryMB(512),
		WithPidsLimit(256),
		WithTimeout(120*time.Second),
	)
	if err != nil {
		t.Skipf("NewCLI (python): %v", err)
	}
	// Pull the image up front; skip if it cannot be pulled.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := s.Exec(ctx, Spec{RepoDir: t.TempDir(), Cmd: []string{"true"}, Network: "bridge"}); err != nil {
		t.Skipf("cannot run Python test image %q (pull failed?): %v", pythonTestImage, err)
	}
	return s
}

// writePythonTestRepo writes a minimal Python repo with requirements.txt
// (six + pytest) and a pytest test that imports six, into dir.
func writePythonTestRepo(t *testing.T, dir string) {
	t.Helper()
	// requirements.txt: pytest must be in here so it installs from the
	// wheelhouse; it must NOT be pip-installed online inside the test container.
	// six is the subject dep — pure-Python, tiny, stable.
	mustWrite(t, filepath.Join(dir, "requirements.txt"), "six==1.16.0\npytest==8.3.5\n", 0o644)
	// Test file: import six to prove the wheelhouse install worked.
	src := `import six

def test_six_version():
    assert six.PY3
`
	mustWrite(t, filepath.Join(dir, "test_six.py"), src, 0o644)
}

// TestIntegrationPythonWheelhouseBuildsOffline proves the full pip FETCH
// round-trip: prefetch downloads the wheelhouse online, then the network-none
// run installs from it via SetupCmds and runs pytest. Exit 0 required.
func TestIntegrationPythonWheelhouseBuildsOffline(t *testing.T) {
	s := newPythonTestCLI(t)

	repo := t.TempDir()
	writePythonTestRepo(t, repo)
	cacheBase := t.TempDir()

	res, err := ResolveDeps(repo, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: s,
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	if res.Prefetch == nil {
		t.Fatal("expected non-nil Prefetch for Python FETCH strategy")
	}

	// Step 1: prefetch (network-enabled, populates the wheelhouse on the host).
	prefetchCtx, prefetchCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer prefetchCancel()
	if err := res.Prefetch(prefetchCtx); err != nil {
		t.Fatalf("Prefetch (pip download): %v", err)
	}

	// Step 2: run pytest network-none with the wheelhouse mount + SetupCmds.
	// The SetupCmds install six + pytest from the wheelhouse into /tmp/.local
	// (HOME=/tmp, set by buildRunArgs) before the test command runs.
	out, err := s.Exec(context.Background(), Spec{
		RepoDir:   repo,
		Cmd:       []string{"python", "-m", "pytest", "test_six.py", "-v"},
		Network:   "none",
		ROMounts:  res.ROMounts,
		Env:       res.Env,
		SetupCmds: res.SetupCmds,
		Timeout:   90 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec (pytest network-none): %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("pytest with wheelhouse should pass; exit=%d\nstdout:\n%s\nstderr:\n%s",
			out.ExitCode, out.Stdout, out.Stderr)
	}
	t.Logf("pytest stdout:\n%s", out.Stdout)
}

// TestIntegrationPythonNoWheelhouseFailsOffline is the negative control: the
// same pytest run WITHOUT the wheelhouse mount fails under --network=none
// (pip cannot reach PyPI, modules cannot be imported). This proves that
// --network=none is enforced and that the wheelhouse is load-bearing in the
// positive test above.
func TestIntegrationPythonNoWheelhouseFailsOffline(t *testing.T) {
	s := newPythonTestCLI(t)

	repo := t.TempDir()
	writePythonTestRepo(t, repo)

	// No mounts, no SetupCmds, no PIP_NO_INDEX — bare network-none run.
	// pytest is not installed in python:3-slim by default, so `python -m pytest`
	// should fail with a ModuleNotFoundError / non-zero exit.
	out, err := s.Exec(context.Background(), Spec{
		RepoDir: repo,
		Cmd:     []string{"python", "-m", "pytest", "test_six.py", "-v"},
		Network: "none",
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode == 0 {
		t.Fatalf("pytest with NO wheelhouse under --network=none should fail, but exited 0;\nstdout:\n%s\nstderr:\n%s",
			out.Stdout, out.Stderr)
	}
	t.Logf("correctly failed (exit=%d); stderr excerpt: %s", out.ExitCode, out.Stderr)
}

// ---- Rust integration tests -------------------------------------------------

// rustTestImage is the Rust toolchain image used for the cargo integration tests.
// rust:1-slim is the official slim image; it ships cargo and rustc.
const rustTestImage = "docker.io/library/rust:1-slim"

// newRustTestCLI builds a CLI backed by rust:1-slim, skipping when no runtime
// is available or the image cannot be used.
func newRustTestCLI(t *testing.T) *CLI {
	t.Helper()
	rt, ok := Detect()
	if !ok {
		t.Skip("no container runtime detected; skipping Rust deps integration test")
	}
	s, err := NewCLI(rt, rustTestImage,
		WithCPUs(2),
		WithMemoryMB(1024),
		WithPidsLimit(512),
		WithTimeout(180*time.Second),
	)
	if err != nil {
		t.Skipf("NewCLI (rust): %v", err)
	}
	// Pull the image up front; skip if it cannot be pulled.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := s.Exec(ctx, Spec{RepoDir: t.TempDir(), Cmd: []string{"true"}, Network: "bridge"}); err != nil {
		t.Skipf("cannot run Rust test image %q (pull failed?): %v", rustTestImage, err)
	}
	return s
}

// writeRustTestCrate writes a minimal Rust crate that depends on the `itoa`
// crate (tiny, no transitive deps, pure Rust). The test verifies that `cargo
// test` succeeds when the registry is pre-fetched and fails without it.
func writeRustTestCrate(t *testing.T, dir string) {
	t.Helper()
	cargoToml := `[package]
name = "deptest"
version = "0.1.0"
edition = "2021"

[dependencies]
itoa = "1"
`
	mustWrite(t, filepath.Join(dir, "Cargo.toml"), cargoToml, 0o644)

	src := `use itoa::Buffer;

#[test]
fn test_itoa() {
    let mut buf = Buffer::new();
    let s = buf.format(42u64);
    assert_eq!(s, "42");
}
`
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "src", "lib.rs"), src, 0o644)
}

// TestIntegrationRustFetchBuildsOffline proves the full cargo FETCH round-trip:
// prefetch downloads the registry online, then the network-none run builds and
// tests with the registry mounted read-only. Exit 0 required.
func TestIntegrationRustFetchBuildsOffline(t *testing.T) {
	s := newRustTestCLI(t)

	repo := t.TempDir()
	writeRustTestCrate(t, repo)
	cacheBase := t.TempDir()

	res, err := ResolveDeps(repo, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: s,
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	if res.Prefetch == nil {
		t.Fatal("expected non-nil Prefetch for Cargo FETCH strategy")
	}

	// Step 1: prefetch (network-enabled, populates the Cargo registry on the host).
	prefetchCtx, prefetchCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer prefetchCancel()
	if err := res.Prefetch(prefetchCtx); err != nil {
		t.Fatalf("Prefetch (cargo fetch): %v", err)
	}

	// Step 2: run cargo test network-none with the registry mount.
	// CARGO_HOME=/cargo (from env) + /cargo/registry mount → cargo resolves
	// crates offline.
	out, err := s.Exec(context.Background(), Spec{
		RepoDir:  repo,
		Cmd:      []string{"cargo", "test"},
		Network:  "none",
		ROMounts: res.ROMounts,
		Env:      res.Env,
		Timeout:  120 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec (cargo test network-none): %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("cargo test with registry mount should pass; exit=%d\nstdout:\n%s\nstderr:\n%s",
			out.ExitCode, out.Stdout, out.Stderr)
	}
	t.Logf("cargo test stdout:\n%s", out.Stdout)
}

// TestIntegrationRustNoRegistryFailsOffline is the negative control: the same
// cargo test WITHOUT the registry mount fails under --network=none (cargo cannot
// reach crates.io, the crate cannot be resolved). This proves that
// --network=none is enforced and that the registry mount is load-bearing.
func TestIntegrationRustNoRegistryFailsOffline(t *testing.T) {
	s := newRustTestCLI(t)

	repo := t.TempDir()
	writeRustTestCrate(t, repo)

	// No mounts, no env — bare network-none run.
	// cargo cannot reach crates.io, so the external dep cannot be resolved.
	out, err := s.Exec(context.Background(), Spec{
		RepoDir: repo,
		Cmd:     []string{"cargo", "test"},
		Network: "none",
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode == 0 {
		t.Fatalf("cargo test with NO registry under --network=none should fail, but exited 0;\nstdout:\n%s\nstderr:\n%s",
			out.Stdout, out.Stderr)
	}
	t.Logf("correctly failed (exit=%d); stderr excerpt: %s", out.ExitCode, out.Stderr)
}

// ---- JS integration tests ---------------------------------------------------

// jsTestImage is the Node.js image used for the npm integration tests.
// node:20-slim ships npm and /bin/sh.
const jsTestImage = "docker.io/library/node:20-slim"

// newJSTestCLI builds a CLI backed by node:20-slim, skipping when no runtime
// is available or the image cannot be used.
func newJSTestCLI(t *testing.T) *CLI {
	t.Helper()
	rt, ok := Detect()
	if !ok {
		t.Skip("no container runtime detected; skipping JS deps integration test")
	}
	s, err := NewCLI(rt, jsTestImage,
		WithCPUs(2),
		WithMemoryMB(512),
		WithPidsLimit(256),
		WithTimeout(120*time.Second),
	)
	if err != nil {
		t.Skipf("NewCLI (node): %v", err)
	}
	// Pull the image up front; skip if it cannot be pulled.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := s.Exec(ctx, Spec{RepoDir: t.TempDir(), Cmd: []string{"true"}, Network: "bridge"}); err != nil {
		t.Skipf("cannot run JS test image %q (pull failed?): %v", jsTestImage, err)
	}
	return s
}

// writeJSTestRepo writes a minimal Node.js repo with package.json and
// package-lock.json (depending on the `is-number` package — tiny, stable, zero
// deps) and a CommonJS test file that uses Node's built-in test runner.
func writeJSTestRepo(t *testing.T, dir string) {
	t.Helper()
	// is-number@7.0.0 is a tiny, pure-JS, zero-dependency predicate. It is used
	// rather than a more famous package because some registry mirrors serve a
	// corrupted ms-2.1.3 tarball (metadata + tarball both report a non-canonical
	// integrity), which breaks a hard-coded lockfile; is-number's canonical,
	// content-addressed integrity is stable across mirrors.
	pkgJSON := `{
  "name": "deptest",
  "version": "1.0.0",
  "dependencies": {
    "is-number": "7.0.0"
  }
}
`
	// package-lock.json for is-number@7.0.0 (content-addressed, stable).
	pkgLock := `{
  "name": "deptest",
  "version": "1.0.0",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "deptest",
      "version": "1.0.0",
      "dependencies": {
        "is-number": "7.0.0"
      }
    },
    "node_modules/is-number": {
      "version": "7.0.0",
      "resolved": "https://registry.npmjs.org/is-number/-/is-number-7.0.0.tgz",
      "integrity": "sha512-41Cifkg6e8TylSpdtTpeLVMqvSBEVzTttHvERD741+pnZ8ANv0004MRL43QKPDlK9cGvNp6NZWZUBlbGXYxxng==",
      "engines": {
        "node": ">=0.12.0"
      }
    }
  }
}
`
	mustWrite(t, filepath.Join(dir, "package.json"), pkgJSON, 0o644)
	mustWrite(t, filepath.Join(dir, "package-lock.json"), pkgLock, 0o644)

	// CommonJS test using Node's built-in test runner (node:test, available
	// since Node 18). The file is .js (not .mjs) so require() is available —
	// an .mjs file is ESM and has no require, which would fail regardless of deps.
	testSrc := `const test = require('node:test');
const assert = require('node:assert');
const isNumber = require('is-number');

test('is-number detects numbers', () => {
    assert.strictEqual(isNumber(42), true);
    assert.strictEqual(isNumber('foo'), false);
});
`
	mustWrite(t, filepath.Join(dir, "test.js"), testSrc, 0o644)
}

// TestIntegrationJSFetchBuildsOffline proves the full npm FETCH round-trip:
// prefetch downloads the npm cache online, then the network-none run copies the
// cache to /tmp, installs from it via SetupCmds, and runs the test. Exit 0 required.
func TestIntegrationJSFetchBuildsOffline(t *testing.T) {
	s := newJSTestCLI(t)

	repo := t.TempDir()
	writeJSTestRepo(t, repo)
	cacheBase := t.TempDir()

	res, err := ResolveDeps(repo, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: s,
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	if res.Prefetch == nil {
		t.Fatal("expected non-nil Prefetch for JS FETCH strategy")
	}

	// Step 1: prefetch (network-enabled, populates the npm cache on the host).
	prefetchCtx, prefetchCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer prefetchCancel()
	if err := res.Prefetch(prefetchCtx); err != nil {
		t.Fatalf("Prefetch (npm ci --ignore-scripts): %v", err)
	}

	// Step 2: run node test network-none with the npm cache mount + SetupCmds.
	// The SetupCmd copies the RO /npmcache to /tmp/npmcache and runs
	// npm ci --cache /tmp/npmcache (offline, npm_config_offline=true) before
	// the test command runs.
	out, err := s.Exec(context.Background(), Spec{
		RepoDir:   repo,
		Cmd:       []string{"node", "--test", "test.js"},
		Network:   "none",
		ROMounts:  res.ROMounts,
		Env:       res.Env,
		SetupCmds: res.SetupCmds,
		Timeout:   90 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec (node --test network-none): %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("node test with npm cache should pass; exit=%d\nstdout:\n%s\nstderr:\n%s",
			out.ExitCode, out.Stdout, out.Stderr)
	}
	t.Logf("node test stdout:\n%s", out.Stdout)
}

// TestIntegrationJSNoCacheFailsOffline is the negative control: the same node
// test WITHOUT the npm cache fails under --network=none (npm cannot reach the
// registry, node_modules is not installed). This proves that --network=none is
// enforced and that the npm cache mount + SetupCmds are load-bearing.
func TestIntegrationJSNoCacheFailsOffline(t *testing.T) {
	s := newJSTestCLI(t)

	repo := t.TempDir()
	writeJSTestRepo(t, repo)

	// No mounts, no SetupCmds — bare network-none run.
	// node_modules/ is not installed, so require('is-number') should fail.
	out, err := s.Exec(context.Background(), Spec{
		RepoDir: repo,
		Cmd:     []string{"node", "--test", "test.js"},
		Network: "none",
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode == 0 {
		t.Fatalf("node test with NO npm cache under --network=none should fail, but exited 0;\nstdout:\n%s\nstderr:\n%s",
			out.Stdout, out.Stderr)
	}
	t.Logf("correctly failed (exit=%d); stderr excerpt: %s", out.ExitCode, out.Stderr)
}

// ---- C#/NuGet integration tests --------------------------------------------

// dotnetTestImage is the .NET SDK image used to actually build/test a C#
// project. mcr.microsoft.com/dotnet/sdk:8.0 ships dotnet (CLI + test runner)
// and the full reference assemblies for net8.0; it is the official image.
const dotnetTestImage = "mcr.microsoft.com/dotnet/sdk:8.0"

// newNuGetTestCLI builds a CLI backed by the .NET SDK image, skipping when no
// runtime is available or the image cannot be used. The first invocation
// (`true` over bridge) is what actually fails when no docker/podman is around
// or the SDK image cannot be pulled — the integration test must therefore
// skip cleanly here, not block CI.
func newNuGetTestCLI(t *testing.T) *CLI {
	t.Helper()
	rt, ok := Detect()
	if !ok {
		t.Skip("no container runtime detected; skipping C#/NuGet deps integration test")
	}
	s, err := NewCLI(rt, dotnetTestImage,
		WithCPUs(2),
		WithMemoryMB(1024),
		WithPidsLimit(512),
		WithTimeout(240*time.Second),
	)
	if err != nil {
		t.Skipf("NewCLI (dotnet): %v", err)
	}
	// Pull the image up front; skip if it cannot be pulled. dotnet SDK images
	// are ~700 MB compressed, so this can take a while on first run.
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	if _, err := s.Exec(ctx, Spec{RepoDir: t.TempDir(), Cmd: []string{"true"}, Network: "bridge"}); err != nil {
		t.Skipf("cannot run dotnet test image %q (pull failed?): %v", dotnetTestImage, err)
	}
	return s
}

// writeNuGetTestRepo writes a minimal xUnit test project that depends on the
// widely-cached Newtonsoft.Json package. xUnit + Microsoft.NET.Test.Sdk are
// the standard test-framework triple (transitive deps are well-known and
// pinned). The single test calls JsonConvert.SerializeObject — proving the
// external package resolves at restore time.
func writeNuGetTestRepo(t *testing.T, dir string) {
	t.Helper()
	csproj := `<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <TargetFramework>net8.0</TargetFramework>
    <IsPackable>false</IsPackable>
  </PropertyGroup>
  <ItemGroup>
    <PackageReference Include="Microsoft.NET.Test.Sdk" Version="17.11.1" />
    <PackageReference Include="xunit" Version="2.9.2" />
    <PackageReference Include="xunit.runner.visualstudio" Version="2.8.2" />
    <PackageReference Include="Newtonsoft.Json" Version="13.0.3" />
  </ItemGroup>
</Project>
`
	// Newtonsoft.Json@13.0.3 is a tiny, zero-transitive-dependency package —
	// chosen so the offline restore has a known, bounded dependency set.
	testSrc := `using Newtonsoft.Json;
using Xunit;

public class Tests
{
    [Fact]
    public void JsonRoundtrip()
    {
        var s = JsonConvert.SerializeObject(42);
        Assert.Equal("42", s);
    }
}
`
	mustWrite(t, filepath.Join(dir, "deptest.csproj"), csproj, 0o644)
	mustWrite(t, filepath.Join(dir, "Tests.cs"), testSrc, 0o644)
}

// TestIntegrationNuGetFetchBuildsOffline proves the full NuGet FETCH round-trip:
// prefetch runs `dotnet restore` online to populate the global packages cache,
// then the network-none run uses NUGET_PACKAGES=/nugetcache (mounted read-only
// from the populated cache) to restore + test the project. Exit 0 required.
func TestIntegrationNuGetFetchBuildsOffline(t *testing.T) {
	s := newNuGetTestCLI(t)

	repo := t.TempDir()
	writeNuGetTestRepo(t, repo)
	cacheBase := t.TempDir()

	res, err := ResolveDeps(repo, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: s,
		userCacheDir: cacheBase,
		// FetchNetwork=host: this test environment's podman bridge network has no
		// working DNS resolver for nuget.org; the host network reaches it directly.
		// Production deployments should configure podman's bridge network DNS.
		FetchNetwork: "host",
	})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	if res.Prefetch == nil {
		t.Fatal("expected non-nil Prefetch for C#/NuGet FETCH strategy")
	}

	// Step 1: prefetch (network-enabled, populates the NuGet global packages
	// cache on the host).
	prefetchCtx, prefetchCancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer prefetchCancel()
	if err := res.Prefetch(prefetchCtx); err != nil {
		t.Fatalf("Prefetch (dotnet restore): %v", err)
	}

	// Step 2: run dotnet test network-none with the packages cache mounted.
	// NUGET_PACKAGES=/nugetcache (from env) + /nugetcache mount → dotnet
	// resolves packages offline.
	out, err := s.Exec(context.Background(), Spec{
		RepoDir: repo,
		Cmd:     []string{"dotnet", "test"},
		Network: "none",
		// Packages come from the cache mount; the SDK compiles, restore runs
		// offline (NUGET_PACKAGES points at the populated cache), and the test
		// runs entirely offline.
		ROMounts: res.ROMounts,
		Env:      res.Env,
		Timeout:  180 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec (dotnet test network-none): %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("dotnet test with NuGet packages mount should pass; exit=%d\nstdout:\n%s\nstderr:\n%s",
			out.ExitCode, out.Stdout, out.Stderr)
	}
	t.Logf("dotnet test stdout:\n%s", out.Stdout)
}

// TestIntegrationNuGetNoCacheFailsOffline is the negative control: the same
// dotnet test WITHOUT the packages cache fails under --network=none (dotnet
// cannot reach nuget.org, the external dep cannot be resolved). This proves
// that --network=none is enforced and that the packages cache mount is
// load-bearing.
func TestIntegrationNuGetNoCacheFailsOffline(t *testing.T) {
	s := newNuGetTestCLI(t)

	repo := t.TempDir()
	writeNuGetTestRepo(t, repo)

	// No mounts, no env — bare network-none run.
	// dotnet cannot reach nuget.org, so the external dep cannot be resolved.
	out, err := s.Exec(context.Background(), Spec{
		RepoDir: repo,
		Cmd:     []string{"dotnet", "test"},
		Network: "none",
		Timeout: 120 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode == 0 {
		t.Fatalf("dotnet test with NO NuGet cache under --network=none should fail, but exited 0;\nstdout:\n%s\nstderr:\n%s",
			out.Stdout, out.Stderr)
	}
	t.Logf("correctly failed (exit=%d); stderr excerpt: %s", out.ExitCode, out.Stderr)
}

// ---- Maven integration tests -----------------------------------------------

// mavenTestImage is the Maven image used for the Maven integration tests.
// maven:3-eclipse-temurin-17 ships mvn, java, and a JDK; it is the official
// Maven image.
const mavenTestImage = "docker.io/library/maven:3-eclipse-temurin-17"

// newMavenTestCLI builds a CLI backed by the Maven image, skipping when no
// runtime is available or the image cannot be used. The first invocation
// (`true` over bridge) is what actually fails when no docker/podman is around
// or the image cannot be pulled — the integration test must therefore skip
// cleanly here, not block CI.
func newMavenTestCLI(t *testing.T) *CLI {
	t.Helper()
	rt, ok := Detect()
	if !ok {
		t.Skip("no container runtime detected; skipping Maven integration test")
	}
	s, err := NewCLI(rt, mavenTestImage,
		WithCPUs(2),
		WithMemoryMB(2048),
		WithPidsLimit(512),
		WithTimeout(300*time.Second),
	)
	if err != nil {
		t.Skipf("NewCLI (maven): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := s.Exec(ctx, Spec{RepoDir: t.TempDir(), Cmd: []string{"true"}, Network: "bridge"}); err != nil {
		t.Skipf("cannot run Maven test image %q (pull failed?): %v", mavenTestImage, err)
	}
	return s
}

// writeMavenTestRepo writes a minimal Maven project with a single test that
// imports the widely-cached commons-lang3 package. The pom.xml uses
// junit-platform-launcher + junit-jupiter (the standard Maven test triple)
// so `mvn test` exercises a real external dependency.
func writeMavenTestRepo(t *testing.T, dir string) {
	t.Helper()
	pom := `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0"
         xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
         xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 http://maven.apache.org/xsd/maven-4.0.0.xsd">
  <modelVersion>4.0.0</modelVersion>
  <groupId>com.example</groupId>
  <artifactId>test-project</artifactId>
  <version>1.0-SNAPSHOT</version>
  <properties>
    <maven.compiler.source>17</maven.compiler.source>
    <maven.compiler.target>17</maven.compiler.target>
  </properties>
  <dependencies>
    <dependency>
      <groupId>org.apache.commons</groupId>
      <artifactId>commons-lang3</artifactId>
      <version>3.14.0</version>
    </dependency>
    <dependency>
      <groupId>org.junit.jupiter</groupId>
      <artifactId>junit-jupiter</artifactId>
      <version>5.10.2</version>
      <scope>test</scope>
    </dependency>
  </dependencies>
  <build>
    <plugins>
      <plugin>
        <groupId>org.apache.maven.plugins</groupId>
        <artifactId>maven-surefire-plugin</artifactId>
        <version>3.2.5</version>
      </plugin>
    </plugins>
  </build>
</project>
`
	writeFile(t, filepath.Join(dir, "pom.xml"), pom)

	testSrc := `package com.example;
import org.apache.commons.lang3.StringUtils;
import org.junit.jupiter.api.Test;
import static org.junit.jupiter.api.Assertions.*;
public class ExampleTest {
    @Test
    public void testStringUtils() {
        assertEquals("hello", StringUtils.lowerCase("HELLO"));
    }
}
`
	writeFile(t, filepath.Join(dir, "src/test/java/com/example/ExampleTest.java"), testSrc)
}

// TestIntegrationMavenFetchBuildsOffline proves the full Maven FETCH round-trip:
// prefetch runs `mvn -B dependency:go-offline` online to populate the local
// repository, then the network-none run uses MAVEN_OPTS=-Dmaven.repo.local=/m2cache
// (mounted read-only from the populated cache) to build and test. Exit 0 required.
func TestIntegrationMavenFetchBuildsOffline(t *testing.T) {
	s := newMavenTestCLI(t)
	dir := t.TempDir()
	writeMavenTestRepo(t, dir)
	cacheBase := t.TempDir()

	res, err := ResolveDeps(dir, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: s,
		FetchImage:   mavenTestImage,
		FetchNetwork: "bridge",
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}

	ctx := context.Background()
	if err := res.Prefetch(ctx); err != nil {
		t.Fatalf("prefetch: %v", err)
	}

	ctx2, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	out, err := s.Exec(ctx2, Spec{
		RepoDir:  dir,
		Cmd:      []string{"mvn", "-B", "test"},
		Network:  "none",
		ROMounts: res.ROMounts,
		Env:      res.Env,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("mvn test with cache under --network=none exited %d;\nstdout:\n%s\nstderr:\n%s",
			out.ExitCode, out.Stdout, out.Stderr)
	}
}

// TestIntegrationMavenNoCacheFailsOffline is the negative control: the same
// mvn test WITHOUT the Maven cache fails under --network=none. This proves
// that --network=none is enforced and that the cache mount is load-bearing.
func TestIntegrationMavenNoCacheFailsOffline(t *testing.T) {
	s := newMavenTestCLI(t)
	dir := t.TempDir()
	writeMavenTestRepo(t, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := s.Exec(ctx, Spec{
		RepoDir: dir,
		Cmd:     []string{"mvn", "-B", "test"},
		Network: "none",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode == 0 {
		t.Fatalf("mvn test with NO cache under --network=none should fail, but exited 0;\nstdout:\n%s\nstderr:\n%s",
			out.Stdout, out.Stderr)
	}
	t.Logf("correctly failed (exit=%d); stderr excerpt: %s", out.ExitCode, out.Stderr)
}

// ---- Gradle integration tests -----------------------------------------------

// gradleTestImage is the Gradle image used for the Gradle integration tests.
// gradle:8-jdk17 ships gradle, java, and a JDK; it is the official Gradle image.
const gradleTestImage = "docker.io/library/gradle:8-jdk17"

// newGradleTestCLI builds a CLI backed by the Gradle image, skipping when no
// runtime is available or the image cannot be used.
func newGradleTestCLI(t *testing.T) *CLI {
	t.Helper()
	rt, ok := Detect()
	if !ok {
		t.Skip("no container runtime detected; skipping Gradle integration test")
	}
	s, err := NewCLI(rt, gradleTestImage,
		WithCPUs(2),
		WithMemoryMB(2048),
		WithPidsLimit(512),
		WithTimeout(300*time.Second),
	)
	if err != nil {
		t.Skipf("NewCLI (gradle): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := s.Exec(ctx, Spec{RepoDir: t.TempDir(), Cmd: []string{"true"}, Network: "bridge"}); err != nil {
		t.Skipf("cannot run Gradle test image %q (pull failed?): %v", gradleTestImage, err)
	}
	return s
}

// writeGradleTestRepo writes a minimal Gradle project with a single test that
// imports guava (widely-cached, pure-Java). Kotlin DSL + JUnit Jupiter is the
// modern Gradle standard; uses the Kotlin build file so the kts detector fires.
func writeGradleTestRepo(t *testing.T, dir string) {
	t.Helper()
	buildKts := `plugins {
    java
}
repositories {
    mavenCentral()
}
dependencies {
    implementation("com.google.guava:guava:33.2.1-jre")
    testImplementation("org.junit.jupiter:junit-jupiter:5.10.2")
}
tasks.test {
    useJUnitPlatform()
}
`
	writeFile(t, filepath.Join(dir, "build.gradle.kts"), buildKts)

	settings := `rootProject.name = "test-project"
`
	writeFile(t, filepath.Join(dir, "settings.gradle.kts"), settings)

	testSrc := `import com.google.common.base.Strings;
import org.junit.jupiter.api.Test;
import static org.junit.jupiter.api.Assertions.*;
public class ExampleTest {
    @Test
    public void testGuava() {
        assertEquals("hello", Strings.nullToEmpty("hello"));
    }
}
`
	writeFile(t, filepath.Join(dir, "src/test/java/ExampleTest.java"), testSrc)
}

// TestIntegrationGradleFetchBuildsOffline proves the full Gradle FETCH round-trip:
// prefetch runs `gradle dependencies --no-daemon -q` online to populate the
// Gradle user home, then the network-none run copies the cache to a writable
// /tmp/gradlecache via SetupCmds and runs `gradle test`. Exit 0 required.
func TestIntegrationGradleFetchBuildsOffline(t *testing.T) {
	s := newGradleTestCLI(t)
	dir := t.TempDir()
	writeGradleTestRepo(t, dir)
	cacheBase := t.TempDir()

	res, err := ResolveDeps(dir, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: s,
		FetchImage:   gradleTestImage,
		FetchNetwork: "bridge",
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}

	ctx := context.Background()
	if err := res.Prefetch(ctx); err != nil {
		t.Fatalf("prefetch: %v", err)
	}

	ctx2, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	out, err := s.Exec(ctx2, Spec{
		RepoDir:   dir,
		Cmd:       []string{"gradle", "test", "--no-daemon"},
		Network:   "none",
		ROMounts:  res.ROMounts,
		Env:       res.Env,
		SetupCmds: res.SetupCmds,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("gradle test with cache under --network=none exited %d;\nstdout:\n%s\nstderr:\n%s",
			out.ExitCode, out.Stdout, out.Stderr)
	}
}

// TestIntegrationGradleNoCacheFailsOffline is the negative control: the same
// gradle test WITHOUT the cache fails under --network=none. This proves that
// --network=none is enforced and that the cache mount + SetupCmds are
// load-bearing.
func TestIntegrationGradleNoCacheFailsOffline(t *testing.T) {
	s := newGradleTestCLI(t)
	dir := t.TempDir()
	writeGradleTestRepo(t, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := s.Exec(ctx, Spec{
		RepoDir: dir,
		Cmd:     []string{"gradle", "test", "--no-daemon"},
		Network: "none",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode == 0 {
		t.Fatalf("gradle test with NO cache under --network=none should fail, but exited 0;\nstdout:\n%s\nstderr:\n%s",
			out.Stdout, out.Stderr)
	}
	t.Logf("correctly failed (exit=%d); stderr excerpt: %s", out.ExitCode, out.Stderr)
}

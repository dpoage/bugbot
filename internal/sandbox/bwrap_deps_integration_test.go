//go:build integration

// Bwrap dependency-strategy integration tests prove bugbot-48ya acceptance 4:
// a go test importing an external module and a python test importing a
// third-party package both execute under a REAL bwrap + network=none run
// with dep_strategy: host. Run with:
//
//	go test -tags integration ./internal/sandbox/...
package sandbox

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBwrapGoHostDepsOffline proves the bwrap analogue of
// TestIntegrationDepsHostCacheBuildsOffline (deps_integration_test.go,
// container backend): with dep_strategy: host, ResolveDeps mounts the real
// host GOMODCACHE (Shared=true, GOFLAGS/GOMODCACHE/GOPROXY=off) plus the
// host `go` toolchain (via DepOptions.HostToolchains — the same field
// repro.New always sets from config), and `go test ./...` against a module
// with an external dependency passes under a live bwrap + network=none run.
// No container, no baked image — this is exactly the bwrap gap the bead
// closes.
func TestBwrapGoHostDepsOffline(t *testing.T) {
	if ok, reason := DetectBwrap(); !ok {
		t.Skipf("bwrap unavailable: %s", reason)
	}
	cache := hostModcacheOrSkip(t) // shared helper from deps_integration_test.go

	tc, err := ResolveHostToolchains([]string{"go"})
	if err != nil || len(tc.Mounts) == 0 {
		t.Skipf("no host go toolchain resolvable on PATH: %v", err)
	}

	// Go's build system forks a worker process per compiled package plus
	// assembler/linker helpers (see TestBwrapGoReproRunsGreen's identical
	// note) — the default PidsLimit is too low even for this tiny module.
	bw, err := NewBwrap(
		WithBwrapCPUs(2),
		WithBwrapMemoryMB(1024),
		WithBwrapPidsLimit(512),
		WithBwrapTimeout(90*time.Second),
	)
	if err != nil {
		t.Skipf("NewBwrap: %v", err)
	}
	t.Cleanup(func() { _ = bw.Close() })

	repo := t.TempDir()
	writeUUIDModule(t, repo) // shared helper from deps_integration_test.go

	res, err := ResolveDeps(repo, DepOptions{
		Strategy:       DepStrategyHost,
		FetchSandbox:   bw,
		HostToolchains: []string{"go"},
		hostModcache:   cache,
	})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	if res.Strategy != DepStrategyHost {
		t.Fatalf("Strategy = %q, want host", res.Strategy)
	}

	out, err := bw.Exec(context.Background(), Spec{
		RepoDir:  repo,
		Cmd:      []string{"go", "test", "./..."},
		Network:  "none",
		ROMounts: res.ROMounts,
		// CGO_ENABLED=0: the bwrap sandbox's fixed allowlist has no C
		// compiler, and the uuid module itself needs none — this only
		// avoids `go test` reflexively building runtime/cgo, unrelated to
		// the dependency-mount mechanism under test here.
		Env:       append(append([]string(nil), res.Env...), "CGO_ENABLED=0"),
		SetupCmds: res.SetupCmds,
		Timeout:   90 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("bwrap dep_strategy:host build should pass under network=none; exit=%d\nstdout:\n%s\nstderr:\n%s", out.ExitCode, out.Stdout, out.Stderr)
	}
}

// TestBwrapPythonHostDepsOffline proves the bwrap-only Python HOST extension
// (resolveBwrapPythonHostDeps) end to end: a site-packages-shaped directory
// is mounted read-only, PYTHONPATH is set, and a script importing from it
// runs successfully under a live bwrap + network=none run alongside the
// host `python3` toolchain mount.
//
// The "third-party package" is a deterministic fixture (a single pure-Python
// module written to its own directory), not a real host-installed pip
// package: this isolates the test from whatever happens to be pip-installed
// on the dev/CI host and proves the actual mechanism under test — mount +
// PYTHONPATH makes a host-provided directory importable inside the sandbox.
// resolveHostPythonSitePackages' real sys.path auto-detection is covered by
// the unit tests in deps_test.go (TestPythonResolveHostUnderBwrap uses the
// same DepOptions.hostPythonSitePackages test seam this test uses).
func TestBwrapPythonHostDepsOffline(t *testing.T) {
	if ok, reason := DetectBwrap(); !ok {
		t.Skipf("bwrap unavailable: %s", reason)
	}

	tc, err := ResolveHostToolchains([]string{"python3"})
	if err != nil || len(tc.Mounts) == 0 {
		t.Skipf("no host python3 toolchain resolvable on PATH: %v", err)
	}

	bw, err := NewBwrap(
		WithBwrapCPUs(1),
		WithBwrapMemoryMB(256),
		WithBwrapPidsLimit(64),
		WithBwrapTimeout(30*time.Second),
	)
	if err != nil {
		t.Skipf("NewBwrap: %v", err)
	}
	t.Cleanup(func() { _ = bw.Close() })

	sitePkgs := t.TempDir()
	mustWrite(t, filepath.Join(sitePkgs, "bugbot_fixture_dep.py"),
		"GREETING = \"hello from host site-packages\"\n", 0o644)

	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "requirements.txt"), "bugbot-fixture-dep\n") // detector only
	writeFile(t, filepath.Join(repo, "check_dep.py"), `import bugbot_fixture_dep
assert bugbot_fixture_dep.GREETING == "hello from host site-packages"
print("OK")
`)

	res, err := ResolveDeps(repo, DepOptions{
		Strategy:               DepStrategyHost,
		FetchSandbox:           bw,
		HostToolchains:         []string{"python3"},
		hostPythonSitePackages: []string{sitePkgs},
	})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	if res.Strategy != DepStrategyHost {
		t.Fatalf("Strategy = %q, want host", res.Strategy)
	}

	out, err := bw.Exec(context.Background(), Spec{
		RepoDir:  repo,
		Cmd:      []string{"python3", "check_dep.py"},
		Network:  "none",
		ROMounts: res.ROMounts,
		Env:      res.Env,
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("bwrap Python dep_strategy:host run should pass under network=none; exit=%d\nstdout:\n%s\nstderr:\n%s", out.ExitCode, out.Stdout, out.Stderr)
	}
	if !strings.Contains(out.Stdout, "OK") {
		t.Errorf("stdout = %q, want it to contain %q", out.Stdout, "OK")
	}
}

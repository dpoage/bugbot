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

	res := modcacheResolution(cache, DepStrategyHost, nil)

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

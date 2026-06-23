//go:build integration

// End-to-end test for `bugbot sandbox build --run`. It exercises the real
// orchestration — vendor -> container build -> offline warm run (network=none)
// -> commit — against a minimal Bazel workspace. Run with:
//
//	go test -tags integration ./internal/cli/...
//
// It skips automatically when a prerequisite is missing (no container runtime,
// no bazel/bazelisk, no resolvable repository cache) rather than failing: the
// recipe needs a real toolchain on the host, mirroring
// internal/sandbox/*_integration_test.go.
package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/sandbox"
)

func TestSandboxBuildRun_EndToEnd(t *testing.T) {
	rt, ok := sandbox.Detect()
	if !ok {
		t.Skip("no container runtime detected; skipping sandbox build integration test")
	}
	bazelBin, err := exec.LookPath("bazelisk")
	if err != nil {
		if bazelBin, err = exec.LookPath("bazel"); err != nil {
			t.Skip("no bazel/bazelisk on PATH; skipping sandbox build integration test")
		}
	}

	// Minimal Bazel workspace: a single native sh_test, no external deps, so it
	// builds + tests fully offline once the image is warmed.
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "MODULE.bazel"), "module(name = \"sandbox_e2e\", version = \"0.0.0\")\n")
	writeFile(t, filepath.Join(repo, ".bazelversion"), "8.4.2\n")
	writeFile(t, filepath.Join(repo, "pass.sh"), "#!/bin/sh\nexit 0\n")
	if chErr := os.Chmod(filepath.Join(repo, "pass.sh"), 0o755); chErr != nil {
		t.Fatal(chErr)
	}
	writeFile(t, filepath.Join(repo, "BUILD.bazel"),
		"sh_test(\n    name = \"pass_test\",\n    srcs = [\"pass.sh\"],\n)\n")

	// Resolve and pin the host repository cache so build.sh's existence check
	// passes for a no-dependency repo (an empty cache is fine — nothing to bake).
	repoCache := resolveRepoCache(t, bazelBin, repo)
	t.Setenv("REPO_CACHE", repoCache)

	image := "localhost/sandbox-e2e-bugbot-sandbox:latest"
	t.Cleanup(func() { _ = exec.Command(rt, "rmi", "-f", image).Run() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cmd := newSandboxBuildCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--target", repo, "--image", image, "--run"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("sandbox build --run: %v\noutput:\n%s", err, buf.String())
	}

	// The final image must exist after a successful warm + commit.
	if err := exec.Command(rt, "image", "inspect", image).Run(); err != nil {
		t.Fatalf("expected committed image %s to exist: %v\noutput:\n%s", image, err, buf.String())
	}
}

func resolveRepoCache(t *testing.T, bazelBin, repo string) string {
	t.Helper()
	out, err := exec.Command(bazelBin, "info", "repository_cache").Output()
	if err != nil {
		t.Skipf("cannot resolve repository_cache via %s: %v", bazelBin, err)
	}
	path := string(bytes.TrimSpace(out))
	if path == "" {
		t.Skip("empty repository_cache path; skipping")
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Skipf("cannot create repository_cache %s: %v", path, err)
	}
	return path
}

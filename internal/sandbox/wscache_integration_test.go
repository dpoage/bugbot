//go:build integration

// Workspace-cache integration test proves the pristine-materialization cache
// (wsCache) actually shortcuts repeated Execs against the same repo state
// through a real container runtime, and correctly invalidates on a new
// commit. Run with:
//
//	go test -tags integration -run TestWorkspaceCacheHitAcrossExecs ./internal/sandbox/
//
// Requires a container runtime; skipped automatically when none is found.
package sandbox

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestWorkspaceCacheHitAcrossExecs runs two sequential Execs against the same
// git repo: the first must materialize the pristine (WorkspaceCacheHit ==
// false), the second must reuse it (WorkspaceCacheHit == true). Committing a
// change in the fixture and running a third Exec must invalidate the cache
// (WorkspaceCacheHit == false again).
func TestWorkspaceCacheHitAcrossExecs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	runtime, ok := Detect()
	if !ok {
		t.Skip("no container runtime (podman/docker) found on PATH")
	}
	const image = "docker.io/library/alpine:latest"
	cli, err := NewCLI(runtime, image)
	if err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}
	defer func() { _ = cli.Close() }()

	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "a.txt"), "v1\n", 0o644)
	gitInit(t, repo)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	spec := Spec{
		RepoDir: repo,
		Cmd:     []string{"cat", "a.txt"},
	}

	res1, err := cli.Exec(ctx, spec)
	if err != nil {
		t.Fatalf("Exec #1: %v", err)
	}
	if res1.WorkspaceCacheHit {
		t.Errorf("Exec #1: WorkspaceCacheHit = true, want false (first materialization)")
	}

	res2, err := cli.Exec(ctx, spec)
	if err != nil {
		t.Fatalf("Exec #2: %v", err)
	}
	if !res2.WorkspaceCacheHit {
		t.Errorf("Exec #2: WorkspaceCacheHit = false, want true (repo unchanged)")
	}

	mustWrite(t, filepath.Join(repo, "a.txt"), "v2\n", 0o644)
	gitCommitAll(t, repo, "second")

	res3, err := cli.Exec(ctx, spec)
	if err != nil {
		t.Fatalf("Exec #3: %v", err)
	}
	if res3.WorkspaceCacheHit {
		t.Errorf("Exec #3: WorkspaceCacheHit = true, want false (repo changed since #1/#2)")
	}
	if res3.ExitCode != 0 {
		t.Fatalf("Exec #3: exit code = %d, stderr = %s", res3.ExitCode, res3.Stderr)
	}
	if res3.Stdout != "v2\n" {
		t.Errorf("Exec #3: stdout = %q, want %q (post-commit content)", res3.Stdout, "v2\n")
	}
}

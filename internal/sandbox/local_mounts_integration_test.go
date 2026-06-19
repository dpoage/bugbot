//go:build integration

// Local-mounts integration test proves that a package whose dependency lives
// in a mounted sibling directory builds and tests offline when that sibling is
// exposed via DepOptions.LocalMounts. Run with:
//
//	go test -tags integration ./internal/sandbox/...
//
// Requires a container runtime with a Go toolchain image. Skipped automatically
// when no runtime is detected or the image cannot be used.
package sandbox

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestLocalMountsSiblingDepBuildsOffline verifies the composable local-mount
// dep layer (bugbot-ixu): a Go module that path-replaces a dependency with a
// sibling on disk builds and tests correctly in a network-none sandbox when
// the sibling is mounted via ResolveDeps LocalMounts.
//
// Layout:
//
//	<tmpdir>/
//	  sibling/           <- the "local dep" (a tiny Go library)
//	    go.mod
//	    lib.go
//	  repo/              <- the "scanned repo" that uses the sibling
//	    go.mod           (replace directive: ../sibling)
//	    main_test.go     (imports and tests the sibling lib)
func TestLocalMountsSiblingDepBuildsOffline(t *testing.T) {
	cli := newGoTestCLI(t)

	root := t.TempDir()

	// --- sibling library -------------------------------------------------------
	siblingDir := filepath.Join(root, "sibling")
	mustMkdir(t, siblingDir)
	writeFile(t, filepath.Join(siblingDir, "go.mod"), "module example.com/sibling\n\ngo 1.21\n")
	writeFile(t, filepath.Join(siblingDir, "lib.go"), `package sibling

// Answer returns the answer to life, the universe and everything.
func Answer() int { return 42 }
`)

	// --- repo that uses the sibling --------------------------------------------
	repoDir := filepath.Join(root, "repo")
	mustMkdir(t, repoDir)
	writeFile(t, filepath.Join(repoDir, "go.mod"), `module example.com/repo

go 1.21

require example.com/sibling v0.0.0

replace example.com/sibling => /sibling
`)
	writeFile(t, filepath.Join(repoDir, "main_test.go"), `package main_test

import (
	"testing"
	"example.com/sibling"
)

func TestAnswer(t *testing.T) {
	if got := sibling.Answer(); got != 42 {
		t.Fatalf("Answer() = %d, want 42", got)
	}
}
`)

	// The container path for the sibling matches the replace directive above.
	siblingMount := ROMount{
		HostPath:      siblingDir,
		ContainerPath: "/sibling",
		Shared:        true,
	}

	// ResolveDeps with off strategy + local mount: the repo is not vendored and
	// has no external registry deps, but the sibling dep is locally mounted.
	res, err := ResolveDeps(repoDir, DepOptions{
		Strategy:    DepStrategyOff,
		LocalMounts: []ROMount{siblingMount},
	})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}

	// Verify the local mount is in the resolution.
	found := false
	for _, m := range res.ROMounts {
		if m.ContainerPath == "/sibling" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sibling mount missing from resolution; mounts=%+v", res.ROMounts)
	}

	// Run `go test ./...` in the repo inside a network-none sandbox.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := cli.Exec(ctx, Spec{
		RepoDir:   repoDir,
		Cmd:       []string{"go", "test", "./..."},
		Env:       res.Env,
		ROMounts:  res.ROMounts,
		SetupCmds: res.SetupCmds,
		Network:   "none",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("go test failed (exit %d):\nstdout: %s\nstderr: %s",
			result.ExitCode, result.Stdout, result.Stderr)
	}
}

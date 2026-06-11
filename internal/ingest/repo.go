// Package ingest builds a navigable model of a target git repository for
// Bugbot's incremental scanning pipeline. It is responsible for:
//
//   - Repo model: opening a repository and snapshotting the tracked-file
//     inventory at HEAD ([Repo.Open], [Repo.Snapshot]).
//   - Fingerprinting: per-file SHA-256 content hashes and the HEAD commit, which
//     feed the store's file_state watermarks ([Repo.Fingerprints],
//     [Repo.HeadCommit]).
//   - Change detection: the set of files changed between two commits
//     ([Repo.ChangedFiles]).
//   - Polling: a single-shot primitive that detects new commits, for both
//     remote-backed and local-only repositories ([Poller.Poll]).
//   - Blast radius: changed files plus their direct dependents, used to scope
//     targeted investigations ([Repo.BlastRadius]).
//
// All git interaction shells out to the `git` CLI via os/exec rather than
// linking a git library; this keeps the package dependency-free and matches
// real-world git semantics (gitignore, rename detection, etc.) exactly.
//
// This package deliberately does NOT import internal/store. Methods return
// plain maps and structs; wiring into the store's watermarks happens later in
// internal/funnel.
package ingest

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// queryRunner executes an external command in dir with the supplied args and
// returns the combined stdout bytes. It is a function type so that tests can
// inject fake tool output without needing a real bazel installation.
type queryRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// Repo is a handle to an opened git repository. It is safe for concurrent use:
// it holds only the immutable repository root path and runs git as a child
// process per call.
//
// The queryRunner field is exported to tests via the Open/newTestRepo path only:
// production code always uses the default execQueryRunner.
type Repo struct {
	root        string
	queryRunner queryRunner // defaults to execQueryRunner
}

// Open validates that path is inside a git working tree and returns a Repo
// rooted at the repository's top-level directory. It returns an error if git is
// unavailable or path is not part of a git repository.
func Open(ctx context.Context, path string) (*Repo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("ingest: resolve repo path %q: %w", path, err)
	}

	// `rev-parse --show-toplevel` both validates that we are in a work tree and
	// gives us the canonical root to anchor every subsequent command at.
	out, err := runGit(ctx, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("ingest: %q is not a git repository: %w", path, err)
	}
	root := strings.TrimSpace(out)
	if root == "" {
		return nil, fmt.Errorf("ingest: %q is not a git repository", path)
	}
	return &Repo{root: root, queryRunner: execQueryRunner}, nil
}

// Root returns the absolute path to the repository's top-level directory.
func (r *Repo) Root() string { return r.root }

// runGit executes `git -C dir args...` and returns its stdout. On failure the
// returned error includes the trimmed stderr, which is far more useful than the
// bare exit status when diagnosing problems.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	// -c protocol guards and an empty config env are unnecessary here: we never
	// commit and only read. We do force a clean, predictable invocation.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

// runGitRaw is like runGit but returns raw bytes, for commands whose output is
// NUL-delimited or otherwise not safe to treat as a UTF-8 string with trimming.
func runGitRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout strings.Builder
	var stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return []byte(stdout.String()), nil
}

// execQueryRunner is the production queryRunner implementation. It runs the
// named tool with the given args in dir and returns its combined stdout+stderr.
// Callers are responsible for applying a context deadline before invoking.
func execQueryRunner(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("execQueryRunner: no args")
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", args[0], err, strings.TrimSpace(out.String()))
	}
	return out.Bytes(), nil
}

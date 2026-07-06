package agent

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// errPathEscape is returned when a requested path resolves outside the root.
var errPathEscape = errors.New("path escapes the repository root")

// fsRoot anchors the built-in code tools at a single repository directory and
// resolves model-supplied, repo-relative paths to absolute on-disk paths while
// guaranteeing they cannot escape the root — including via "..", absolute
// inputs, or symlinks that point outside the tree.
type fsRoot struct {
	// root is the cleaned, symlink-resolved absolute repository root. All
	// resolved paths must remain within it.
	root string
}

// newFSRoot resolves dir to a canonical absolute root. It follows symlinks on
// the root itself (so the containment check compares like with like) but does
// not require dir to exist as a directory beyond being resolvable.
func newFSRoot(dir string) (*fsRoot, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve root %q: %w", dir, err)
	}
	// Canonicalize the root through any symlinks so containment comparisons are
	// against the real path. If the root itself can't be resolved (e.g. doesn't
	// exist), fall back to the cleaned absolute path — tool calls will then fail
	// naturally on access.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return &fsRoot{root: filepath.Clean(abs)}, nil
}

// resolve maps a model-supplied, repo-relative path to an absolute on-disk path
// inside the root, rejecting absolute inputs and any path that escapes the root
// either lexically ("..") or after symlink resolution.
//
// An empty path resolves to the root itself (useful for list_dir of the repo
// root).
func (r *fsRoot) resolve(rel string) (string, error) {
	// Normalize separators so callers may use forward slashes regardless of OS,
	// matching the repo-relative, slash-normalized convention used elsewhere.
	rel = filepath.FromSlash(rel)

	// Reject absolute inputs outright: tools take repo-relative paths only.
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: absolute paths are not allowed (%q)", errPathEscape, rel)
	}

	joined := filepath.Join(r.root, rel)
	cleaned := filepath.Clean(joined)

	// Lexical containment check: cleaned must be the root or under root+sep.
	if !r.contains(cleaned) {
		return "", fmt.Errorf("%w: %q", errPathEscape, rel)
	}

	// Symlink containment check: resolve the longest existing prefix of the path
	// and ensure it still lands inside the root. This defeats symlinks that point
	// outside the tree. Non-existent tail components are fine (the access will
	// fail naturally); we only validate what exists.
	if resolved, err := r.evalExistingPrefix(cleaned); err == nil {
		if !r.contains(resolved) {
			return "", fmt.Errorf("%w: %q resolves outside the root via symlink", errPathEscape, rel)
		}
	}

	return cleaned, nil
}

// contains reports whether p is the root itself or lies beneath it.
func (r *fsRoot) contains(p string) bool {
	if p == r.root {
		return true
	}
	return strings.HasPrefix(p, r.root+string(filepath.Separator))
}

// evalExistingPrefix delegates to the package-level evalExistingPrefixPath so
// both the in-repository and dep-source traversal guards share one
// implementation (see pathutil.go).
func (r *fsRoot) evalExistingPrefix(p string) (string, error) {
	return evalExistingPrefixPath(p)
}

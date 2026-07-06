package agent

import "path/filepath"

// evalExistingPrefixPath resolves symlinks on the longest prefix of p that
// exists on disk, then re-appends the non-existent tail. This lets callers
// validate containment even when the final path component does not yet exist,
// while still catching a symlinked intermediate directory that escapes a root.
//
// It is the single authoritative implementation used by both fsRoot
// (in-repository traversal guard) and DepSourceRoots (dep-source traversal
// guard). Keeping one copy ensures any hardening applied to this path hits
// both security boundaries simultaneously.
func evalExistingPrefixPath(p string) (string, error) {
	// Walk from the full path up toward the filesystem root, finding the longest
	// prefix that EvalSymlinks can resolve.
	tail := ""
	cur := p
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if tail == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, tail), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root without resolving anything.
			return p, nil
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

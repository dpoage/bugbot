package ingest

import "strings"

// matchGlob reports whether the slash-separated path matches the glob pattern.
//
// Supported syntax (a practical subset of gitignore/doublestar globbing,
// sufficient for the config's scan.include/exclude lists):
//
//   - `?`      matches any single non-separator character
//   - `*`      matches any run of non-separator characters (including empty)
//   - `**`     matches any number of path segments, including zero; e.g.
//     `a/**/b` matches `a/b`, `a/x/b`, `a/x/y/b`. A leading `**/`
//     also matches with zero leading segments, so `**/*.go` matches
//     `main.go` as well as `cmd/main.go`.
//   - character classes `[...]` are NOT supported and are matched literally.
//
// Both pattern and path are treated as forward-slash separated; callers must
// pass repo-relative, slash-normalized paths.
func matchGlob(pattern, path string) bool {
	return matchSegments(splitSlash(pattern), splitSlash(path))
}

// splitSlash splits on '/', dropping a single leading empty segment so that an
// absolute-looking pattern behaves the same as a relative one. Empty input
// yields a single empty segment, which matches an empty path.
func splitSlash(s string) []string {
	if s == "" {
		return []string{""}
	}
	parts := strings.Split(s, "/")
	return parts
}

// matchSegments matches pattern segments against path segments, handling `**`
// as a multi-segment wildcard via recursion/backtracking.
func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Collapse consecutive `**`.
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			// `**` is the last segment: matches everything that remains,
			// including zero segments.
			if len(pat) == 1 {
				return true
			}
			rest := pat[1:]
			// Try to consume 0..len(name) leading segments with `**`.
			for i := 0; i <= len(name); i++ {
				if matchSegments(rest, name[i:]) {
					return true
				}
			}
			return false
		}

		if len(name) == 0 {
			return false
		}
		if !matchSegment(pat[0], name[0]) {
			return false
		}
		pat = pat[1:]
		name = name[1:]
	}
	return len(name) == 0
}

// matchSegment matches a single path segment against a single pattern segment
// containing `*` and `?` wildcards (no separators). Implemented with iterative
// backtracking to avoid pathological recursion on long `*` runs.
func matchSegment(pat, name string) bool {
	var (
		px, nx         int
		starPx, starNx = -1, -1
	)
	for nx < len(name) {
		if px < len(pat) {
			switch pat[px] {
			case '?':
				px++
				nx++
				continue
			case '*':
				// Record backtrack point; tentatively match zero chars.
				starPx, starNx = px, nx
				px++
				continue
			default:
				if pat[px] == name[nx] {
					px++
					nx++
					continue
				}
			}
		}
		// Mismatch: backtrack to the last '*' and have it consume one more char.
		if starPx >= 0 {
			px = starPx + 1
			starNx++
			nx = starNx
			continue
		}
		return false
	}
	// Consume any trailing '*' in the pattern.
	for px < len(pat) && pat[px] == '*' {
		px++
	}
	return px == len(pat)
}

// matchAny reports whether path matches any of the patterns.
func matchAny(patterns []string, path string) bool {
	for _, p := range patterns {
		if matchGlob(p, path) {
			return true
		}
	}
	return false
}

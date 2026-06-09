package agent

import "strings"

// matchGlob reports whether the slash-separated, repo-relative path matches the
// glob pattern. It supports the same practical subset used elsewhere in Bugbot:
//
//   - `?`  matches any single non-separator character
//   - `*`  matches any run of non-separator characters (including empty)
//   - `**` matches any number of path segments, including zero; e.g. `**/*.go`
//     matches both `main.go` and `cmd/app/main.go`.
//
// Character classes are not supported and match literally. Both pattern and
// path are treated as forward-slash separated.
func matchGlob(pattern, path string) bool {
	return matchSegments(splitSlash(pattern), splitSlash(path))
}

// splitSlash splits on '/'. Empty input yields a single empty segment, which
// matches an empty path.
func splitSlash(s string) []string {
	if s == "" {
		return []string{""}
	}
	return strings.Split(s, "/")
}

// matchSegments matches pattern segments against path segments, treating `**` as
// a multi-segment wildcard via backtracking.
func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 1 {
				return true
			}
			rest := pat[1:]
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

// matchSegment matches a single path segment against a pattern segment with `*`
// and `?` wildcards (no separators), using iterative backtracking.
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
		if starPx >= 0 {
			px = starPx + 1
			starNx++
			nx = starNx
			continue
		}
		return false
	}
	for px < len(pat) && pat[px] == '*' {
		px++
	}
	return px == len(pat)
}

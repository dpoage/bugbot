package ingest

import (
	"sort"
	"strings"
)

// sortStrings sorts a string slice in place, ascending.
func sortStrings(s []string) { sort.Strings(s) }

// splitNULAll splits NUL-delimited output into all segments INCLUDING empty
// trailing fields removed, but preserving interior structure. Unlike splitNUL
// (which drops every empty segment), this only trims a single trailing empty
// produced by a terminating NUL, because name-status parsing relies on field
// positions.
func splitNULAll(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	parts := strings.Split(string(b), "\x00")
	// Drop a single trailing empty field from the terminating NUL.
	if n := len(parts); n > 0 && parts[n-1] == "" {
		parts = parts[:n-1]
	}
	return parts
}

// stringSet is a tiny ordered-on-demand set of strings.
type stringSet struct {
	m map[string]struct{}
}

func newStringSet() *stringSet { return &stringSet{m: map[string]struct{}{}} }

func (s *stringSet) add(v string) {
	if v != "" {
		s.m[v] = struct{}{}
	}
}

func (s *stringSet) has(v string) bool {
	_, ok := s.m[v]
	return ok
}

func (s *stringSet) len() int { return len(s.m) }

func (s *stringSet) sorted() []string {
	out := make([]string, 0, len(s.m))
	for v := range s.m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

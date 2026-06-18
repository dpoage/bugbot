package ingest

import (
	"sort"
	"testing"
)

// TestExtensionsForLanguage verifies that ExtensionsForLanguage returns the
// correct, sorted extensions for each known language and an empty slice for
// LangOther (which maps to no extension).
func TestExtensionsForLanguage(t *testing.T) {
	cases := []struct {
		lang     Language
		wantExts []string // must all appear; result may have more
		wantNone bool     // expect empty result
	}{
		{LangGo, []string{".go"}, false},
		{LangPython, []string{".py", ".pyi"}, false},
		{LangTypeScript, []string{".ts", ".tsx", ".mts", ".cts"}, false},
		{LangJavaScript, []string{".js", ".mjs", ".cjs", ".jsx"}, false},
		{LangRust, []string{".rs"}, false},
		{LangC, []string{".c", ".h"}, false},
		{LangCPP, []string{".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx"}, false},
		{LangElixir, []string{".ex", ".exs"}, false},
		{LangOther, nil, true},
	}

	for _, tc := range cases {
		exts := ExtensionsForLanguage(tc.lang)

		if tc.wantNone {
			if len(exts) != 0 {
				t.Errorf("ExtensionsForLanguage(%s): want empty, got %v", tc.lang, exts)
			}
			continue
		}

		if len(exts) == 0 {
			t.Errorf("ExtensionsForLanguage(%s): want non-empty, got empty", tc.lang)
			continue
		}

		// Result must be sorted.
		if !sort.StringsAreSorted(exts) {
			t.Errorf("ExtensionsForLanguage(%s): result not sorted: %v", tc.lang, exts)
		}

		// All expected extensions must be present.
		set := make(map[string]bool, len(exts))
		for _, e := range exts {
			set[e] = true
		}
		for _, want := range tc.wantExts {
			if !set[want] {
				t.Errorf("ExtensionsForLanguage(%s): missing %q in %v", tc.lang, want, exts)
			}
		}

		// Round-trip: each returned extension must map back to lang.
		for _, ext := range exts {
			if got := extLang[ext]; got != tc.lang {
				t.Errorf("ExtensionsForLanguage(%s): ext %q maps to %s, not %s", tc.lang, ext, got, tc.lang)
			}
		}
	}
}

// TestDetectLanguage pins the extension→Language mapping for a sample of paths
// per language, including extensionless/upper-case/relative variants. New
// languages get a row here so a missing entry fails loudly.
func TestDetectLanguage(t *testing.T) {
	cases := []struct {
		path string
		want Language
	}{
		{"foo.go", LangGo},
		{"foo.py", LangPython},
		{"foo.PY", LangPython}, // case-insensitive
		{"a/b/c.ts", LangTypeScript},
		{"x.ex", LangElixir},
		{"a/b/c.exs", LangElixir},
		{"foo.rs", LangRust},
		{"foo", LangOther},         // extensionless
		{"foo.unknown", LangOther}, // unknown extension
	}
	for _, tc := range cases {
		if got := DetectLanguage(tc.path); got != tc.want {
			t.Errorf("DetectLanguage(%q) = %s, want %s", tc.path, got, tc.want)
		}
	}
}

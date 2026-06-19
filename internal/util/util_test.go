package util

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestShortSHA(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"shorter-than-12", "abcdef", "abcdef"},
		{"exactly-12", "abcdef123456", "abcdef123456"},
		{"longer-than-12-truncates", "abcdef1234567890", "abcdef123456"},
		{"full-40-char-sha", "abcdef1234567890abcdef1234567890abcdef12", "abcdef123456"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShortSHA(tc.in); got != tc.want {
				t.Errorf("ShortSHA(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"empty", "", 12, ""},
		{"shorter", "abc", 12, "abc"},
		{"equal", "abcdefghijkl", 12, "abcdefghijkl"},
		{"longer", "abcdefghijklmnop", 12, "abcdefghijkl"},
		{"zero-clamps-to-all", "abc", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Truncate(tc.in, tc.n); got != tc.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

func TestSortedKeys(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got := SortedKeys(map[string]int{})
		if len(got) != 0 {
			t.Errorf("SortedKeys(empty) = %v, want []", got)
		}
	})

	t.Run("ordering", func(t *testing.T) {
		m := map[string]int{
			"delta":   3,
			"alpha":   1,
			"charlie": 2,
			"bravo":   4,
		}
		got := SortedKeys(m)
		want := []string{"alpha", "bravo", "charlie", "delta"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("SortedKeys = %v, want %v", got, want)
		}
	})

	t.Run("generic-typed-values", func(t *testing.T) {
		m := map[string]struct{ X int }{
			"b": {X: 2},
			"a": {X: 1},
		}
		got := SortedKeys(m)
		want := []string{"a", "b"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("SortedKeys(generic) = %v, want %v", got, want)
		}
	})

	t.Run("string-valued", func(t *testing.T) {
		// The repro/artifacts.go caller uses map[string]string specifically;
		// exercise the same shape here so the call site is type-checked.
		m := map[string]string{
			"zzz": "last",
			"aaa": "first",
			"mmm": "middle",
		}
		got := SortedKeys(m)
		want := []string{"aaa", "mmm", "zzz"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("SortedKeys(string) = %v, want %v", got, want)
		}
	})
}

func TestCollapseWhitespace(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"only-spaces", "    ", ""},
		{"only-newlines", "\n\n\n", ""},
		{"only-tabs", "\t\t", ""},
		{"leading-and-trailing", "   hello   ", "hello"},
		{"mixed-runs-collapse-to-single-space", "a   b\t\tc\n\nd", "a b c d"},
		{"carriage-return", "a\r\nb", "a b"},
		{"preserves-words", "the quick brown fox", "the quick brown fox"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CollapseWhitespace(tc.in); got != tc.want {
				t.Errorf("CollapseWhitespace(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	t.Run("ascii-shorter", func(t *testing.T) {
		if got := TruncateRunes("hello", 10); got != "hello" {
			t.Errorf("TruncateRunes(%q, 10) = %q, want %q", "hello", got, "hello")
		}
	})

	t.Run("ascii-equal", func(t *testing.T) {
		if got := TruncateRunes("hello", 5); got != "hello" {
			t.Errorf("TruncateRunes(%q, 5) = %q, want %q", "hello", got, "hello")
		}
	})

	t.Run("ascii-truncates-with-ellipsis", func(t *testing.T) {
		got := TruncateRunes("hello world", 5)
		want := "hello…"
		if got != want {
			t.Errorf("TruncateRunes(%q, 5) = %q, want %q", "hello world", got, want)
		}
	})

	t.Run("multibyte-rune-boundary", func(t *testing.T) {
		// 锁 is 3 bytes per rune in UTF-8. 50 runes truncated to 10 must
		// produce 10 runes + ellipsis (11 runes) and remain valid UTF-8.
		in := strings.Repeat("锁", 50)
		got := TruncateRunes(in, 10)
		if !utf8.ValidString(got) {
			t.Fatalf("TruncateRunes produced invalid UTF-8: %q", got)
		}
		r := []rune(got)
		if len(r) != 11 {
			t.Errorf("rune length = %d, want 11 (10 + ellipsis)", len(r))
		}
	})

	t.Run("multibyte-shorter-than-max", func(t *testing.T) {
		in := strings.Repeat("锁", 5) // 15 bytes, 5 runes
		got := TruncateRunes(in, 10)
		if got != in {
			t.Errorf("TruncateRunes shorter-than-max = %q, want %q", got, in)
		}
	})

	t.Run("empty-string", func(t *testing.T) {
		if got := TruncateRunes("", 5); got != "" {
			t.Errorf("TruncateRunes(\"\", 5) = %q, want \"\"", got)
		}
	})
}

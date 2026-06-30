package funnel

import (
	"strings"
	"testing"
)

// TestNormalizeSummary pins the deterministic post-processor that runs on every
// model-produced package summary. The cartographer's historical model
// nondeterminism (free-form prompt + permissive schema) let summaries open
// with markdown headings, bold labels, or any preamble; the injected
// "REPO CONTEXT — package summaries" finder block was therefore noisy and
// non-uniform. normalizeSummary is the byte-uniform guarantee: regardless of
// what the model emits, the persisted/returned summary is a single bounded
// paragraph. This test pins the contract so any future regression in the
// normalizer is caught at unit-test time, not by a noisy injection block
// downstream.
//
// The table covers the exact shapes the bead captured from the live corpus:
//   - "# Package Summary\n\nFoo bar."
//   - "## Summary\nFoo."
//   - "# Package: x\nFoo."
//   - "**Purpose:** Foo."
//   - "**Purpose**: Foo."
//   - a multi-line body (asserts the result is one line)
//   - an over-cap body (asserts the word cap + ellipsis)
//
// plus a few extra guards (leading whitespace, multiple lines, multibyte
// text, the "…" token vs a trailing sentence that legitimately ends in a
// period).
func TestNormalizeSummary(t *testing.T) {
	// Build a 130-word body to exercise the word cap.
	longBody := strings.Repeat("word ", 130)
	longBody = strings.TrimRight(longBody, " ")

	cases := []struct {
		name    string
		in      string
		want    string // expected exact output
		wantCap int    // expected maximum word count (0 -> no cap check)
	}{
		{
			name: "h1 with Package Summary label",
			in:   "# Package Summary\n\nFoo bar.",
			want: "Foo bar.",
		},
		{
			name: "h2 with Summary label",
			in:   "## Summary\nFoo.",
			want: "Foo.",
		},
		{
			name: "h1 with Package colon name",
			in:   "# Package: x\nFoo.",
			want: "Foo.",
		},
		{
			name: "bold Purpose colon",
			in:   "**Purpose:** Foo.",
			want: "Foo.",
		},
		{
			name: "bold Purpose colon outside bold",
			in:   "**Purpose**: Foo.",
			want: "Foo.",
		},
		{
			name: "bold Package Purpose colon",
			in:   "**Package Purpose:** Bar baz.",
			want: "Bar baz.",
		},
		{
			name: "multi-line body collapses to one line",
			in:   "Foo\n\nbar\nbaz qux.",
			want: "Foo bar baz qux.",
		},
		{
			name: "leading whitespace trimmed",
			in:   "   \n\nHello world.",
			want: "Hello world.",
		},
		{
			name: "h1 then bold label then body",
			in:   "# Package Summary\n**Purpose:** Hello world.",
			want: "Hello world.",
		},
		{
			name: "preserves multibyte CJK without splitting runes",
			in:   "# Summary\n中文 测试 内容 在 这里。",
			want: "中文 测试 内容 在 这里。",
		},
		{
			name: "plain text unchanged",
			in:   "Just a plain summary sentence.",
			want: "Just a plain summary sentence.",
		},
		{
			name:    "over-cap body is truncated to cap plus ellipsis",
			in:      longBody,
			wantCap: cartographySummaryMaxWords + 1, // 120 words + 1 "…" token
		},
		{
			name: "stacked leading headings are all stripped",
			in:   "# Package Summary\n\n## Purpose\nFoo bar.",
			want: "Foo bar.",
		},
		{
			name: "h1 then h2 then body",
			in:   "# Title\n## Subtitle\nBody here.",
			want: "Body here.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeSummary(tc.in)
			if tc.want != "" && got != tc.want {
				t.Errorf("normalizeSummary(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Universal invariants: no leading '#' (heading), no leading
			// '*' (bold), and no embedded newline.
			if got != "" {
				if strings.HasPrefix(got, "#") {
					t.Errorf("normalizeSummary output still begins with '#': %q", got)
				}
				if strings.HasPrefix(got, "*") {
					t.Errorf("normalizeSummary output still begins with '*': %q", got)
				}
				if strings.Contains(got, "\n") {
					t.Errorf("normalizeSummary output contains a newline: %q", got)
				}
			}
			// Per-case word-count assertion.
			if tc.wantCap > 0 {
				if n := len(strings.Fields(got)); n > tc.wantCap {
					t.Errorf("normalizeSummary(%q) word count = %d, want <= %d (got %q)",
						tc.in, n, tc.wantCap, got)
				}
			}
		})
	}
}

// TestNormalizeSummary_OverCapStructure pins the EXACT shape of the truncated
// output: 120 words, a single space, then the single ellipsis character. This
// is what the rendered "REPO CONTEXT — package summaries" block relies on —
// a model that has historically been chatty must be cut to a uniform shape.
func TestNormalizeSummary_OverCapStructure(t *testing.T) {
	// 125 words: 5 must be dropped, 120 remain.
	in := strings.TrimRight(strings.Repeat("alpha ", 125), " ")
	got := normalizeSummary(in)
	words := strings.Fields(got)
	if len(words) != cartographySummaryMaxWords+1 {
		t.Fatalf("expected %d tokens (120 words + ellipsis), got %d: %q",
			cartographySummaryMaxWords+1, len(words), got)
	}
	if words[cartographySummaryMaxWords] != "…" {
		t.Errorf("expected the last token to be the ellipsis %q, got %q",
			"…", words[cartographySummaryMaxWords])
	}
	// And the first 120 tokens must be the input's first 120 tokens verbatim.
	for i := 0; i < cartographySummaryMaxWords; i++ {
		if words[i] != "alpha" {
			t.Errorf("token %d: got %q, want %q", i, words[i], "alpha")
		}
	}
}

// TestNormalizeSummary_AtCapNoTruncation pins the boundary: an exactly-cap
// length input must NOT be truncated (no trailing ellipsis).
func TestNormalizeSummary_AtCapNoTruncation(t *testing.T) {
	in := strings.TrimRight(strings.Repeat("beta ", cartographySummaryMaxWords), " ")
	got := normalizeSummary(in)
	if strings.Contains(got, "…") {
		t.Errorf("exactly-cap input must not be truncated, got %q", got)
	}
	if n := len(strings.Fields(got)); n != cartographySummaryMaxWords {
		t.Errorf("word count = %d, want %d", n, cartographySummaryMaxWords)
	}
}

// TestNormalizeSummary_EmptyAndHeadingOnly pins the empty-result contract.
// summarizePackage treats an empty normalizeSummary as an error and drops
// the package; both the empty string and a heading-only string must
// normalize to "" so that contract holds.
func TestNormalizeSummary_EmptyAndHeadingOnly(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"\n\n",
		"# Package Summary",
		"## Summary\n",
		"**Purpose:**",
		"**Purpose:**\n",
	}
	for _, in := range cases {
		if got := normalizeSummary(in); got != "" {
			t.Errorf("normalizeSummary(%q) = %q, want \"\"", in, got)
		}
	}
}

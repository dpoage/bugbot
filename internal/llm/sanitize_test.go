package llm

import (
	"strings"
	"testing"
)

func TestStripThinkBlocks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no think block",
			in:   `{"file":"a.go"}`,
			want: `{"file":"a.go"}`,
		},
		{
			name: "think before payload",
			in:   "<think>let me reason about this</think>\n{\"file\":\"a.go\"}",
			want: `{"file":"a.go"}`,
		},
		{
			name: "thinking tag variant",
			in:   "<thinking>reason</thinking>{\"x\":1}",
			want: `{"x":1}`,
		},
		{
			name: "case insensitive",
			in:   "<THINK>reason</Think>{\"x\":1}",
			want: `{"x":1}`,
		},
		{
			name: "multiline think",
			in:   "<think>\nline one\nline two\n</think>\n{\"x\":1}",
			want: `{"x":1}`,
		},
		{
			name: "multiple consecutive blocks",
			in:   "<think>a</think><think>b</think>\n{\"x\":1}",
			want: `{"x":1}`,
		},
		{
			name: "think before prose summary",
			in:   "<think>\nThe user wants a summary.\n</think>\n\n**Purpose:** does a thing.",
			want: "**Purpose:** does a thing.",
		},
		{
			name: "unclosed trailing think (truncation)",
			in:   "<think>truncated reasoning with no close",
			want: ``,
		},
		{
			name: "closed block then unclosed truncation",
			in:   "<think>first</think><think>truncated",
			want: ``,
		},
		{
			name: "literal <think> inside body is preserved",
			in:   `{"note":"contains <think> literally","x":1}`,
			want: `{"note":"contains <think> literally","x":1}`,
		},
		{
			name: "embedded closing tag inside body not over-stripped",
			in:   "<think>r</think>{\"note\":\"a </think> b\"}",
			want: `{"note":"a </think> b"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.TrimSpace(StripThinkBlocks(tc.in)); got != tc.want {
				t.Errorf("StripThinkBlocks(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

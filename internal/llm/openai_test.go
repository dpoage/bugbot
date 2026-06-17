package llm

import (
	"testing"

	"github.com/openai/openai-go/v3"
)

func TestStripThinkBlocks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single think block before json",
			in:   "<think>reasoning</think>\n{\"x\":1}",
			want: `{"x":1}`,
		},
		{
			name: "no tags unchanged",
			in:   `{"x":1}`,
			want: `{"x":1}`,
		},
		{
			name: "two think blocks both removed",
			in:   "<think>a</think>hello<think>b</think>world",
			want: "helloworld",
		},
		{
			name: "multiline think content",
			in:   "<think>line1\nline2\nline3</think>result",
			want: "result",
		},
		{
			name: "thinking spelling removed",
			in:   "<thinking>reasoning</thinking>answer",
			want: "answer",
		},
		{
			name: "unterminated think left intact",
			in:   "before<think>no close here",
			want: "before<think>no close here",
		},
		{
			name: "mixed case tags removed",
			in:   "<ThInK>x</ThInK>kept",
			want: "kept",
		},
		{
			name: "whitespace trimmed after strip",
			in:   "  \n<think>cot</think>\n\n  {\"k\":1}  \t\n",
			want: `{"k":1}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripThinkBlocks(tc.in); got != tc.want {
				t.Errorf("stripThinkBlocks(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestOpenAIToResponse_StripsThinkBlocks is the wiring assertion: the
// helper must run inside toResponse so reasoning-model output never reaches
// the parser with its CoT prefix attached.
func TestOpenAIToResponse_StripsThinkBlocks(t *testing.T) {
	cc := &openai.ChatCompletion{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Content: "<think>cot</think>{\"k\":1}",
			},
		}},
	}
	ad := &openaiAdapter{}
	resp := ad.toResponse(cc)
	if resp.Text != `{"k":1}` {
		t.Errorf("toResponse Text = %q, want %q", resp.Text, `{"k":1}`)
	}
}

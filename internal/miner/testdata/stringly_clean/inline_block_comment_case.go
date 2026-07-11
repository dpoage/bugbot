package stringly_clean

// Inline-block-comment case regression:
// A single-line /* inline */ comment on the same line as a case arm must NOT
// cause the case to be dropped. Zero leads expected.

type InlineBlockCommentMode string

const (
	InlineBlockCommentModeA InlineBlockCommentMode = "a"
	InlineBlockCommentModeB InlineBlockCommentMode = "b"
)

func runInlineBlockCommentCase(m InlineBlockCommentMode) {
	switch m {
	case "a": /* inline comment */
	case "b":
	}
}

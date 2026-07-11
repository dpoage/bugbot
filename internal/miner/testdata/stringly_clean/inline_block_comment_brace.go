package stringly_clean

// Inline-block-comment brace regression:
// A single-line /* inline */ comment inside an if-block on the same line as a
// case body must NOT desync the brace depth. Zero leads expected.

type InlineBlockCommentBraceMode string

const (
	InlineBlockCommentBraceModeA InlineBlockCommentBraceMode = "a"
	InlineBlockCommentBraceModeB InlineBlockCommentBraceMode = "b"
)

func runInlineBlockCommentBrace(m InlineBlockCommentBraceMode, cond bool) {
	switch m {
	case "a":
		if cond { /* inline comment */
		}
	case "b":
	}
}

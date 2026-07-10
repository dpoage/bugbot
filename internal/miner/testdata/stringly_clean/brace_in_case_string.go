package stringly_clean

// Brace-in-case-string regression (Finding 1):
// A `}` inside a string literal in a case body must not decrement braceDepth
// and pop the switch early. Both cases must be recognized; no false type-B.

type BraceMode string

const (
	BraceModeA BraceMode = "a"
	BraceModeB BraceMode = "b"
)

func handleBraceInString(m BraceMode) {
	switch m {
	case "a":
		s := "}" // `}` inside string must NOT close the switch
		_ = s
	case "b":
	}
}

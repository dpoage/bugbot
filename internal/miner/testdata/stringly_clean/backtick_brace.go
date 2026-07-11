package stringly_clean

// Backtick-brace regression:
// A `}` inside a single-line backtick raw string in a case body must not
// decrement braceDepth and pop the switch early. Both cases must be
// recognized; no false type-B lead.

type BacktickBraceMode string

const (
	BacktickBraceModeA BacktickBraceMode = "a"
	BacktickBraceModeB BacktickBraceMode = "b"
)

func handleBacktickBrace(m BacktickBraceMode) {
	switch m {
	case "a":
		s := `}` // `}` inside raw string must NOT close the switch
		_ = s
	case "b":
	}
}

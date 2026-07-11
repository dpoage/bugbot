package stringly_clean

// Rune-with-brace regression: a `}` inside a rune literal (`'}'`) must NOT
// be counted by the brace counter, which would pop the switch early. Both
// cases must be recognized; no false type-B lead for missing "b".

type RuneBraceMode string

const (
	RuneBraceModeA RuneBraceMode = "a"
	RuneBraceModeB RuneBraceMode = "b"
)

func handleRuneWithBrace(m RuneBraceMode) {
	switch m {
	case "a":
		var r rune
		if r == '}' {
			r = 0
		}
		_ = r
	case "b":
	}
}

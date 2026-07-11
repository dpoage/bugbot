package stringly_clean

// Rune-with-quote regression: a `"` inside a rune literal (`'"'`) must NOT
// open a fake double-quoted string that blanks the following `{`. Both cases
// must be recognized; no false type-B lead for missing "b".

type RuneQuoteMode string

const (
	RuneQuoteModeA RuneQuoteMode = "a"
	RuneQuoteModeB RuneQuoteMode = "b"
)

func handleRuneWithQuote(m RuneQuoteMode) {
	switch m {
	case "a":
		var c byte
		if c == '"' {
			c = 0
		}
		_ = c
	case "b":
	}
}

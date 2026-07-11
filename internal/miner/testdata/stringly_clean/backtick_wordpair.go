package stringly_clean

// Backtick-wordpair regression:
// A `cmd Mode` word-pair inside a single-line backtick raw string on a
// resolve line must NOT cause a raw-string scrutinee switch to resolve to
// the named enum type. Zero leads expected.

type BacktickMode string

const (
	BacktickModeRun  BacktickMode = "run"
	BacktickModeStop BacktickMode = "stop"
)

func runBacktickWordPair(cmd string) {
	msg := `cmd BacktickMode` // word-pair inside raw string must NOT resolve scrutinee
	_ = msg
	switch cmd {
	case "run":
	case "stop":
	}
}

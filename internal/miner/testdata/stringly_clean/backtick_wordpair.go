package stringly_clean

// Backtick-wordpair regression:
// A `cmd Mode` word-pair inside a single-line backtick raw string on a
// resolve line must NOT cause a raw-string scrutinee switch to resolve to
// the named enum type. Zero leads expected.

type BacktickMode string

const (
	BacktickModeRun   BacktickMode = "run"
	BacktickModeStop  BacktickMode = "stop"
	BacktickModePause BacktickMode = "pause" // uncovered: absent from switch to make test fail-before/pass-after
)

func runBacktickWordPair(cmd string) {
	msg := `cmd BacktickMode` // word-pair inside raw string must NOT resolve scrutinee
	_ = msg
	switch cmd {
	case "run":
	case "stop":
	}
}

// typedSwitch triggers a genuine type-B lead for the missing "pause" arm.
// Without backtick tracking, the word-pair above would ALSO falsely resolve
// this function's raw-string switch — producing extra false leads.
func dispatchBacktickMode(m BacktickMode) {
	switch m {
	case "run":
	case "stop":
		// "pause" deliberately missing to produce exactly 1 genuine lead
	}
}

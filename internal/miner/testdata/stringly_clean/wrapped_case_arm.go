package stringly_clean

// Wrapped-case-arm regression (Finding 2):
// gofmt preserves a case list split across two lines:
//
//	case "b",
//		"c":
//
// Both "b" and "c" must be recognized; no false type-B for either.

type WrappedMode string

const (
	WrappedModeA WrappedMode = "a"
	WrappedModeB WrappedMode = "b"
	WrappedModeC WrappedMode = "c"
)

func handleWrappedCase(m WrappedMode) {
	switch m {
	case "a":
	case "b",
		"c":
	}
}

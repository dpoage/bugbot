package stringly_clean

// Enum-type-in-comment regression (Finding 3):
// A `cmd CommentMode` word-pair that appears only inside a string literal or a
// trailing // comment must NOT cause the raw-string `cmd` scrutinee to resolve
// to the CommentMode enum type.
//
// If resolution falsely fires, the switch on a raw string would be analyzed as
// a typed-enum switch. With "pause" as an uncovered const, the miner would emit
// a false type-B lead. After the fix: 0 leads.

type CommentMode string

const (
	CommentModeRun   CommentMode = "run"
	CommentModeStop  CommentMode = "stop"
	CommentModePause CommentMode = "pause" // uncovered const — only flagged if resolution falsely fires
)

// logMode dispatches on a raw string cmd.
// The string literal on the next line contains "cmd CommentMode" — without
// sanitization, varDeclRe would match this pair and falsely resolve cmd to
// CommentMode, emitting a type-B lead for the uncovered "pause" const.
func logMode(cmd string) {
	_ = "cmd CommentMode: " + cmd // `cmd CommentMode` in string — must NOT resolve
	switch cmd {
	case "run":
	case "stop":
	}
}

// logMode2 demonstrates the same bug via a trailing comment.
func logMode2(cmd string) {
	_ = cmd // cmd CommentMode changed — must NOT resolve scrutinee to CommentMode
	switch cmd {
	case "run":
	case "stop":
	}
}

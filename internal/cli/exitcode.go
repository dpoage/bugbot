package cli

// Exit codes for the bugbot command:
//
//	0  normal completion — scan/review/doctor passed, no gate failures
//	1  crash or hard error — config invalid, store failed to open, internal error
//	2  gate failure — a CI gate check triggered (review found new verified findings,
//	   reliability failure, etc.) but the command itself ran correctly
//
// Code 2 lets CI scripts distinguish "bugbot found bugs" from "bugbot crashed"
// without parsing human-readable output. Machine outputs (JSON from scan/report/
// export) always go to stdout and are unaffected by exit code.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitGateFailure = 2
)

// GateError is returned by commands that implement a CI gate (e.g. `bugbot review`
// --fail-on=verified). main.go detects this type and exits with ExitGateFailure
// instead of the default ExitError, allowing CI to distinguish a gate trip from
// a crash or misconfiguration.
type GateError struct {
	msg string
}

func (e *GateError) Error() string { return e.msg }

// newGateError wraps msg in a GateError so main.go can assign exit code 2.
func newGateError(msg string) *GateError { return &GateError{msg: msg} }

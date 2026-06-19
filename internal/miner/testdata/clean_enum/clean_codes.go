// Package x provides mode constants.
package x

// Mode constants for operation mode selection.
const (
	ModeRead  = iota // 0: read-only
	ModeWrite        // 1: read-write
	ModeAdmin        // 2: administrative
)

// handleMode processes a mode value using named constants only.
func handleMode(mode int) string {
	switch mode {
	case ModeRead:
		return "read"
	case ModeWrite:
		return "write"
	case ModeAdmin:
		return "admin"
	default:
		return "unknown"
	}
}

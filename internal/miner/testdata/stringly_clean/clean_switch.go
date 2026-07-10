// Package x demonstrates a stringly-correct dispatch: every case matches a produced literal.
package x

// statusFromCode returns a status string given an integer code.
func statusFromCode(code int) string {
	switch code {
	case 1:
		return "active"
	case 2:
		return "inactive"
	default:
		return "pending"
	}
}

// handleStatus processes a status string.
// All cases exactly match the literals produced above.
func handleStatus(status string) string {
	switch status {
	case "active":
		return "do-active-thing"
	case "inactive":
		return "do-inactive-thing"
	case "pending":
		return "do-pending-thing"
	default:
		return "unhandled"
	}
}

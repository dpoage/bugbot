// Package x demonstrates a stringly-typed drift defect.
// The producer emits "active" but the consumer switch has a typo "activ".
package x

// statusFromCode returns a status string given an integer code.
func statusFromCode(code int) string {
	switch code {
	case 1:
		return "active" // producer: emits "active"
	case 2:
		return "inactive"
	default:
		return "pending"
	}
}

// handleStatus processes a status string.
// BUG: case "activ" is a typo; the producer never emits "activ".
func handleStatus(status string) string {
	switch status {
	case "activ": // typo — producer emits "active", not "activ"
		return "do-active-thing"
	case "inactive":
		return "do-inactive-thing"
	case "pending":
		return "do-pending-thing"
	default:
		return "unhandled"
	}
}

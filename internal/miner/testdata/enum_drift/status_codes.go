// Package x provides status code constants.
package x

// StatusCodes is an iota enum for HTTP-like status handling.
const (
	StatusPending  = iota // 0: request is pending
	StatusApproved        // 1: request approved
	StatusRejected        // 2: request rejected
	StatusExpired         // 3: request expired
)

// handleStatus processes a status value.
func handleStatus(status int) string {
	switch status {
	case 0: // BUG: should be StatusPending
		return "pending"
	case 1: // BUG: should be StatusApproved
		return "approved"
	case StatusRejected:
		return "rejected"
	default:
		return "unknown"
	}
}

// Package x demonstrates a stringly-correct closed-enum dispatch.
// All case literals exactly match a defined const value of the Status type.
package x

// Status is a named string type — a closed enum.
type Status string

const (
	StatusActive   Status = "active"
	StatusInactive Status = "inactive"
	StatusPending  Status = "pending"
)

// handleStatus dispatches on Status.
// All cases exactly match const values — no drift.
func handleStatus(status Status) string {
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

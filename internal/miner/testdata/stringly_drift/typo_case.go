// Package x demonstrates a stringly-typed closed-enum drift defect.
// Status is a named string type with a closed const set.
// The switch has a typo case literal "activ" that matches no const value.
package x

// Status is a named string type — a closed enum.
type Status string

const (
	StatusActive   Status = "active"
	StatusInactive Status = "inactive"
	StatusPending  Status = "pending"
)

// handleStatus dispatches on Status.
// BUG: case "activ" is a typo; no const value of Status equals "activ".
func handleStatus(status Status) string {
	switch status {
	case "activ": // typo — should be "active" (StatusActive)
		return "do-active-thing"
	case "inactive":
		return "do-inactive-thing"
	case "pending":
		return "do-pending-thing"
	default:
		return "unhandled"
	}
}

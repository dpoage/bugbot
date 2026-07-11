// Package x demonstrates an explicit-subset switch with a default clause (D2).
// Kind is a 3-value enum. The switch only covers alpha_kind explicitly; the
// default clause handles the rest. Type-B (missing-arm) must NOT fire.
package x

// Kind is a named string type — a closed enum.
type Kind string

const (
	KindAlpha Kind = "alpha_kind"
	KindBeta  Kind = "beta_kind"
	KindGamma Kind = "gamma_kind"
)

// handleKind dispatches on Kind with an explicit case + default.
// This is the explicit-subset idiom — no drift lead should be emitted.
func handleKind(k Kind) {
	switch k {
	case "alpha_kind":
		// handle alpha
	default:
		// beta_kind and gamma_kind handled here
	}
}

// Package x demonstrates the two-function scope-spoof scenario (D1 regression).
// Mode is a named string type. handleMode uses a typed Mode param (no drift).
// routeCommand uses a raw string param named "mode" — must NOT trigger leads.
package x

// Mode is a named string type — a closed enum.
type Mode string

const (
	ModeKubectl Mode = "kubectl"
	ModeHelm    Mode = "helm"
)

// handleMode dispatches on the Mode enum — correctly typed, no drift.
func handleMode(mode Mode) {
	switch mode {
	case "kubectl":
		// ok
	case "helm":
		// ok
	}
}

// routeCommand uses a raw string param also named "mode".
// Its switch must NOT be resolved to Mode (different function scope).
func routeCommand(mode string) {
	switch mode {
	case "kubectl":
		// raw string, no type
	case "helm":
		// raw string, no type
	}
}

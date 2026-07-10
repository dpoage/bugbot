// Package x demonstrates a switch with multiple uncovered enum values (D3).
// Color is a 4-value enum. The switch covers only "red" — three values are
// uncovered. The miner must emit them in sorted (deterministic) order.
package x

// Color is a named string type — a closed enum.
type Color string

const (
	ColorRed    Color = "red"
	ColorGreen  Color = "green"
	ColorBlue   Color = "blue"
	ColorYellow Color = "yellow"
)

// handleColor dispatches on Color with only one case and no default.
// Three uncovered values: blue, green, yellow (sorted order).
func handleColor(c Color) {
	switch c {
	case "red":
		// handle red
	}
}

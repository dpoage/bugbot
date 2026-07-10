package stringly_drift

// Closure over outer typed param: no shadowing; the closure body references the
// outer `m Mode` directly. The typo "activ" (not "active") must produce a lead.

type Mode string

const (
	ModeActive   Mode = "active"
	ModeInactive Mode = "inactive"
)

func processMode(m Mode) {
	run := func() {
		switch m {
		case "activ": // typo — must fire
		}
	}
	run()
}

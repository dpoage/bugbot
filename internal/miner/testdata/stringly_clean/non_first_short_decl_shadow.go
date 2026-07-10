package stringly_clean

// Non-first short-decl shadow: inside the if-block, `mode` appears as the
// SECOND name in `a, mode := ...`. The switch is over the short-decl'd string
// (not the outer typed param). Must produce ZERO leads.

type ShortMode string

const (
	ShortModeDocker ShortMode = "docker"
	ShortModePodman ShortMode = "podman"
)

func getShortCmd() string { return "docker" }

func handleNonFirstShortDecl(mode ShortMode) {
	if true {
		a, mode := 1, getShortCmd()
		_ = a
		switch mode {
		case "docker":
		case "podman":
		}
	}
}

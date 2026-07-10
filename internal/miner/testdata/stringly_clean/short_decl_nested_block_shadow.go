package stringly_clean

// Short-decl nested block shadow (case D): inside the if-block, `mode` is
// rebound via := from normalize() — a raw string, not BlockMode. The switch
// is over the rebound string; must produce ZERO leads.

type BlockMode string

const (
	BlockModeDocker BlockMode = "docker"
	BlockModePodman BlockMode = "podman"
	BlockModeHelm   BlockMode = "helm"
)

func normalize() string { return "docker" }

func handle(mode BlockMode) {
	if true {
		mode := normalize()
		switch mode {
		case "docker":
		case "podman":
		}
	}
}

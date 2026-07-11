package stringly_clean

// Var-inferred shadow (case F): inside the if-block, `mode` is declared via
// `var mode = getCmd()` — type inferred as string, not InfMode. The switch is
// over the var-declared string; must produce ZERO leads.

type InfMode string

const (
	InfModeDocker InfMode = "docker"
	InfModePodman InfMode = "podman"
	InfModeHelm   InfMode = "helm"
)

func getCmdInf() string { return "docker" }

func handleVarInferred(mode InfMode) {
	if true {
		var mode = getCmdInf()
		switch mode {
		case "docker":
		case "podman":
		}
	}
}

package stringly_clean

// Var-typed shadow (case E): inside the if-block, `mode` is declared via
// `var mode string = getCmd()` — a raw string, not VarMode. The switch is
// over the var-declared string; must produce ZERO leads.

type VarMode string

const (
	VarModeDocker VarMode = "docker"
	VarModePodman VarMode = "podman"
	VarModeHelm   VarMode = "helm"
)

func getCmd() string { return "docker" }

func handleVarTyped(mode VarMode) {
	if true {
		var mode string = getCmd()
		switch mode {
		case "docker":
		case "podman":
		}
	}
}

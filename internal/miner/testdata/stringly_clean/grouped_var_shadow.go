package stringly_clean

// Grouped-var shadow: inside the if-block, `mode` is declared in a grouped
// `var mode, other string = ...` statement. The switch is over the var-declared
// string (not the outer typed param). Must produce ZERO leads.

type GroupedMode string

const (
	GroupedModeDocker GroupedMode = "docker"
	GroupedModePodman GroupedMode = "podman"
	GroupedModeHelm   GroupedMode = "helm" // 3rd const: uncovered by switch, discriminates shadow regression
)

func getGroupedCmd() string   { return "docker" }
func getGroupedOther() string { return "other" }

func handleGroupedVar(mode GroupedMode) {
	if true {
		var mode, other string = getGroupedCmd(), getGroupedOther()
		_ = other
		switch mode {
		case "docker":
		case "podman":
		}
	}
}

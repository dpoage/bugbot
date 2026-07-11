package stringly_clean

// Short-decl closure shadow (case C): inside the handler closure, `mode` is
// rebound via := from fetchCmd() — a raw string, not Mode. The switch is over
// the rebound string; must produce ZERO leads.

type ContainerMode string

const (
	ContainerModeDocker ContainerMode = "docker"
	ContainerModePodman ContainerMode = "podman"
	ContainerModeHelm   ContainerMode = "helm"
)

func fetchCmd() string { return "docker" }

func outerWithShortDeclClosure(mode ContainerMode) {
	handler := func() {
		mode := fetchCmd()
		switch mode {
		case "docker":
		case "podman":
		}
	}
	handler()
}

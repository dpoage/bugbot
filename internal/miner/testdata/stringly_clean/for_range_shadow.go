package stringly_clean

// For-range shadow: inside the for-range, `mode` is redeclared via `:=` as
// the loop variable. The switch is over the loop variable (a raw string key),
// not the outer typed param. Must produce ZERO leads.

type ForMode string

const (
	ForModeDocker ForMode = "docker"
	ForModePodman ForMode = "podman"
	ForModeHelm   ForMode = "helm" // 3rd const: uncovered by switch, discriminates shadow regression
)

func handleForRange(mode ForMode, items map[string]int) {
	for mode := range items {
		switch mode {
		case "docker":
		case "podman":
		}
	}
}

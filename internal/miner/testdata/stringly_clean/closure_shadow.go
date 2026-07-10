package stringly_clean

// Closure shadow: the inner func's `mode string` shadows the outer `mode Mode`.
// The switch inside the closure is over a raw string — must produce ZERO leads.

type Mode string

const (
	ModeDocker Mode = "docker"
	ModePodman Mode = "podman"
	ModeHelm   Mode = "helm"
)

func outer(mode Mode) func(string) {
	return func(mode string) {
		switch mode {
		case "docker":
		case "podman":
		}
	}
}

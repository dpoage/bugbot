package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// newPrimeCmd prints repo-aware guidance for authoring a bugbot.yaml. It is the
// bugbot analogue of `bd prime`: an agent (or human) runs it inside the target
// repository to learn how to configure Bugbot correctly — most importantly that
// the sandbox image MUST carry the target language's toolchain, the single most
// common silent misconfiguration (repro/verify run the toolchain inside the
// sandbox under network=none; a toolchain-less image makes every finding
// silently stay unreproduced with an environment_error).
//
// prime instructs; it does not write. `bugbot init` writes the annotated
// template and `bugbot doctor` validates the result.
func newPrimeCmd() *cobra.Command {
	var target string
	cmd := &cobra.Command{
		Use:   "prime",
		Short: "Print guidance for authoring bugbot.yaml for the target repo",
		Long: `prime prints instructions for preparing a ` + "bugbot.yaml" + ` for a target
repository. It detects the repo's build systems and container runtime and
recommends a concrete sandbox image and dependency strategy, then explains the
non-obvious decisions (toolchain image, dep_strategy, secrets, roles, budgets).

It is intended to be read by an LLM agent setting Bugbot up on a new repo. It
only prints guidance; use ` + "`bugbot init`" + ` to write the template and
` + "`bugbot doctor`" + ` to validate.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			repoDir, err := filepath.Abs(target)
			if err != nil {
				return fmt.Errorf("resolve target %q: %w", target, err)
			}
			rt, ok := sandbox.Detect()
			facts := gatherPrimeFacts(repoDir, rt, ok)
			_, err = io.WriteString(cmd.OutOrStdout(), renderPrime(facts))
			return err
		},
	}
	addTargetFlag(cmd, &target)
	return cmd
}

// primeFacts is the detected state of the target repo plus the recommendation
// derived from it. It is a plain data struct so renderPrime is a pure function
// of detected facts (testable without a filesystem).
type primeFacts struct {
	Target       string
	Runtime      string
	RuntimeFound bool
	BuildSystems []ingest.BuildSystem
	GoVersion    string // major.minor from go.mod, "" if absent/unknown
	Vendored     bool   // Go vendor/modules.txt present (offline build works)
	HasReqsTxt   bool   // Python requirements.txt present (wheelhouse FETCH works)
	Image        string // recommended sandbox.image
	ImageNote    string
	DepStrategy  string // recommended sandbox.dep_strategy
	StrategyNote string
}

// gatherPrimeFacts probes repoDir for build systems, vendoring, and the Go
// version, and derives the image/dep_strategy recommendation. runtime/runtimeOK
// come from sandbox.Detect (injected so the command stays testable).
func gatherPrimeFacts(repoDir, runtime string, runtimeOK bool) primeFacts {
	bs := ingest.DetectBuildSystems(repoDir)
	f := primeFacts{
		Target:       repoDir,
		Runtime:      runtime,
		RuntimeFound: runtimeOK,
		BuildSystems: bs,
		GoVersion:    goModVersion(repoDir),
		Vendored:     fileExists(filepath.Join(repoDir, "vendor", "modules.txt")),
		HasReqsTxt:   fileExists(filepath.Join(repoDir, "requirements.txt")),
	}
	f.Image, f.ImageNote = recommendImage(bs, f.GoVersion)
	f.DepStrategy, f.StrategyNote = recommendDepStrategy(bs, f.Vendored, f.HasReqsTxt)
	return f
}

// toolchainImage maps a LANGUAGE-specific build system to a default image that
// carries that language's toolchain. Meta build systems (bazel/make/ninja and
// the workspace variants beyond their language) are handled by recommendImage.
func toolchainImage(bs ingest.BuildSystem, goVersion string) (image string, ok bool) {
	switch bs {
	case ingest.BuildSystemGoModule, ingest.BuildSystemGoWorkspace:
		tag := "latest"
		if goVersion != "" {
			tag = goVersion
		}
		return "docker.io/library/golang:" + tag + "-alpine", true
	case ingest.BuildSystemPython:
		return "docker.io/library/python:3-slim", true
	case ingest.BuildSystemCargo:
		return "docker.io/library/rust:1-slim", true
	case ingest.BuildSystemNPM, ingest.BuildSystemJSWorkspace:
		return "docker.io/library/node:22-slim", true
	case ingest.BuildSystemCMake, ingest.BuildSystemMeson:
		return "docker.io/library/gcc:14", true
	}
	return "", false
}

// recommendImage picks a sandbox image that carries the toolchain for the
// repo's primary language. Bazel is checked FIRST: bugbot's repro pipeline
// actually runs `bazel test //...` for Bazel repos, so a non-bazel image
// (e.g. golang:*-alpine) would silently fail every reproduce under
// network=none — repro would exit 127 / `environment_error` and the
// finding would stay unverified. Other language-specific tools are
// preferred over bazel when no Bazel marker is present.
func recommendImage(bs []ingest.BuildSystem, goVersion string) (image, note string) {
	// Bazel detected: the image MUST be bazel-capable (we run `bazel test`),
	// AND it must still carry the language toolchains the targets build, AND
	// a prefetched bazel repository cache so offline (network=none) repro
	// works. A plain `gcr.io/bazel-public/bazel:latest` may not be enough on
	// its own — point users at a custom image in that case.
	if containsBuildSystem(bs, ingest.BuildSystemBazel) {
		return "gcr.io/bazel-public/bazel:latest",
			"Bazel detected: bugbot runs `bazel test //...` for Bazel repos, so the image MUST be bazel-capable. The image must ALSO carry the target language toolchains (Go/Java/C++/...) your targets build, and for offline (network=none) repro it must include a prefetched bazel repository cache — a plain bazel image is usually not enough; consider a custom image, or disable repro for Bazel repos."
	}
	for _, b := range bs {
		if img, ok := toolchainImage(b, goVersion); ok {
			return img, imageNoteFor(b)
		}
	}
	for _, b := range bs {
		switch b {
		case ingest.BuildSystemMake, ingest.BuildSystemNinja:
			return "docker.io/library/gcc:14",
				"Only a generic build system (make/ninja) was detected; gcc:14 covers C/C++ + make. If the repo is another language, use that language's toolchain image instead."
		}
	}
	return "docker.io/library/debian:stable-slim",
		"No language toolchain was detected. debian:stable-slim has NO compiler or test runner — keep it ONLY if you will not run `bugbot repro`/verify. Otherwise set an image carrying the target language's toolchain."
}

// imageNoteFor returns the per-language caveat appended to the image
// recommendation.
func imageNoteFor(bs ingest.BuildSystem) string {
	switch bs {
	case ingest.BuildSystemGoModule, ingest.BuildSystemGoWorkspace:
		return "golang:*-alpine is pure-Go-ready. If the repo uses cgo, switch to golang:<ver> (Debian-based, ships gcc)."
	case ingest.BuildSystemPython:
		return "python:3-slim ships pip. If the repo needs compiled wheels not on PyPI, use a fuller image or add build deps via a custom image."
	case ingest.BuildSystemCargo:
		return "rust:1-slim ships cargo + rustc. Crates are not vendored by default — see dep_strategy below."
	case ingest.BuildSystemNPM, ingest.BuildSystemJSWorkspace:
		return "node:22-slim ships node + npm. Pin to the major your repo targets."
	case ingest.BuildSystemCMake, ingest.BuildSystemMeson:
		return "gcc:14 ships gcc/g++/make but NOT cmake or meson — add them via a custom image, or use an image that already bundles your build tool."
	}
	return ""
}

// recommendDepStrategy picks the network=none dependency strategy for the repo.
// Go and Python have wired strategies; Bazel intentionally does NOT — bugbot
// has no bazel dependency strategy today, and offline (network=none) bazel
// repro requires a custom image with a prefetched bazel repository cache, or
// disabling repro for bazel repos. Other ecosystems get "off" with a generic
// note to vendor/commit deps.
func recommendDepStrategy(bs []ingest.BuildSystem, vendored, hasReqs bool) (strategy, note string) {
	isGo := containsBuildSystem(bs, ingest.BuildSystemGoModule) || containsBuildSystem(bs, ingest.BuildSystemGoWorkspace)
	isPy := containsBuildSystem(bs, ingest.BuildSystemPython) || hasReqs
	isBazel := containsBuildSystem(bs, ingest.BuildSystemBazel)
	switch {
	case isBazel:
		return "off", "Bazel detected: Bugbot has NO bazel dependency strategy. Offline (network=none) bazel repro requires a custom sandbox image with a prefetched bazel repository cache (the default bazel image does not bundle one), or disable repro for this repo. 'off' is correct only if every bazel target you care about builds from sources already in the repo."
	case isGo && vendored:
		return "off", "Go modules are vendored (vendor/modules.txt) → the build resolves entirely from vendor/ under network=none. No mounts needed."
	case isGo:
		return "host", "Non-vendored Go: 'host' mounts the host Go module cache read-only (exposes PUBLIC module source only — never secrets). 'fetch' warms a bugbot-managed cache with ONE online `go mod download`. Or run `go mod vendor` and use 'off'."
	case isPy && hasReqs:
		return "fetch", "Python with requirements.txt: 'fetch' builds an offline wheelhouse (ONE online `pip download`) then installs it under network=none. Use 'off' only if the repro imports no third-party packages."
	case isPy:
		return "off", "Python without requirements.txt: pyproject/uv-only dep strategies are not wired yet — keep 'off' and ensure the repro imports only the stdlib or already-installed packages."
	default:
		return "off", "This ecosystem has no offline dependency strategy in Bugbot yet. Vendor/commit dependencies into the repo, or bake them into a custom sandbox image."
	}
}

// goModVersion reads the `go X.Y[.Z]` directive from repoDir/go.mod and returns
// it reduced to major.minor (the form golang container images are tagged with).
// Returns "" when go.mod is absent or has no parseable directive.
func goModVersion(repoDir string) string {
	data, err := os.ReadFile(filepath.Join(repoDir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "go ")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if parts := strings.Split(v, "."); len(parts) >= 2 {
			return parts[0] + "." + parts[1]
		}
		return v
	}
	return ""
}

// fileExists reports whether path exists and is a regular file.
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// containsBuildSystem reports whether bs contains want.
func containsBuildSystem(bs []ingest.BuildSystem, want ingest.BuildSystem) bool {
	for _, b := range bs {
		if b == want {
			return true
		}
	}
	return false
}

// renderPrime renders the full guidance document from detected facts. It is a
// pure function of f so it can be unit-tested with synthetic facts.
func renderPrime(f primeFacts) string {
	var b strings.Builder

	b.WriteString("# bugbot prime — preparing bugbot.yaml for this repo\n\n")
	b.WriteString("Bugbot runs a precision-first pipeline (ingest -> hypothesize -> verify ->\n")
	b.WriteString("reproduce -> report). The verify and reproduce stages execute the target's\n")
	b.WriteString("OWN test/build toolchain inside a sandbox container under network=none.\n\n")

	// --- Detected ---------------------------------------------------------
	b.WriteString("## Detected in this repository\n\n")
	fmt.Fprintf(&b, "- target: %s\n", f.Target)
	if f.RuntimeFound {
		fmt.Fprintf(&b, "- container runtime: %s\n", f.Runtime)
	} else {
		b.WriteString("- container runtime: NONE FOUND — install podman or docker, or repro/verify are skipped\n")
	}
	if len(f.BuildSystems) == 0 {
		b.WriteString("- build systems: none detected\n")
	} else {
		names := make([]string, len(f.BuildSystems))
		for i, bs := range f.BuildSystems {
			names[i] = string(bs)
		}
		fmt.Fprintf(&b, "- build systems: %s (primary: %s)\n", strings.Join(names, ", "), names[0])
	}
	if f.GoVersion != "" {
		fmt.Fprintf(&b, "- go version (from go.mod): %s\n", f.GoVersion)
	}
	if containsBuildSystem(f.BuildSystems, ingest.BuildSystemGoModule) || containsBuildSystem(f.BuildSystems, ingest.BuildSystemGoWorkspace) {
		fmt.Fprintf(&b, "- go vendored: %t\n", f.Vendored)
	}
	if f.HasReqsTxt {
		b.WriteString("- python requirements.txt: present\n")
	}
	b.WriteString("\n")

	// --- Recommendation ---------------------------------------------------
	b.WriteString("## Recommended sandbox settings for this repo\n\n")
	b.WriteString("```yaml\n")
	b.WriteString("sandbox:\n")
	fmt.Fprintf(&b, "  image: %s\n", f.Image)
	fmt.Fprintf(&b, "  dep_strategy: %s\n", f.DepStrategy)
	b.WriteString("  network: none\n")
	b.WriteString("```\n\n")
	fmt.Fprintf(&b, "- image: %s\n", f.ImageNote)
	fmt.Fprintf(&b, "- dep_strategy: %s\n\n", f.StrategyNote)

	// --- Why the image matters -------------------------------------------
	b.WriteString("## Why the image matters (do not skip)\n\n")
	b.WriteString("The reproduce/verify stages run the repo's test or build command INSIDE the\n")
	b.WriteString("sandbox image. If the image lacks that toolchain the command exits 127, or its\n")
	b.WriteString("build cache overruns /tmp — Bugbot records `environment_error` and the finding\n")
	b.WriteString("SILENTLY stays unreproduced. The image MUST carry the repo's language toolchain:\n\n")
	b.WriteString("    Go       docker.io/library/golang:<ver>-alpine   (golang:<ver> for cgo/gcc)\n")
	b.WriteString("    Python   docker.io/library/python:3-slim\n")
	b.WriteString("    Node/TS  docker.io/library/node:22-slim\n")
	b.WriteString("    Rust     docker.io/library/rust:1-slim\n")
	b.WriteString("    C/C++    docker.io/library/gcc:14  (add cmake/meson if your build needs them)\n")
	b.WriteString("    Bazel    gcr.io/bazel-public/bazel:latest + language toolchains; offline\n")
	b.WriteString("             (network=none) repro requires a custom image with a prefetched\n")
	b.WriteString("             bazel repository cache (or disable repro for Bazel repos)\n")
	b.WriteString("    mixed    a custom image carrying every toolchain you need\n\n")

	// --- dep_strategy -----------------------------------------------------
	b.WriteString("## dep_strategy (network=none dependency resolution)\n\n")

	b.WriteString("    vendored Go (vendor/)      off    builds offline from vendor/\n")
	b.WriteString("    non-vendored Go            host   mount host module cache RO, or 'fetch'\n")
	b.WriteString("    Python + requirements.txt  fetch  offline wheelhouse (one online step)\n")
	b.WriteString("    Bazel                     off    NO bazel dep strategy; offline repro needs\n")
	b.WriteString("                                   a custom image with a prefetched cache\n")
	b.WriteString("                                   (or disable repro for Bazel repos)\n")
	b.WriteString("    other ecosystems           off    vendor/commit deps or bake a custom image\n\n")

	// --- Secrets ----------------------------------------------------------
	b.WriteString("## Secrets\n\n")
	b.WriteString("NEVER put API keys in bugbot.yaml. Each provider names api_key_env (or, for\n")
	b.WriteString("Claude OAuth, auth_token_env); export the value in the environment before\n")
	b.WriteString("running. Bugbot reads it from the process environment at run time.\n\n")

	// --- Roles & budgets --------------------------------------------------
	b.WriteString("## Roles & budgets\n\n")
	b.WriteString("- roles: tier a cheap/fast model to `finder` (breadth), the strongest model to\n")
	b.WriteString("  `verifier` (precision), a mid model to `reproducer`.\n")
	b.WriteString("- budgets: per_cycle_tokens bounds one investigation; per_day_tokens bounds the\n")
	b.WriteString("  daemon across 24h. 0 or any negative value means UNLIMITED on that axis.\n\n")

	// --- Workflow ---------------------------------------------------------
	b.WriteString("## Workflow\n\n")
	b.WriteString("1. `bugbot init`                 # writes an annotated bugbot.yaml — then edit it\n")
	b.WriteString("2. set sandbox.image + dep_strategy per the recommendation above\n")
	b.WriteString("3. set roles to real provider+model names; export each api_key_env\n")
	b.WriteString("4. `bugbot doctor`              # validates config, runtime, image presence, keys\n")
	b.WriteString("5. `bugbot scan` / `bugbot repro`\n")

	return b.String()
}

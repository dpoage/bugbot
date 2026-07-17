package sandbox

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// runParams is the fully-resolved set of inputs to a single container run,
// after backend defaults have been applied to a Spec.
type runParams struct {
	// containerName is the generated --name, used to forcibly remove the
	// container on timeout.
	containerName string
	// workspace is the host path of the prepared rw workspace mounted at
	// /workspace inside the container.
	workspace string
	image     string
	network   string
	cpus      float64
	memoryMB  int
	pidsLimit int
	env       []string
	cmd       []string
	// roMounts are extra read-only bind mounts (e.g. a dependency cache),
	// rendered after the writable workspace mount and never writable.
	roMounts []ROMount
	// rwMounts are extra writable bind mounts: the trusted
	// dependency-prefetch step's cache dirs, and operator "writable: true"
	// local_mounts entries (bugbot-wjc2; see Spec.RWMounts for the security
	// tradeoff).
	rwMounts []ROMount
	// setupCmds are optional in-container commands run before cmd. When
	// non-empty the backend wraps execution in /bin/sh; each command is
	// shell-quoted and chained with "|| exit 125" so any setup failure aborts
	// the run with an environment_error exit code (see Spec.SetupCmds).
	setupCmds [][]string
}

// workspaceMount is where the writable workspace copy is mounted inside the
// container, and the working directory for the executed command.
const workspaceMount = "/workspace"

// buildRunArgs constructs the argv passed to the runtime CLI (excluding the
// runtime binary itself) for a `run` invocation. It is a pure function so the
// security-relevant flag construction can be exercised in unit tests without a
// container runtime.
//
// Security posture encoded here (defense in depth for untrusted, model-driven
// code):
//   - --rm                      : always reap the container on exit.
//   - --network=<network>       : "none" by default, no egress.
//   - --read-only               : read-only root filesystem...
//   - --tmpfs /tmp              : ...with a writable scratch tmpfs sized to
//     host language toolchain caches (Go's cold build cache alone can run to
//     hundreds of MB).
//   - --env HOME=/tmp           : caches that default under $HOME (Go, pip,
//     npm, ...) land on the writable tmpfs instead of dying on the read-only
//     root; without this `go test` fails instantly with "failed to initialize
//     build cache: read-only file system" before it ever compiles. Spec.Env
//     entries are appended after and may override.
//   - -v ws:/workspace:rw,Z     : the workspace copy is the only writable mount
//     (Z relabels for SELinux; harmless elsewhere). The original repo is never
//     mounted.
//   - -v host:ctr:ro[,Z]        : any Spec.ROMounts are mounted READ-ONLY (a
//     dependency cache, for example). These are never writable, but they DO
//     expose host content to untrusted code, so callers must only mount
//     public/cache content — never secrets. See the package doc and Spec.ROMounts.
//     The :Z suffix (SELinux private relabel) is added ONLY when ROMount.Shared
//     is false (bugbot-owned dirs). Shared host dirs (e.g. the user's Go module
//     cache) must NOT be relabeled: :Z on a multi-GB shared cache is slow,
//     breaks the host go toolchain, and breaks other containers sharing the dir.
//     See ROMount.Shared for the full tradeoff. RWMounts (prefetch only, always
//     bugbot-owned) always get :rw,Z.
//   - --workdir /workspace      : run from the workspace.
//   - --cap-drop ALL            : drop all Linux capabilities.
//   - --security-opt no-new-privileges : block privilege escalation (setuid).
//   - --pids-limit              : cap process count (fork-bomb resistance).
//   - --memory / --cpus         : resource limits.
func buildRunArgs(p runParams) []string {
	args := []string{
		"run",
		"--rm",
		"--name", p.containerName,
		"--network=" + p.network,
		"--read-only",
		"--tmpfs", "/tmp:rw,exec,nosuid,size=512m",
		"--env", "HOME=/tmp",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--workdir", workspaceMount,
		"-v", fmt.Sprintf("%s:%s:rw,Z", p.workspace, workspaceMount),
	}

	// Read-only mounts are rendered right after the writable workspace, in the
	// caller-supplied order, and are always :ro (never writable). Bugbot-owned
	// dirs (Shared=false) additionally get the :Z SELinux relabel suffix for
	// isolation; shared host dirs (Shared=true) must NOT be relabeled — see
	// ROMount.Shared for the full rationale.
	for _, m := range p.roMounts {
		label := "ro,Z"
		if m.Shared {
			label = "ro"
		}
		args = append(args, "-v", fmt.Sprintf("%s:%s:%s", m.HostPath, m.ContainerPath, label))
	}
	// Writable mounts: the trusted dependency-prefetch step's caches and
	// operator "writable: true" local_mounts (bugbot-wjc2). Same Shared
	// semantics as the RO loop: host-owned dirs (Shared=true, e.g. a bazel
	// vendor dir the host also manages) must NOT be SELinux :Z relabeled —
	// a private container context would break host-side management of the
	// same tree.
	for _, m := range p.rwMounts {
		label := "rw,Z"
		if m.Shared {
			label = "rw"
		}
		args = append(args, "-v", fmt.Sprintf("%s:%s:%s", m.HostPath, m.ContainerPath, label))
	}

	if p.pidsLimit > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(p.pidsLimit))
	}
	if p.memoryMB > 0 {
		args = append(args, "--memory", strconv.Itoa(p.memoryMB)+"m")
	}
	if p.cpus > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(p.cpus, 'f', -1, 64))
	}
	for _, e := range p.env {
		args = append(args, "--env", e)
	}

	args = append(args, p.image)

	// When SetupCmds are present, wrap the execution in /bin/sh: the setup
	// script runs each setup command with "|| exit 125" and then exec's the
	// original command so it retains its own exit code and signal disposition.
	// The argv shape is:
	//   /bin/sh -c <script> sh <original cmd...>
	// where "sh" is $0 (the shell's argv[0]) and the original cmd becomes
	// $1, $2, ... (positional parameters fed to "exec $@").
	// When SetupCmds is empty, the original cmd is appended directly and
	// /bin/sh is never involved, preserving existing behavior for Go runs.
	if len(p.setupCmds) > 0 {
		script := buildSetupScript(p.setupCmds)
		args = append(args, "/bin/sh", "-c", script, "sh")
	}
	args = append(args, p.cmd...)
	return args
}

// validateMounts checks that every extra bind mount (read-only and writable)
// is well-formed: both paths absolute and non-empty, and no duplicate
// ContainerPath across the combined set (two mounts at the same container path
// is a configuration error and the runtime's behavior would be ambiguous). The
// workspace mount at /workspace is implicit and not represented here. It
// returns the first problem found.
func validateMounts(ro, rw []ROMount) error {
	seen := make(map[string]bool, len(ro)+len(rw))
	check := func(mounts []ROMount, kind string) error {
		for _, m := range mounts {
			if m.HostPath == "" || m.ContainerPath == "" {
				return fmt.Errorf("sandbox: %s mount requires non-empty host and container paths", kind)
			}
			if !filepath.IsAbs(m.HostPath) {
				return fmt.Errorf("sandbox: %s mount host path %q must be absolute", kind, m.HostPath)
			}
			if !filepath.IsAbs(m.ContainerPath) {
				return fmt.Errorf("sandbox: %s mount container path %q must be absolute", kind, m.ContainerPath)
			}
			if seen[m.ContainerPath] {
				return fmt.Errorf("sandbox: duplicate mount container path %q", m.ContainerPath)
			}
			seen[m.ContainerPath] = true
		}
		return nil
	}
	if err := check(ro, "read-only"); err != nil {
		return err
	}
	return check(rw, "writable")
}

// shellQuote returns a POSIX single-quoted form of arg that is safe to embed in
// a shell script regardless of the arg's content (spaces, $, ;, newlines, etc.).
// Embedded single quotes are escaped by closing the current quote, inserting a
// literal backslash-quoted single quote, then reopening the quote — the only
// POSIX-portable way to include a literal single quote inside a single-quoted
// string.
//
// Examples:
//
//	"hello world"     → 'hello world'
//	"it's"            → 'it'"'"'s'
//	"$HOME"           → '$HOME'      (prevents variable expansion)
//	"; rm -rf /"      → '; rm -rf /' (no injection)
//	""                → ''           (empty arg preserved)
func shellQuote(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

// buildSetupScript constructs a POSIX sh script fragment that runs each setup
// command in order, aborting with exit 125 on any failure, then exec's the
// original command. The result is intended as the -c argument to /bin/sh.
//
// Exit 125 is chosen deliberately: internal/repro/interpret.go and patch.go
// both classify container exit 125/126/127 as environment_error, so a setup
// failure (e.g. "npm ci --offline" cache miss) never surfaces as a false
// bug demonstration.
//
// The trailing `exec "$@"` passes the original command (from sh's positional
// parameters $1, $2, ...) with exec so the sh wrapper process is replaced by
// the actual command — it retains its own exit code and signal disposition
// rather than going through another sh exit-code forwarding layer.
func buildSetupScript(setupCmds [][]string) string {
	var b strings.Builder
	for _, argv := range setupCmds {
		// An empty argv would render as a bare "|| exit 125" line, which sh
		// treats as a successful no-op — the guard would silently never fire.
		// Skip such entries rather than emitting a dead guard.
		if len(argv) == 0 {
			continue
		}
		for i, arg := range argv {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(shellQuote(arg))
		}
		b.WriteString(" || exit 125\n")
	}
	b.WriteString("exec \"$@\"")
	return b.String()
}

// removeArgs constructs the argv for forcibly removing a container by name,
// used to reap a container that outran its timeout.
func removeArgs(containerName string) []string {
	return []string{"rm", "-f", containerName}
}

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
	// runtime is the resolved container runtime binary name ("podman" or
	// "docker", see cli.go's candidateRuntimes/Detect). buildRunArgs uses it
	// to gate the podman-only "--userns=keep-id:uid=,gid=" identity mapping
	// (see resolveNonRootUser/containerUID below): Docker has no equivalent
	// per-run flag (only a daemon-wide --userns-remap), so the non-root-USER
	// fix does not apply under Docker.
	runtime string
	// containerUID/containerGID, when containerUID > 0, are the numeric
	// identity a non-root image USER resolves to (see resolveNonRootUser).
	// buildRunArgs then adds "--user <uid>:<gid>" plus
	// "--userns=keep-id:uid=<uid>,gid=<gid>" so the container's non-root
	// process IS the invoking host user, one layer removed through podman's
	// user namespace — with ZERO host-side ownership mutation (see the
	// buildRunArgs doc comment for the full mechanism and bugbot-p8y4's
	// --design for the alternatives evaluated). Zero value (0) means "no
	// override": either the probe was never run (runtime != "podman"),
	// found the image runs as root (uid 0 needs no override — root-in-
	// container already maps to the invoking host user under rootless
	// podman, the pre-existing working case), or failed best-effort.
	containerUID, containerGID int
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
	// scratchSizeMB is the size (MB) of the writable /tmp tmpfs scratch
	// space, rendered as the --tmpfs size=... mount option (bugbot-yrox).
	// <= 0 falls back to fallbackScratchSizeMB — buildRunArgs always emits a
	// sized tmpfs, never an unbounded one.
	scratchSizeMB int
	env           []string
	cmd           []string
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

// fallbackScratchSizeMB is the writable tmpfs scratch-space size (MB)
// applied to /tmp — and, under bwrap, the tmpfs root as well
// (bwrap_command.go) — when a resolved runParams.scratchSizeMB /
// bwrapParams.scratchSizeBytes is <= 0 (no operator override,
// sandbox.scratch_size_mb, was ever configured). Named distinctly from the
// CLI/Bwrap struct fields also called "defaultScratchSizeMB" — this
// constant is the LAST-RESORT fallback below even a zero-value backend
// field, not "the backend's configured default". Matches the container
// backend's historical hardcoded 512m, so an unconfigured host sees
// byte-identical behavior to before bugbot-yrox.
const fallbackScratchSizeMB = 512

// buildRunArgs constructs the argv passed to the runtime CLI (excluding the
// runtime binary itself) for a `run` invocation. It is a pure function so the
// security-relevant flag construction can be exercised in unit tests without a
// container runtime.
//
// Security posture encoded here (defense in depth for untrusted, model-driven
// code):
//
//   - --rm                      : always reap the container on exit.
//
//   - --network=<network>       : "none" by default, no egress.
//
//   - --read-only               : read-only root filesystem...
//
//   - --tmpfs /tmp              : ...with a writable scratch tmpfs sized by
//     p.scratchSizeMB (sandbox.scratch_size_mb; <= 0 falls back to
//     fallbackScratchSizeMB) — big enough for host language toolchain caches
//     (Go's cold build cache alone can run to hundreds of MB) but explicitly
//     bounded rather than left to the host's free RAM (bugbot-yrox).
//
//   - --env HOME=/tmp           : caches that default under $HOME (Go, pip,
//     npm, ...) land on the writable tmpfs instead of dying on the read-only
//     root; without this `go test` fails instantly with "failed to initialize
//     build cache: read-only file system" before it ever compiles. Spec.Env
//     entries are appended after and may override.
//
//   - --entrypoint=              : unconditionally NEUTRALIZE the image's own
//     ENTRYPOINT (bugbot-p8y4/bugbot-cbm5). Without this, any image that
//     declares one gets it silently PREPENDED to whatever argv this function
//     builds: gcr.io/bazel-public/bazel ships ENTRYPOINT
//     [/usr/local/bin/bazel], turning bugbot's own `/bin/sh -c <script> sh
//     <cmd>` into `bazel /bin/sh -c <script> sh <cmd>` — bazel then tries to
//     run "/bin/sh" as one of ITS OWN startup options and fails with
//     "Command /bin/sh not found", which internal/repro's classifySmoke
//     pattern-matches on "not found" and misreports as toolchain_missing,
//     wrongly gating the whole repro stage (BlocksRepro) on a fabricated
//     verdict for images that have nothing to do with bazel. Every Spec.Cmd
//     is already a complete, explicit argv (or the /bin/sh setup wrapper
//     below); bugbot never wants an image's bundled entrypoint script
//     running ahead of — or instead of — the command it was asked to run.
//     Verified against podman 5.7.0 AND real Docker 27.5.1/29.6.0 on this
//     host: `--entrypoint=` (a single token, matching the `--network=`
//     convention above) resets it exactly like `--entrypoint ""` on BOTH
//     runtimes, so it is emitted unconditionally, never runtime-gated.
//
//   - --user <uid>:<gid> + --userns=keep-id:uid=<uid>,gid=<gid> : emitted
//     ONLY when p.containerUID > 0 (a non-root image USER was resolved —
//     see resolveNonRootUser), and ONLY on podman (p.runtime == "podman";
//     Docker has no per-run equivalent, only a daemon-wide --userns-remap,
//     so this fix does not extend to the Docker backend). This is
//     bugbot-p8y4's second defect fix: an image with a non-root USER (e.g.
//     bazel-public's now-shipped "USER ubuntu") otherwise cannot read the
//     workspace, which prepareWorkspace leaves owned by the invoking host
//     user at mode 0700.
//
//     MECHANISM (verified empirically, podman 5.7.0): "--user U:G" forces
//     the containerized process to run as UID U — the SAME numeric UID the
//     image's own USER directive resolves to (so the process still looks
//     like "ubuntu" from inside, matching what the image expects). Without
//     anything else, rootless podman's default user-namespace mapping would
//     send that U to an unrelated SUBORDINATE host UID (confirmed: uid 1000
//     maps to 100999) — completely unable to read anything owned by the
//     invoking host user. "--userns=keep-id:uid=U,gid=G" changes that
//     mapping so container UID U is the ONE explicitly mapped back to the
//     invoking host user (not "0", keep-id's bare-form default) — so the
//     containerized non-root process IS the host user, one namespace layer
//     removed, for exactly the mount it needs to read/write. HOST-SIDE
//     OWNERSHIP OF THE WORKSPACE IS NEVER TOUCHED: verified before/after a
//     real run — mode and owning UID/GID on the host are byte-identical.
//
//     This replaced an earlier, REJECTED design that chowned the mount via
//     podman's ":U" bind-mount option instead. That was wrong: :U moves the
//     mount's ownership to the subordinate UID for the DURATION of the
//     bind, but nothing moves it back afterward, and bugbot's own HOST-side
//     code (Spec.CaptureFiles, the deferred workspace os.RemoveAll, a
//     reused Spec.Workspace iteration directory, the workspace-growth-
//     ceiling watchdog's filesystem probe, and the persistent dependency-
//     prefetch cache) all run as the INVOKING host user AFTER the container
//     exits — every one of those was found (two dual-oracle review rounds,
//     live podman probes) to silently break once its target was chowned
//     out from under it. --userns=keep-id:uid=,gid= has none of that
//     failure mode because it never mutates anything on the host: it only
//     changes how the CONTAINER's OWN namespace maps an already-existing
//     host identity.
//
//     Resolving U/G from an arbitrary image (Config.User may be a bare
//     name like "ubuntu", not a number) requires one lightweight probe
//     container per distinct image (`id -u`/`id -g`); see
//     resolveNonRootUser's doc comment for that mechanism and its cache.
//
//   - -v ws:/workspace:rw,Z     : the workspace copy is the only writable
//     mount (Z relabels for SELinux; harmless elsewhere). The original repo
//     is never mounted.
//
//   - -v host:ctr:ro[,Z]        : any Spec.ROMounts are mounted READ-ONLY (a
//     dependency cache, for example). These are never writable, but they DO
//     expose host content to untrusted code, so callers must only mount
//     public/cache content — never secrets. See the package doc and Spec.ROMounts.
//     The :Z suffix (SELinux private relabel) is added ONLY when ROMount.Shared
//     is false (bugbot-owned dirs). Shared host dirs (e.g. the user's Go module
//     cache) must NOT be relabeled: :Z on a multi-GB shared cache is slow,
//     breaks the host go toolchain, and breaks other containers sharing the dir.
//     See ROMount.Shared for the full tradeoff. RWMounts (prefetch caches and
//     operator "writable: true" local_mounts, bugbot-wjc2) follow the same :Z
//     rule: relabeled only when Shared is false. Neither loop ever chowns
//     anything — the --userns=keep-id mapping above is what makes these
//     mounts' EXISTING host ownership readable/writable to a non-root
//     container process; no mount-option change was needed here at all.
//
//   - --workdir /workspace      : run from the workspace.
//
//   - --cap-drop ALL            : drop all Linux capabilities.
//
//   - --security-opt no-new-privileges : block privilege escalation (setuid).
//
//   - --pids-limit              : cap process count (fork-bomb resistance).
//
//   - --memory / --cpus         : resource limits.
func buildRunArgs(p runParams) []string {
	scratchMB := p.scratchSizeMB
	if scratchMB <= 0 {
		scratchMB = fallbackScratchSizeMB
	}
	args := []string{
		"run",
		"--rm",
		"--name", p.containerName,
		"--network=" + p.network,
		"--read-only",
		"--tmpfs", fmt.Sprintf("/tmp:rw,exec,nosuid,size=%dm", scratchMB),
		"--env", "HOME=/tmp",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--workdir", workspaceMount,
		"--entrypoint=",
	}

	// Non-root image identity: see the doc comment above and
	// resolveNonRootUser. Podman-only (Docker has no per-run keep-id
	// equivalent); no-op (containerUID == 0) for a root-USER image or when
	// the resolution probe never ran/failed.
	if p.runtime == "podman" && p.containerUID > 0 {
		args = append(args,
			"--user", fmt.Sprintf("%d:%d", p.containerUID, p.containerGID),
			fmt.Sprintf("--userns=keep-id:uid=%d,gid=%d", p.containerUID, p.containerGID),
		)
	}

	args = append(args, "-v", fmt.Sprintf("%s:%s:rw,Z", p.workspace, workspaceMount))

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

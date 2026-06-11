package sandbox

import (
	"fmt"
	"path/filepath"
	"strconv"
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
	// rwMounts are extra writable bind mounts, used only by the trusted
	// dependency-prefetch step (see Spec.RWMounts).
	rwMounts []ROMount
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
	// Writable mounts are used only by the trusted dependency-prefetch step.
	for _, m := range p.rwMounts {
		args = append(args, "-v", fmt.Sprintf("%s:%s:rw,Z", m.HostPath, m.ContainerPath))
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

// removeArgs constructs the argv for forcibly removing a container by name,
// used to reap a container that outran its timeout.
func removeArgs(containerName string) []string {
	return []string{"rm", "-f", containerName}
}

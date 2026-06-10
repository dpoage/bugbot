package sandbox

import (
	"fmt"
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

// removeArgs constructs the argv for forcibly removing a container by name,
// used to reap a container that outran its timeout.
func removeArgs(containerName string) []string {
	return []string{"rm", "-f", containerName}
}

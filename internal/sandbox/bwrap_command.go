package sandbox

import (
	"fmt"
	"path/filepath"
	"strings"
)

// bwrapParams is the fully-resolved set of inputs to a single bwrap run,
// after backend defaults have been applied to a Spec. It mirrors runParams
// (command.go) so the two backends stay structurally comparable, but carries
// no containerName/image — bwrap has neither a daemon-tracked container nor
// a meaningful Spec.Image (see capabilities.go's fingerprint-keyed probe
// cache for how that's handled instead).
type bwrapParams struct {
	// workspace is the host path of the prepared rw workspace bound at
	// /workspace inside the sandbox.
	workspace string
	network   string
	env       []string
	cmd       []string
	// roMounts are extra read-only bind mounts (e.g. a dependency cache),
	// rendered after the fixed toolchain allowlist. Shared has no meaning for
	// bwrap (there is no SELinux relabeling concept) and is ignored.
	roMounts []ROMount
	// rwMounts are extra writable bind mounts: the trusted
	// dependency-prefetch step's cache dirs, and operator "writable: true"
	// local_mounts entries (bugbot-wjc2; see Spec.RWMounts).
	rwMounts []ROMount
	// setupCmds are optional in-sandbox commands run before cmd. When
	// non-empty the backend wraps execution in /bin/sh, exactly like
	// buildRunArgs (see Spec.SetupCmds).
	setupCmds [][]string
	// toolchainBinds are additional read-only binds resolved by the
	// host-toolchain resolver (bugbot-14g0) for toolchains beyond the fixed
	// allowlist below. Nil until that resolver is wired in; base
	// functionality (the fixed allowlist) works without it.
	toolchainBinds []ROMount
	// toolchainPathPrepend is ResolveHostToolchains' PathPrepend for the
	// same resolution that produced toolchainBinds: in-sandbox directories
	// to place at the front of PATH so resolved toolchain binaries are
	// actually reachable. Empty when no toolchains were configured/resolved.
	toolchainPathPrepend string
	// baselinePathAppend is the ":"-joined in-sandbox directories of the
	// resolved POSIX baseline utilities (resolveBwrapBaseline), appended
	// AFTER DefaultContainerPath — and after any caller-supplied PATH in
	// env — so core utilities stay reachable without ever shadowing
	// allowlist binaries or operator toolchains. Empty on FHS hosts.
	baselinePathAppend string
}

// fixedROAllowlist is the minimal, hardcoded set of host directories bound
// read-only into every bwrap sandbox so a bare-bones toolchain (a shell, libc,
// dynamic linker, core utilities) is reachable. This is intentionally narrow:
// broader binds (a whole $HOME, /root, or /etc) can exfiltrate host secrets
// through workspace -> transcript -> LLM even under network=none, since the
// untrusted command can read anything bound in and the sandbox output is fed
// back to a model. Toolchain-specific binds (go, node, python, ...) are
// layered on top via bwrapParams.toolchainBinds, never by widening this list.
//
// Bound with --ro-bind-try (not --ro-bind): non-FHS hosts (NixOS, Guix)
// genuinely lack /lib, /sbin, or even /bin as real paths — a strict --ro-bind
// on an absent path makes bwrap exit 1 before the sandboxed command ever
// runs, which previously failed EVERY run on such a host. A missing entry
// here just means that slice of the allowlist contributes nothing; it is
// still narrower than the container backend's baked image, exactly as
// intended.
//
// /etc/resolv.conf is deliberately absent here: it is added only when the
// network is enabled (see buildBwrapArgs), matching the container backend's
// contract that DNS resolution is unavailable under network=none.
//
// /nix/store, /gnu/store, and /etc/static exist for store-based distros
// (NixOS, Guix), where the FHS paths above are symlink farms into the store:
// /bin/sh -> /nix/store/...-bash/bin/sh. Binding /bin alone carries the
// SYMLINK into the sandbox but not its target, so exec fails with
// "execvp /bin/sh: No such file or directory" — which broke every
// SetupCmds-wrapped run and every /bin/sh-based capability probe on such
// hosts. Binding the store roots read-only resolves that class wholesale
// (shells, env, and the rpath lib closures of any store-resolved toolchain)
// without widening the secret-exfiltration surface: both stores are
// world-readable by design on their distros, so the sandboxed code gains no
// read access it would not already have running unsandboxed as the same
// user. /etc/static is NixOS's symlink-farm indirection into /nix/store
// (e.g. /etc/ssl/certs -> /etc/static/ssl/certs), needed so the /etc/ssl
// bind above resolves. On FHS hosts none of the three exist and the
// --ro-bind-try is a no-op.
var fixedROAllowlist = []string{
	"/usr",
	"/lib",
	"/lib64",
	"/bin",
	"/sbin",
	"/etc/ssl",
	"/etc/static",
	"/nix/store",
	"/gnu/store",
}

// buildBwrapArgs constructs the argv passed to the bwrap binary for a single
// run. It is a pure function so the security-relevant flag construction can
// be exercised in unit tests without bubblewrap or a Linux host.
//
// Security posture encoded here (defense in depth for untrusted, model-driven
// code — mirrors buildRunArgs' contract for the container backend):
//   - --unshare-all             : unshare every namespace (user, pid, net,
//     ipc, uts, cgroup) by default; --share-net is added back ONLY when the
//     resolved network mode explicitly enables it (never the "none" default).
//   - --die-with-parent         : bwrap's child is killed if bugbot itself
//     dies, so a crashed harness can never leave an orphaned sandboxed
//     process running.
//   - --new-session             : the sandboxed process gets its own session,
//     so it cannot inject input into (or steal signals from) bugbot's
//     controlling terminal.
//   - --clearenv + --setenv     : the sandbox starts with NO inherited host
//     environment; every variable it sees is explicit, mirroring --env on the
//     container backend. HOME=/tmp, USER=bugbot, and LOGNAME=bugbot are set
//     first as the sandbox's defaults, but p.env is rendered afterward and
//     DOES win on a repeat --setenv of any of them (bwrap's env map is
//     last-write-wins, exactly like buildRunArgs' --env HOME=/tmp — an
//     operator explicitly setting HOME/USER/LOGNAME in Spec.Env overrides the
//     default, not the other way around). USER/LOGNAME exist because the
//     tmpfs root has no /etc/passwd: bazel's client launcher hard-fails with
//     "FATAL: $USER is not set" without them, and POSIX tools generally
//     expect USER to be set (bugbot-wjc2).
//   - tmpfs / FIRST             : the root filesystem is an empty tmpfs,
//     established BEFORE any subpath (--proc, --dev, --tmpfs /tmp, the
//     allowlist, workspace) is bound — bwrap applies mount operations in
//     argv order within one shared mount namespace, so mounting "/" AFTER
//     something is already mounted at a subpath (e.g. /tmp) shadows that
//     subpath's mount entirely: the new root's own (empty) /tmp directory
//     wins, silently making the "earlier" /tmp completely inaccessible.
//     Getting this backwards previously made HOME=/tmp — and therefore
//     every toolchain cache that defaults under it (Go, npm, pip, ...) —
//     unusable in every real run without ever raising an error.
//   - --proc /proc, --dev /dev  : minimal, namespace-scoped pseudo-filesystems
//     (no host /proc or /dev is ever bound), layered onto the tmpfs root.
//   - --tmpfs /tmp              : writable scratch space for language
//     toolchain caches, sized like the container backend's /tmp tmpfs.
//   - --ro-bind-try allowlist   : ONLY the fixed allowlist (fixedROAllowlist)
//     plus any resolved toolchain/extra RO mounts are bound in, read-only —
//     best-effort (--ro-bind-try) since non-FHS hosts genuinely lack some
//     allowlist paths, see fixedROAllowlist's doc. No wholesale $HOME,
//     /root, or /etc bind ever happens.
//   - --bind ws /workspace      : the workspace copy is the ONLY writable
//     mount. The original repo is never mounted.
//   - --chdir /workspace        : run from the workspace.
//
// Resource caps (memory/CPU/pids) have no bwrap flag equivalent — bwrap has no
// cgroups of its own — so they are applied by the caller wrapping this argv in
// a systemd-run --user --scope or cgroup v2 invocation; see bwrap_caps.go.
func buildBwrapArgs(p bwrapParams) []string {
	args := []string{
		"--unshare-all",
		"--die-with-parent",
		"--new-session",
		"--clearenv",
		"--setenv", "HOME", "/tmp",
		"--setenv", "USER", "bugbot",
		"--setenv", "LOGNAME", "bugbot",
	}

	// Network defaults to unshared (set by --unshare-all above). Only an
	// explicitly enabling network mode restores it — "none" (the default) and
	// the empty string both stay unshared.
	if bwrapNetworkEnabled(p.network) {
		args = append(args, "--share-net")
	}

	// tmpfs root MUST be established before anything else is bound under it
	// (see the doc above) — everything not explicitly bound below is then
	// absent, not merely read-only. This is the bwrap analogue of
	// --read-only + --tmpfs /tmp on the container backend, except there is
	// no underlying image filesystem to fall back to at all.
	args = append(args, "--tmpfs", "/")

	args = append(args, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")

	for _, host := range fixedROAllowlist {
		args = append(args, "--ro-bind-try", host, host)
	}
	if bwrapNetworkEnabled(p.network) {
		// DNS resolution is meaningless (and unreachable) under the unshared
		// network namespace, so /etc/resolv.conf is bound only when network
		// access was explicitly requested.
		args = append(args, "--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf")
	}

	// Toolchain binds resolved beyond the fixed allowlist (host-toolchain
	// resolver, bugbot-14g0) are rendered next, still read-only.
	for _, m := range p.toolchainBinds {
		args = append(args, "--ro-bind", m.HostPath, m.ContainerPath)
	}

	// Operator-supplied extra mounts, same ordering contract as buildRunArgs:
	// read-only mounts first, in caller order, always read-only regardless of
	// Shared (bwrap has no SELinux relabeling concept, so Shared is a no-op
	// here — plain ro binds either way).
	for _, m := range p.roMounts {
		args = append(args, "--ro-bind", m.HostPath, m.ContainerPath)
	}
	for _, m := range p.rwMounts {
		args = append(args, "--bind", m.HostPath, m.ContainerPath)
	}

	// The writable workspace copy is the only writable mount, bound last so it
	// always wins if an operator-supplied mount collides on ContainerPath
	// (validateMounts already rejects intra-set duplicates; this is defense
	// in depth against a mount aimed at /workspace itself).
	args = append(args, "--bind", p.workspace, workspaceMount)
	args = append(args, "--chdir", workspaceMount)

	// PATH must be set explicitly: --clearenv wipes it along with everything
	// else, so without this every in-sandbox command would silently fall back
	// to the shell's compiled-in default path, which never includes resolved
	// toolchain directories. toolchainPathPrepend (from the host-toolchain
	// resolver's PathPrepend) goes first so resolved toolchains shadow any
	// same-named binary under the fixed allowlist; DefaultContainerPath (same
	// constant the container backend's toolchain wiring uses) is always the
	// tail so plain allowlisted binaries stay reachable even with no
	// toolchains configured; baselinePathAppend comes last so POSIX baseline
	// utilities are a pure fallback, never shadowing either. p.env is applied
	// after this, so an operator who sets PATH explicitly in Spec.Env still
	// wins — but see the env loop below: the baseline is re-appended to a
	// caller PATH too.
	path := DefaultContainerPath
	if p.toolchainPathPrepend != "" {
		path = p.toolchainPathPrepend + ":" + DefaultContainerPath
	}
	path = appendBaselinePath(path, p.baselinePathAppend)
	args = append(args, "--setenv", "PATH", path)

	// --clearenv leaves the sandbox with no environment at all; every
	// variable it sees must be set explicitly here, mirroring --env on the
	// container backend. HOME/USER/LOGNAME's defaults (set above) ARE
	// overridable here: an operator entry for any of them in p.env
	// (Spec.Env) renders a second --setenv, and bwrap's env map is
	// last-write-wins, so it wins —
	// same contract as buildRunArgs' --env HOME=/tmp. A malformed entry
	// (no "=") is dropped rather than passed to bwrap as a broken --setenv
	// invocation.
	//
	// PATH is the one variable that is rewritten rather than passed
	// verbatim: the POSIX baseline (see baselinePathAppend) is appended to
	// a caller-supplied PATH as well. Container images make core utilities
	// reachable under any PATH bugbot constructs because every internal
	// constructor ends with the DefaultContainerPath tail and images
	// populate those directories; on store-based hosts those directories
	// hold no utilities, so a caller-supplied value like the capability
	// prober's "<toolchains>:<default>" (engine.depProbeInputs)
	// would silently lose mkdir/grep/... without this append. The baseline
	// is a strict suffix — a caller PATH still shadows everything in it.
	for _, e := range p.env {
		key, value, ok := splitEnvKV(e)
		if !ok {
			continue
		}
		if key == "PATH" {
			value = appendBaselinePath(value, p.baselinePathAppend)
		}
		args = append(args, "--setenv", key, value)
	}

	if len(p.setupCmds) > 0 {
		script := buildSetupScript(p.setupCmds)
		args = append(args, "/bin/sh", "-c", script, "sh")
	}
	args = append(args, p.cmd...)
	return args
}

// bwrapNetworkEnabled reports whether network is an explicit opt-in to
// network access. Only "host" enables it; "none" and the empty string (which
// resolves to "none" as the package default) keep the network namespace
// unshared. Any other value is rejected earlier by validateBwrapNetwork.
func bwrapNetworkEnabled(network string) bool {
	return network == "host"
}

// splitEnvKV splits a KEY=VALUE environment entry. ok is false for a
// malformed entry (no "="), which the caller then drops rather than passing
// a broken --setenv invocation to bwrap.
func splitEnvKV(kv string) (key, value string, ok bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return "", "", false
}

// validateBwrapNetwork resolves and validates a Spec's network mode against
// the bwrap backend's narrower contract (acceptance criterion 2): only "none"
// (the default) and "host" are meaningful — there is no bwrap equivalent of
// the container backend's bridge/custom network modes, since --unshare-all
// either shares the host's single network namespace wholesale or not at all.
func validateBwrapNetwork(network string) (string, error) {
	switch network {
	case "", "none", "host":
		if network == "" {
			return "none", nil
		}
		return network, nil
	default:
		return "", fmt.Errorf("sandbox: bwrap backend supports network %q values none or host, got %q", "none/host", network)
	}
}

// bwrapAllowlistContainerPaths returns the fixed allowlist's container paths,
// used by validateBwrapMounts to reject an operator-supplied mount that would
// collide with (and silently shadow or be shadowed by) a fixed bind.
func bwrapAllowlistContainerPaths() map[string]bool {
	seen := make(map[string]bool, len(fixedROAllowlist)+1)
	for _, p := range fixedROAllowlist {
		seen[p] = true
	}
	seen[workspaceMount] = true
	return seen
}

// validateBwrapMounts extends validateMounts (command.go) with the one rule
// specific to bwrap's tmpfs-root model: no caller-supplied mount may target a
// path already claimed by the fixed allowlist or the workspace, since bwrap
// binds are applied in a fixed order and a collision would silently shadow
// (or be shadowed by) sandbox-critical content instead of raising the
// "duplicate mount" error validateMounts gives for two Spec-level mounts
// colliding with each other.
func validateBwrapMounts(ro, rw []ROMount) error {
	if err := validateMounts(ro, rw); err != nil {
		return err
	}
	reserved := bwrapAllowlistContainerPaths()
	check := func(mounts []ROMount, kind string) error {
		for _, m := range mounts {
			if reserved[filepath.Clean(m.ContainerPath)] {
				return fmt.Errorf("sandbox: %s mount container path %q collides with a fixed bwrap bind", kind, m.ContainerPath)
			}
		}
		return nil
	}
	if err := check(ro, "read-only"); err != nil {
		return err
	}
	return check(rw, "writable")
}

// appendBaselinePath appends the POSIX-baseline directories to a PATH value
// unless they are already its suffix (a repeated Spec.Env PATH entry, or a
// caller that composed the effective PATH itself). When no baseline
// resolved on this host (the FHS case), the incoming value is returned
// untouched, keeping standard-distro argv byte-identical.
func appendBaselinePath(current, baseline string) string {
	if baseline == "" {
		return current
	}
	if current == "" {
		return baseline
	}
	if current == baseline || strings.HasSuffix(current, ":"+baseline) {
		return current
	}
	return current + ":" + baseline
}

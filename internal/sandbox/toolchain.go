package sandbox

// toolchain.go implements host toolchain provisioning: resolving named
// toolchains (or explicit host directories) into read-only bind mounts, a
// PATH prefix, and provenance fingerprints. This generalizes the existing
// ROMount mechanism (previously used only for dependency caches, see
// deps.go's modcache/pipcache/... mounts) to host-installed TOOLCHAINS
// (node, python, cargo, ...) so a sandbox image that lacks a toolchain can
// still run it: the host demonstrably has every toolchain it needs to
// build/test its own targets daily, and mounting it read-only exposes that
// without granting the untrusted run write access or network egress.
//
// # Security posture
//
// Exactly the same as any other ROMount (see the package doc and
// ROMount.Shared): only PUBLIC, READ-ONLY content is exposed to the
// container — the resolved binary and the directory that holds its runtime
// dependencies (the symlink closure target's containing directory; for a
// nix-store or asdf/nvm shim layout this is normally already a
// self-contained, versioned directory). NEVER a secret or credential
// directory. Operators configuring sandbox.host_toolchains are exposing
// their own PATH's resolution of that name (or an explicit directory they
// name directly) to whatever untrusted, model-driven code the sandbox runs —
// audit accordingly, exactly as for sandbox.local_mounts.
//
// # Resolution algorithm
//
//  1. Each requested entry is either a bare name (resolved via the HOST's
//     `command -v <name>`, i.e. Go's exec.LookPath against the host PATH) or
//     an absolute directory path (used directly, no lookup — lets an
//     operator pin an exact toolchain install outside PATH, e.g. a specific
//     nix store path).
//  2. A resolved executable's symlink chain is followed to its final target
//     (filepath.EvalSymlinks) so nix/asdf/nvm shim layouts resolve to the
//     real toolchain directory rather than a one-file shim that would be
//     useless mounted alone.
//  3. The mounted root is the resolved target's containing directory, or that
//     directory's parent when the containing directory is named "bin" (the
//     common `<toolchain-root>/bin/<name>` layout) — this pulls in sibling
//     lib/ and share/ directories the runtime needs alongside the binary,
//     in one mount.
//  4. A provenance fingerprint (resolved host path + `<name> --version`,
//     run on the HOST, not in any sandbox) is recorded per toolchain.
//     Hermeticity is knowingly traded for provisioning correctness here; the
//     fingerprint is what keeps a verdict attributable to the exact host
//     toolchain build that produced it.
//
// # Contract for other sandbox backends
//
// ResolveHostToolchains is the single implementation of this algorithm, so
// resolution/fingerprinting/PATH handling never drifts between backends.
// Other Sandbox implementations (e.g. a bwrap backend) call it directly and
// translate ToolchainResolution.Mounts into their own bind-mount mechanism
// (this package's CLI backend renders them via Spec.ROMounts, same as any
// other read-only mount).
import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// hostToolchainMountRoot is the fixed container path prefix under which every
// resolved host toolchain is mounted, one subdirectory per requested entry.
// This keeps toolchain ContainerPaths from colliding with each other or with
// the dependency-cache mounts in deps.go (/modcache, /pipcache, ...).
const hostToolchainMountRoot = "/opt/bugbot-toolchains"

// defaultContainerPath is appended after any resolved toolchain bin
// directories when building the container's PATH override. It mirrors a
// standard Linux distribution's default PATH so images that already ship
// their own toolchains (and set no ENV PATH override) keep working exactly
// as before when sandbox.host_toolchains is unconfigured or resolves nothing.
const DefaultContainerPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// toolchainVersionProbeTimeout bounds the HOST-side `<bin> --version` probe
// used to build a fingerprint. This runs directly on the host (not inside any
// sandbox), so it must not be allowed to hang the caller on a broken binary.
const toolchainVersionProbeTimeout = 3 * time.Second

// ToolchainFingerprint records provenance for one resolved host toolchain
// mount: the host path actually mounted and the toolchain's reported version
// string. Recorded in run metadata (see repro artifact READMEs) so a
// demonstrated bug — or its absence — can be attributed to the exact host
// toolchain build that produced it.
type ToolchainFingerprint struct {
	// Name is the requested toolchain entry (e.g. "node") or, for an
	// explicit directory entry, the directory's base name.
	Name string
	// Path is the resolved host directory that was mounted read-only.
	Path string
	// Version is the toolchain's self-reported version string. Best-effort:
	// empty when the `--version` probe failed or does not apply.
	Version string
}

// ToolchainResolution is the result of resolving a set of named host
// toolchains (or explicit directories) for sandbox provisioning.
type ToolchainResolution struct {
	// Mounts are read-only bind mounts to add to a sandbox Spec (or a
	// backend's own bind-mount list). Each is Shared=true: these are
	// host-owned toolchain installs, not bugbot-owned dirs, so they must
	// never be SELinux :Z relabeled (see ROMount.Shared).
	Mounts []ROMount
	// PathPrepend is a ":"-joined, request-ordered list of in-container
	// directories (the containing directory of each resolved executable,
	// rewritten under its mount's ContainerPath) to place at the front of
	// PATH inside the container. Empty when no requested entry resolved to
	// an executable (e.g. every entry was an explicit non-executable dir).
	PathPrepend string
	// Fingerprints records provenance for each successfully resolved
	// toolchain, in request order. Entries that failed to resolve are
	// silently absent (see ResolveHostToolchains doc).
	Fingerprints []ToolchainFingerprint
}

// ResolveHostToolchains resolves each entry in names into a read-only bind
// mount, in-container PATH entry, and provenance fingerprint.
//
// An entry is either:
//   - a bare name (e.g. "node"): resolved via the host's PATH
//     (exec.LookPath), then its symlink closure is followed to the real
//     toolchain directory (see the file doc's resolution algorithm).
//   - an absolute directory path (starts with "/"): used directly as the
//     mounted root, no PATH lookup or symlink following.
//
// Resolution is best-effort per entry: a name that cannot be resolved on the
// host (not on PATH, dangling symlink, or a named directory that does not
// exist) is skipped with no error. This mirrors ROMount's existing
// best-effort ethos — a misconfigured entry degrades (the resulting
// CapabilitySet probe will legitimately report that ecosystem unavailable),
// it does not abort the run. Duplicate ContainerPaths (two entries resolving
// to the same mount) are de-duplicated; only the first is kept.
func ResolveHostToolchains(names []string) (ToolchainResolution, error) {
	var res ToolchainResolution
	var pathDirs []string
	seenContainerPaths := make(map[string]bool, len(names))

	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		root, execPath, err := resolveToolchainRoot(name)
		if err != nil {
			// Not resolvable on this host: skip, best-effort (see doc).
			continue
		}
		ctrPath := hostToolchainMountRoot + "/" + sanitizeToolchainSegment(name)
		if seenContainerPaths[ctrPath] {
			continue
		}
		seenContainerPaths[ctrPath] = true

		res.Mounts = append(res.Mounts, ROMount{
			HostPath:      root,
			ContainerPath: ctrPath,
			Shared:        true, // host-owned toolchain install; never :Z relabeled
		})

		if execPath != "" {
			binDir := ctrPath
			if rel, rerr := filepath.Rel(root, filepath.Dir(execPath)); rerr == nil && rel != "." {
				binDir = filepath.Join(ctrPath, rel)
			}
			pathDirs = append(pathDirs, binDir)
		}

		res.Fingerprints = append(res.Fingerprints, ToolchainFingerprint{
			Name:    name,
			Path:    root,
			Version: probeToolchainVersion(name, execPath),
		})
	}

	res.PathPrepend = strings.Join(pathDirs, ":")
	return res, nil
}

// resolveToolchainRoot resolves name to (mountedRootDir, resolvedExecPath).
// For an absolute-path entry, execPath is "" (no binary identified; the
// directory itself is mounted verbatim) and root is name, validated to exist
// and be a directory. For a bare name, root is the containing directory of
// the symlink-closure-resolved executable (see the file doc's step 3), and
// execPath is that resolved executable path.
func resolveToolchainRoot(name string) (root, execPath string, err error) {
	if filepath.IsAbs(name) {
		info, statErr := os.Stat(name)
		if statErr != nil || !info.IsDir() {
			return "", "", fmt.Errorf("sandbox: host toolchain dir %q not found: %w", name, statErr)
		}
		return name, "", nil
	}

	found, lookErr := exec.LookPath(name)
	if lookErr != nil {
		return "", "", fmt.Errorf("sandbox: host toolchain %q not on PATH: %w", name, lookErr)
	}
	resolved, evalErr := filepath.EvalSymlinks(found)
	if evalErr != nil {
		return "", "", fmt.Errorf("sandbox: resolve symlink closure for %q: %w", name, evalErr)
	}
	dir := filepath.Dir(resolved)
	root = dir
	if filepath.Base(dir) == "bin" {
		// Pull in the toolchain root (sibling lib/, share/, ...) alongside
		// the bin/ directory, not just the single binary's own folder — but
		// ONLY when that root is narrow (a version-manager's own versioned
		// directory, a nix store path, ...). A $HOME/bin/node or
		// ~/.local/bin/node layout would otherwise ascend straight to $HOME
		// or ~/.local and RO-mount the user's entire home directory (SSH
		// keys, git credentials, unrelated dotfiles) into whatever untrusted,
		// model-driven code the sandbox runs. See isOverbroadToolchainRoot.
		if candidate := filepath.Dir(dir); !isOverbroadToolchainRoot(candidate) {
			root = candidate
		}
	}
	return root, resolved, nil
}

// isOverbroadToolchainRoot reports whether dir is a shared, multi-purpose
// directory that must never be RO-mounted wholesale as a "toolchain root":
// the user's home directory itself, or a broad catch-all subdirectory like
// ~/.local that holds far more than one toolchain. A $HOME/bin/node or
// ~/.local/bin/node layout ascends exactly here without this guard.
//
// Narrow, single-purpose version-manager directories
// (~/.nvm/versions/node/vX, ~/.asdf/installs/..., a nix store path) are NOT
// caught by this — they are exactly the layout the ascent exists to support.
func isOverbroadToolchainRoot(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	home = filepath.Clean(home)
	dir = filepath.Clean(dir)
	if dir == home {
		return true
	}
	for _, broad := range []string{".local", ".config", ".cache", ".ssh"} {
		if dir == filepath.Join(home, broad) {
			return true
		}
	}
	return false
}

// sanitizeToolchainSegment reduces name to a single, safe path component for
// use under hostToolchainMountRoot: the base name only, so an absolute
// directory entry (e.g. "/nix/store/xxx-nodejs-18") mounts at a flat
// "/opt/bugbot-toolchains/xxx-nodejs-18" rather than a nested, traversal-prone
// path, and a bare name is used as-is (it is already a single component).
func sanitizeToolchainSegment(name string) string {
	base := filepath.Base(name)
	if base == "" || base == "." || base == "/" || base == string(filepath.Separator) {
		base = "toolchain"
	}
	return base
}

// probeToolchainVersion best-effort runs `<bin> --version` on the HOST — not
// inside any sandbox; this inspects the host toolchain being mounted, before
// any container exists — and returns its first output line, trimmed. Returns
// "" on any failure (binary rejects --version, times out, etc.); the
// fingerprint remains useful with just the resolved path in that case.
func probeToolchainVersion(name, execPath string) string {
	bin := execPath
	if bin == "" {
		bin = name
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolchainVersionProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line)
}

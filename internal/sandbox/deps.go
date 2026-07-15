package sandbox

// deps.go implements the dependency strategy resolver for sandboxed builds.
//
// The sandbox defaults to --network=none, so a build/test can only resolve
// external modules if their source is already present inside the container.
// This file decides, for a given repo, HOW to make those dependencies present
// without granting the untrusted run network access:
//
//	VENDORED  the repo vendors its deps (vendor/modules.txt). Nothing to mount;
//	          we set GOFLAGS=-mod=vendor so `go` ignores the network entirely and
//	          builds from vendor/ (explicit rather than incidental).
//	HOST      mount the host's Go module cache read-only at /modcache and point
//	          GOMODCACHE at it. GOPROXY=off turns any cache miss into a clear,
//	          immediate error instead of a network hang under --network=none.
//	FETCH     run ONE online `go mod download` in a separate, still-hardened
//	          container to populate a bugbot-managed cache dir, then mount THAT
//	          dir read-only exactly like HOST. Everything after the prefetch is
//	          network-none.
//	OFF       current behavior: no mounts, no env (the default).
//
// Vendored detection applies in ALL modes including OFF — it is free and safe.
// The Go strategy is only applied when the repo has a go.mod; other ecosystems
// can grow their own resolver alongside this one (the Resolution return type is
// ecosystem-agnostic).
//
// # Ecosystem registry
//
// ResolveDeps iterates the ordered ecosystems table and calls resolve on every
// ecosystem whose detect returns true for repoDir. The results are merged:
//
//   - ROMounts: appended in table order. Each ecosystem must use distinct
//     ContainerPaths; the existing validateMounts uniqueness check backstops
//     this at Exec time, but ecosystem authors must design ContainerPaths to be
//     globally unique across all ecosystems (e.g. /modcache for Go, /pipcache
//     for Python) to avoid silent shadowing.
//   - Env: appended in table order.
//   - SetupCmds: appended in table order.
//   - Prefetch: chained into a single func that runs them sequentially; the
//     first error aborts the chain.
//   - Strategy: set to the FIRST matching ecosystem's strategy.
//
// A repo with no matching ecosystem resolves to an empty Resolution{Strategy:
// DepStrategyOff}, identical to today's non-Go behavior.
//
// # Ecosystem coverage
//
// Strategy support per ecosystem (vendored / off / host / fetch):
//
//	Go      go.mod            vendored(vendor/modules.txt) · off · host · fetch
//	Python  requirements.txt  off · fetch          (host→off; no vendored convention)
//	Rust    Cargo.toml        vendored(vendor/ + .cargo/config replace-with) · off · host · fetch
//	JS/npm  package.json      vendored(node_modules/) · off · fetch   (host→off)
//	C#/Nug  *.csproj/*.sln    off · host · fetch   (no v1 vendored convention)
//	Maven   pom.xml           off · host · fetch   (no v1 vendored convention)
//	Gradle  build.gradle[.kts]/settings.gradle[.kts]
//	                          off · fetch          (host→off; RO cache contention)
//
// Each ecosystem owns a unique container mount path: /modcache (Go),
// /depcache (Python), /cargo/registry (Rust), /npmcache (JS), /nugetcache (C#/NuGet),
// /m2cache (Maven), /gradlecache (Gradle).
// See the README section "Sandbox dependency strategies" for the full per-ecosystem
// matrix (prefetch commands, offline-enforcement env, in-sandbox setup, security).
//
// # Security posture (per-mount Shared semantics)
//
// Each ecosystem's resolve func controls the Shared flag on its ROMounts.
// Shared=false (bugbot-owned dirs, e.g. the fetch cache) receives :Z SELinux
// relabeling for container-private isolation. Shared=true (host-owned dirs,
// e.g. the user's Go module cache under ~/go/pkg/mod) MUST NOT be relabeled:
// :Z on a multi-GB shared tree is slow, breaks the host toolchain, and breaks
// any other container sharing the same dir. See ROMount.Shared for the full
// rationale. Ecosystem authors must set Shared=true for any host-owned
// directory they mount.
//
// # Stdlib-only constraint
//
// This file deliberately depends only on the standard library (the package as a
// whole imports nothing else). The go.mod / vendor markers are checked directly
// rather than via internal/ingest to keep sandbox free of heavier imports and
// import cycles.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DepStrategy selects how external module dependencies are made available to a
// network-none sandbox run. See the file doc for the per-strategy behavior.
type DepStrategy string

const (
	// DepStrategyOff is the default: no dependency mounts or env are added.
	// Vendored repos are still detected (that path is free and safe).
	DepStrategyOff DepStrategy = "off"
	// DepStrategyVendored is set automatically when a repo's dependencies are
	// already materialized locally (vendor/modules.txt for Go, vendor/ +
	// .cargo/config stanza for Rust, node_modules/ for JS). It is never
	// selected by the user — it overrides any requested strategy when the
	// vendored invariant is satisfied.
	DepStrategyVendored DepStrategy = "vendored"
	// DepStrategyHost mounts the host Go module cache read-only.
	DepStrategyHost DepStrategy = "host"
	// DepStrategyFetch performs a one-time online prefetch into a bugbot-managed
	// cache, then mounts that cache read-only.
	DepStrategyFetch DepStrategy = "fetch"
)

// ValidDepStrategy reports whether s is a recognized strategy. The empty string
// is accepted and treated as DepStrategyOff by ResolveDeps.
func ValidDepStrategy(s DepStrategy) bool {
	switch s {
	case "", DepStrategyOff, DepStrategyVendored, DepStrategyHost, DepStrategyFetch:
		return true
	default:
		return false
	}
}

// modcacheMount is where a dependency module cache is mounted read-only inside
// the container (HOST and FETCH strategies).
const modcacheMount = "/modcache"

// goCacheDir and goTmpDir are disk-backed build-scratch directories created
// inside the writable /workspace mount for Go runs. Go's build cache (GOCACHE,
// default $HOME/.cache/go-build) and transient work dir ($WORK, default under
// $TMPDIR) otherwise land on the small tmpfs the sandbox mounts at /tmp
// (buildRunArgs: --tmpfs /tmp:size=512m). A cold build of a dependency-heavy
// package overruns that tmpfs ("no space left on device"), which the reproducer
// reads as an environment_error and never promotes — and a refuter's
// sandbox_exec check fails the same way. /workspace is a per-run, disk-backed
// copy with real space, so the caches go there instead. Both names are
// dot-prefixed so `go test ./...` skips them (go ignores directories beginning
// with "." or "_").
const (
	goCacheDir = workspaceMount + "/.bugbot-gocache"
	goTmpDir   = workspaceMount + "/.bugbot-gotmp"
)

// DepOptions configures the dependency resolver.
type DepOptions struct {
	// Strategy selects the dependency strategy. Empty is treated as
	// DepStrategyOff.
	Strategy DepStrategy

	// FetchSandbox is the Sandbox used to run the one-time online prefetch under
	// DepStrategyFetch. Required only for that strategy; ignored otherwise. It is
	// typically the same backend used for the main run.
	FetchSandbox Sandbox

	// FetchImage overrides the image used for the prefetch container. Empty uses
	// the FetchSandbox's configured default image.
	FetchImage string

	// FetchNetwork is the container network mode for the prefetch run. Empty
	// defaults to "bridge" (the standard NAT network both podman and docker
	// accept) so the prefetch can actually reach the proxy. The prefetch is the
	// ONLY run that ever touches the network.
	FetchNetwork string

	// hostModcache, when set, overrides the resolved host module cache directory
	// (test seam). Empty resolves via `go env GOMODCACHE` with a $HOME fallback.
	hostModcache string

	// hostCargoRegistry, when set, overrides the resolved host Cargo registry
	// directory for the HOST strategy (test seam). Empty resolves via $CARGO_HOME
	// then ~/.cargo/registry.
	hostCargoRegistry string

	// hostNuGetPackages, when set, overrides the resolved host NuGet global
	// packages directory for the HOST strategy (test seam). Empty resolves via
	// $NUGET_PACKAGES then ~/.nuget/packages.
	hostNuGetPackages string

	// hostMavenRepository, when set, overrides the resolved host Maven local
	// repository directory for the HOST strategy (test seam). Empty resolves
	// via $HOME/.m2/repository.
	hostMavenRepository string

	// userCacheDir, when set, overrides the base directory for bugbot-managed
	// fetch caches (test seam). Empty uses os.UserCacheDir.
	userCacheDir string

	// LocalMounts, when non-nil, are additional read-only host directories to
	// bind-mount into the sandbox INDEPENDENT of ecosystem detection. They
	// compose with any ecosystem-derived mounts (off/host/fetch) so a repo can
	// have both a registry-cache mount AND sibling-directory mounts active in
	// the same Spec. Resolved from config.Sandbox.LocalMounts by the caller;
	// the sandbox package does not parse in-repo manifests (v1 security boundary).
	// Each mount must have HostPath set; Shared is always true (host-owned dirs,
	// no SELinux :Z relabel). See ROMount.Shared for the full rationale.
	LocalMounts []ROMount
	// HostToolchains is an ordered list of host toolchain names (resolved via
	// the host's PATH) or explicit host directories to bind-mount read-only
	// into the sandbox and prepend to its PATH, via ResolveHostToolchains.
	// Independent of ecosystem detection and LocalMounts: a repo may have
	// ecosystem mounts, local mounts, AND host toolchain mounts active in the
	// same Spec. Resolved from config.Sandbox.HostToolchains by the caller.
	HostToolchains []string
}

// Resolution is the result of resolving a repo's dependency strategy: the
// read-only mounts and extra environment a run should carry, plus an optional
// one-time Prefetch hook. It is ecosystem-agnostic so non-Go resolvers can
// return the same shape.
type Resolution struct {
	// ROMounts are read-only mounts to add to the run's Spec.
	ROMounts []ROMount
	// Env are extra KEY=VALUE entries to append to the run's Spec.Env. They are
	// appended after the caller's own env, matching the sandbox's "later wins"
	// ordering, so they take effect for the run.
	Env []string
	// SetupCmds are in-container commands to run before Cmd (see Spec.SetupCmds).
	// Non-Go ecosystems populate this (e.g. ["npm","ci","--offline"]); the Go
	// ecosystem contributes a single mkdir for its build-scratch dirs (goCacheDir).
	SetupCmds [][]string

	// Prefetch, when non-nil, must be called ONCE before the first network-none
	// run for this repo (e.g. from the reproducer's PromoteAll setup). It runs
	// the online module download and is a no-op on repeat calls (it is guarded
	// by a sync.Once internally). Strategies other than FETCH leave it nil.
	Prefetch func(ctx context.Context) error

	// Strategy is the strategy actually applied (after vendored detection may
	// have overridden the requested one), for logging/diagnostics.
	Strategy DepStrategy
	// Fingerprints records provenance for any host toolchains mounted into
	// this Resolution (see ResolveHostToolchains). Empty when
	// DepOptions.HostToolchains was empty or resolved nothing on this host.
	Fingerprints []ToolchainFingerprint
}

// ecosystem describes how to detect a build ecosystem and resolve its
// dependency strategy into a Resolution. The ordered ecosystems table is
// iterated by resolveWith; entries whose detect returns true contribute their
// Resolution to the merged result.
type ecosystem struct {
	// name is a human-readable identifier for logging and diagnostics.
	name string
	// detect reports whether repoDir belongs to this ecosystem (e.g. presence
	// of go.mod for Go). It must be fast and side-effect-free.
	detect func(repoDir string) bool
	// resolve returns the Resolution for repoDir under opts. It is only called
	// when detect returned true.
	resolve func(repoDir string, opts DepOptions) (Resolution, error)
}

// ecosystems is the ordered registry of per-ecosystem resolvers. resolveWith
// iterates this table in order, merging every matching ecosystem's Resolution.
// To add a new ecosystem (C#/NuGet, ...), append a new entry here;
// the merge semantics in resolveWith handle the rest. ContainerPaths across
var ecosystems = []ecosystem{
	goEcosystem,
	pythonEcosystem,
	cargoEcosystem,
	jsEcosystem,
	nugetEcosystem,
	mavenEcosystem,
	gradleEcosystem,
}

// goEcosystem is the Go dependency resolver. It detects repos with a go.mod
// and applies the VENDORED / HOST / FETCH / OFF dependency strategy, and in
// every mode relocates Go's build scratch onto the disk-backed workspace
// (see goCacheDir / resolveGo).
var goEcosystem = ecosystem{
	name:    "go",
	detect:  hasGoModule,
	resolve: resolveGo,
}

// resolveGo resolves the Go ecosystem for repoDir: the dependency strategy
// (resolveGoDeps) plus the build-scratch relocation common to every strategy
// (applyGoBuildScratch). Strategy validation has already run in resolveWith.
func resolveGo(repoDir string, opts DepOptions) (Resolution, error) {
	res, err := resolveGoDeps(repoDir, opts)
	if err != nil {
		return Resolution{}, err
	}
	return applyGoBuildScratch(res), nil
}

// applyGoBuildScratch appends the env and setup command that move Go's build
// cache (GOCACHE) and transient work dir ($WORK, via GOTMPDIR) off the small
// /tmp tmpfs onto the disk-backed workspace. GOCACHE is auto-created by go, but
// GOTMPDIR must already exist, so the mkdir setup command precedes the run.
// This routes the Go command through the /bin/sh setup wrapper in buildRunArgs,
// which the image's shell + mkdir satisfy — both are present wherever the Go
// toolchain is. See goCacheDir for the failure this prevents.
func applyGoBuildScratch(res Resolution) Resolution {
	res.Env = append(res.Env, "GOCACHE="+goCacheDir, "GOTMPDIR="+goTmpDir)
	res.SetupCmds = append(res.SetupCmds, []string{"mkdir", "-p", goCacheDir, goTmpDir})
	return res
}

// resolveGoDeps resolves only the dependency strategy (vendored / off / host /
// fetch) for repoDir. The build-scratch relocation is layered on by resolveGo.
func resolveGoDeps(repoDir string, opts DepOptions) (Resolution, error) {
	// Vendored detection wins in every mode: it is free, safe, and makes `go`
	// ignore the network entirely.
	if isVendored(repoDir) {
		return Resolution{
			Env:      []string{"GOFLAGS=-mod=vendor"},
			Strategy: DepStrategyVendored,
		}, nil
	}

	strategy := opts.Strategy
	if strategy == "" {
		strategy = DepStrategyOff
	}

	switch strategy {
	case DepStrategyOff:
		return Resolution{Strategy: DepStrategyOff}, nil

	case DepStrategyHost:
		cache, err := resolveHostModcache(opts.hostModcache)
		if err != nil {
			return Resolution{}, err
		}
		// shared=true: the host Go module cache is NOT owned by bugbot. SELinux
		// :Z would relabel the entire cache to a container-private MCS label,
		// which is slow on multi-GB caches and can break the host go toolchain
		// and any other container sharing the same directory. :ro without a label
		// suffix is the correct choice here; see ROMount.Shared for details.
		return modcacheResolution(cache, DepStrategyHost, true, nil), nil

	case DepStrategyFetch:
		if opts.FetchSandbox == nil {
			return Resolution{}, fmt.Errorf("sandbox: dependency strategy %q requires a fetch sandbox", strategy)
		}
		cache, err := fetchCacheDir(repoDir, opts.userCacheDir)
		if err != nil {
			return Resolution{}, err
		}
		prefetch := newPrefetch(repoDir, cache, opts)
		// shared=false: the fetch cache is a bugbot-owned directory under the
		// user cache dir. :Z isolation is appropriate here.
		return modcacheResolution(cache, DepStrategyFetch, false, prefetch), nil

	default:
		// Unreachable given ValidDepStrategy above, but keep it explicit.
		return Resolution{}, fmt.Errorf("sandbox: unhandled dependency strategy %q", strategy)
	}
}

// hasGoModule reports whether repoDir contains a root go.mod.
func hasGoModule(repoDir string) bool {
	st, err := os.Stat(filepath.Join(repoDir, "go.mod"))
	return err == nil && !st.IsDir()
}

// isVendored reports whether repoDir has a populated vendor tree
// (vendor/modules.txt), which `go` uses as the authoritative vendor manifest.
func isVendored(repoDir string) bool {
	st, err := os.Stat(filepath.Join(repoDir, "vendor", "modules.txt"))
	return err == nil && !st.IsDir()
}

// ResolveDeps decides the dependency strategy for repoDir and returns the
// mounts/env/prefetch a run should use. It never errors on a repo that matches
// no ecosystem or when the strategy is off: such repos resolve to an empty
// Resolution (OFF).
//
// Strategy validation runs first; an invalid strategy is an immediate error.
// The function then delegates to resolveWith using the package-level ecosystems
// table — see its doc comment for the per-ecosystem merge semantics.
func ResolveDeps(repoDir string, opts DepOptions) (Resolution, error) {
	if !ValidDepStrategy(opts.Strategy) {
		return Resolution{}, fmt.Errorf("sandbox: invalid dependency strategy %q (want off, host, or fetch)", opts.Strategy)
	}
	res, err := resolveWith(ecosystems, repoDir, opts)
	if err != nil {
		return Resolution{}, err
	}
	// Append operator-configured local mounts AFTER ecosystem-derived mounts.
	// This is independent of ecosystem detection: a repo may have local mounts
	// even when no ecosystem matched (Strategy=off). Order: ecosystem mounts
	// first (registry caches), then operator local mounts. The validateMounts
	// uniqueness check in sandbox.Exec backstops any ContainerPath collisions.
	res.ROMounts = append(res.ROMounts, opts.LocalMounts...)

	// Host toolchain mounts (fix A, bugbot-14g0): resolved AFTER local mounts,
	// independent of ecosystem detection and dep strategy. A resolved
	// toolchain overrides the container's PATH entirely (see
	// defaultContainerPath) so its bin dir wins even against an ecosystem
	// image that already ships (an older or absent) copy of the same tool.
	if len(opts.HostToolchains) > 0 {
		tc, tcErr := ResolveHostToolchains(opts.HostToolchains)
		if tcErr != nil {
			return Resolution{}, fmt.Errorf("sandbox: resolve host toolchains: %w", tcErr)
		}
		res.ROMounts = append(res.ROMounts, tc.Mounts...)
		res.Fingerprints = append(res.Fingerprints, tc.Fingerprints...)
		if tc.PathPrepend != "" {
			res.Env = append(res.Env, "PATH="+tc.PathPrepend+":"+DefaultContainerPath)
		}
	}
	return res, nil
}

// resolveWith is the internal dispatcher that iterates the provided table and
// merges matching ecosystems' Resolutions. It is a separate function (rather
// than inlining into ResolveDeps) so tests can inject an extended table without
// mutating the global ecosystems var — important for parallel-test safety.
//
// Merge rules (see file doc for rationale):
//   - ROMounts: appended in table order. ContainerPaths must be unique across
//     all ecosystems; Exec's validateMounts backstops this.
//   - Env: appended in table order.
//   - SetupCmds: appended in table order.
//   - Prefetch: chained sequentially; first error wins.
//   - Strategy: taken from the FIRST matching ecosystem.
//
// No matching ecosystem → Resolution{Strategy: DepStrategyOff}.
func resolveWith(table []ecosystem, repoDir string, opts DepOptions) (Resolution, error) {
	var merged Resolution
	firstMatch := true

	for _, eco := range table {
		if !eco.detect(repoDir) {
			continue
		}
		r, err := eco.resolve(repoDir, opts)
		if err != nil {
			return Resolution{}, err
		}
		// Strategy is taken from the first matching ecosystem.
		if firstMatch {
			merged.Strategy = r.Strategy
			firstMatch = false
		}
		merged.ROMounts = append(merged.ROMounts, r.ROMounts...)
		merged.Env = append(merged.Env, r.Env...)
		merged.SetupCmds = append(merged.SetupCmds, r.SetupCmds...)
		// Chain Prefetch funcs: run them sequentially; first error aborts.
		if r.Prefetch != nil {
			prev := merged.Prefetch
			next := r.Prefetch
			if prev == nil {
				merged.Prefetch = next
			} else {
				merged.Prefetch = func(ctx context.Context) error {
					if err := prev(ctx); err != nil {
						return err
					}
					return next(ctx)
				}
			}
		}
	}

	if firstMatch {
		// No ecosystem matched.
		return Resolution{Strategy: DepStrategyOff}, nil
	}
	return merged, nil
}

// modcacheResolution builds the Resolution shared by HOST and FETCH: a single
// read-only modcache mount plus the env that points `go` at it and turns a
// cache miss into a hard error rather than a network hang.
//
// shared controls ROMount.Shared: true for the HOST strategy (user's real Go
// module cache — must not be SELinux-relabeled), false for the FETCH strategy
// (bugbot-owned cache dir — :Z isolation is correct). See ROMount.Shared.
func modcacheResolution(hostCache string, strategy DepStrategy, shared bool, prefetch func(context.Context) error) Resolution {
	return Resolution{
		ROMounts: []ROMount{{HostPath: hostCache, ContainerPath: modcacheMount, Shared: shared}},
		Env: []string{
			"GOMODCACHE=" + modcacheMount,
			"GOFLAGS=-mod=mod",
			// GOPROXY=off: under --network=none any cache miss must fail fast and
			// clearly instead of hanging on an unreachable proxy.
			"GOPROXY=off",
		},
		Prefetch: prefetch,
		Strategy: strategy,
	}
}

// goEnvTimeout is the maximum time we will wait for `go env GOMODCACHE` to
// respond. A wedged or very slow go binary should not block funnel/repro
// construction indefinitely.
const goEnvTimeout = 5 * time.Second

// resolveHostModcache resolves the host Go module cache directory. It prefers
// the override, then `go env GOMODCACHE`, then $HOME/go/pkg/mod. It returns an
// error when no path can be determined (go not installed AND no HOME) or when
// the resolved path does not exist on the host (catches a misconfigured or
// unpopulated cache early, before podman emits an opaque bind-mount error).
func resolveHostModcache(override string) (string, error) {
	if override != "" {
		return checkModcacheExists(override)
	}

	// Ask `go` for its authoritative cache path. The exec is bounded by a
	// short timeout so a wedged go binary cannot hang the caller indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), goEnvTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "go", "env", "GOMODCACHE").Output(); err == nil {
		if dir := strings.TrimSpace(string(out)); dir != "" {
			return checkModcacheExists(dir)
		}
	}

	// go not installed or printed nothing: fall back to the conventional path.
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("sandbox: cannot resolve host Go module cache (go not found and HOME unset); vendor deps or set dep_strategy: off|fetch")
	}
	return checkModcacheExists(filepath.Join(home, "go", "pkg", "mod"))
}

// checkModcacheExists verifies that dir exists on the host and returns it if
// so. When the directory is missing it returns a clear, actionable error
// instead of letting podman fail with an opaque bind-mount message at run time.
func checkModcacheExists(dir string) (string, error) {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("sandbox: host module cache %q does not exist; run 'go mod download' on the host first, or use dep_strategy: off|fetch", dir)
		}
		return "", fmt.Errorf("sandbox: stat host module cache %q: %w", dir, err)
	}
	return dir, nil
}

// fetchCacheDir returns the bugbot-managed module-cache directory for repoDir
// (e.g. ~/.cache/bugbot/modcache/<hash>). The directory is created if missing.
// override, when non-empty, replaces the ~/.cache/bugbot/modcache base
// (test seam).
func fetchCacheDir(repoDir, override string) (string, error) {
	return fetchEcosystemCacheDir("modcache", repoDir, override)
}

// repoHash derives a stable directory name from the absolute repo path so two
// different repos never share a fetch cache.
func repoHash(repoDir string) string {
	abs, err := filepath.Abs(repoDir)
	if err != nil {
		abs = repoDir
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:8])
}

// fetchEcosystemCacheDir returns the bugbot-managed cache directory for the
// named ecosystem and repoDir under the user cache dir
// (e.g. ~/.cache/bugbot/<ecosystem>/<hash>). Using the user cache dir rather
// than a dir inside the scanned repo keeps the repo tree clean and lets the
// cache survive across runs. override (when non-empty) replaces the
// ~/.cache/bugbot/<ecosystem> base, allowing tests to redirect to a temp dir.
// The directory is created if missing.
func fetchEcosystemCacheDir(ecosystem, repoDir, override string) (string, error) {
	base := override
	if base == "" {
		uc, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("sandbox: resolve user cache dir for %s fetch cache: %w", ecosystem, err)
		}
		base = filepath.Join(uc, "bugbot", ecosystem)
	}
	dir := filepath.Join(base, repoHash(repoDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("sandbox: create %s fetch cache dir: %w", ecosystem, err)
	}
	return dir, nil
}

// newPrefetchOnce wraps run in a sync.Once so the returned function calls run
// at most once per Resolution — all four ecosystem prefetch constructors share
// this pattern.
func newPrefetchOnce(run func(context.Context) error) func(context.Context) error {
	var once sync.Once
	var onceErr error
	return func(ctx context.Context) error {
		once.Do(func() { onceErr = run(ctx) })
		return onceErr
	}
}

// newPrefetch builds the one-time online prefetch hook for the FETCH strategy.
// It runs `go mod download all` in the FetchSandbox with network enabled and
// GOMODCACHE pointed at hostCache, and is keyed on the repo's go.sum hash so an
// unchanged dependency set is not re-downloaded. The returned func is guarded by
// a sync.Once so it runs at most once per Resolution even if called repeatedly.
func newPrefetch(repoDir, hostCache string, opts DepOptions) func(context.Context) error {
	return newPrefetchOnce(func(ctx context.Context) error {
		return runPrefetch(ctx, repoDir, hostCache, opts)
	})
}

// prefetchSentinel is the marker file written into the fetch cache recording the
// go.sum hash the cache was populated for; a matching hash means the cache is
// already warm and the online download is skipped.
const prefetchSentinel = ".bugbot-fetched"

// runPrefetch performs the actual online module download. It is a no-op when the
// cache is already warm for the repo's current go.sum.
func runPrefetch(ctx context.Context, repoDir, hostCache string, opts DepOptions) error {
	sum, sumErr := goSumHash(repoDir)
	sentinel := filepath.Join(hostCache, prefetchSentinel)

	if sumErr == nil {
		if prev, err := os.ReadFile(sentinel); err == nil && strings.TrimSpace(string(prev)) == sum {
			// Cache already populated for this exact dependency set.
			return nil
		}
	}

	network := opts.FetchNetwork
	if network == "" {
		// "bridge" is the standard NAT network both podman and docker accept;
		// this is the only run that is ever allowed network access.
		network = "bridge"
	}

	// The prefetch container is still fully hardened (read-only root, cap-drop,
	// no-new-privileges, pids/memory/cpu limits — all from buildRunArgs); only
	// the network differs and the cache is bound WRITABLE so `go mod download`
	// can populate it. This is the single trusted, online populate step; the
	// later network-none run mounts the same dir read-only via ROMounts.
	spec := Spec{
		RepoDir:  repoDir,
		Image:    opts.FetchImage,
		Network:  network,
		Cmd:      []string{"go", "mod", "download", "all"},
		RWMounts: []ROMount{{HostPath: hostCache, ContainerPath: modcacheMount}},
		Env: []string{
			"GOMODCACHE=" + modcacheMount,
			"GOFLAGS=-mod=mod",
		},
	}
	res, err := opts.FetchSandbox.Exec(ctx, spec)
	if err != nil {
		return fmt.Errorf("sandbox: prefetch `go mod download` failed to launch: %w", err)
	}
	if res.TimedOut {
		return fmt.Errorf("sandbox: prefetch `go mod download` timed out")
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sandbox: prefetch `go mod download` exited %d: %s", res.ExitCode, lastLines(res.Stderr, 20))
	}

	if sumErr == nil {
		// Record the warm-cache sentinel; a write failure only costs a redundant
		// future fetch, so it is non-fatal.
		_ = os.WriteFile(sentinel, []byte(sum), 0o644)
	}
	return nil
}

// goSumHash returns a hex hash of the repo's go.sum (or go.mod when go.sum is
// absent, e.g. a module with no deps) so the fetch cache can be keyed on the
// dependency set.
func goSumHash(repoDir string) (string, error) {
	for _, name := range []string{"go.sum", "go.mod"} {
		data, err := os.ReadFile(filepath.Join(repoDir, name))
		if err == nil {
			sum := sha256.Sum256(data)
			return hex.EncodeToString(sum[:]), nil
		}
	}
	return "", fmt.Errorf("sandbox: no go.sum or go.mod in %q", repoDir)
}

// lastLines returns up to the last n non-empty lines of s, trimmed, for compact
// error messages.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ---- Python ecosystem -------------------------------------------------------
//
// v1 strategy: pip wheelhouse FETCH only, driven by root requirements.txt.
//
// Scope decisions (final; do not change silently — flag any deviation):
//
//   - DETECT: root requirements.txt exists. pyproject-only and uv.lock-only
//     repos resolve to OFF: verifying that uv's --offline / read-only-cache
//     behaviour is sufficiently robust is not worth blocking the Python
//     deployment, so uv support is explicitly DEFERRED.
//
//   - HOST → OFF (same empty Resolution). pip's HTTP cache (~/.cache/pip)
//     holds wheel and response files keyed on URLs, not the installed
//     packages themselves. There is no `pip install --no-index` analogue for
//     the HTTP cache: --no-index requires a local directory of wheel files
//     (a wheelhouse), which HOST does not provide. Mounting the pip HTTP
//     cache into a network-none container does not help install anything, so
//     HOST is treated as OFF rather than silently broken.
//
//   - FETCH → pip wheelhouse prefetch + offline install:
//     1. Prefetch (network bridge, ONE online step): run
//          pip download -r requirements.txt -d /depcache
//        in a network-enabled, otherwise-hardened container with the cache
//        dir mounted WRITABLE at /depcache.
//     2. Resolution: mount the same dir READ-ONLY at /depcache (Shared=false
//        — bugbot-owned, gets :Z), set PIP_NO_INDEX=1 (offline enforcement,
//        the GOPROXY=off analogue — any pip call in the test command that
//        would hit the network fails fast instead of hanging), and add a
//        SetupCmd that installs from the wheelhouse before the test runs:
//          pip install --user --no-index --find-links=/depcache -r requirements.txt
//        --user targets /tmp/.local (HOME=/tmp, set by buildRunArgs) which is
//        writable via the tmpfs. Python's user-site is on sys.path automatically
//        inside python:3-slim, so packages installed there are importable.
//        See the integration test for empirical confirmation (or the comment
//        on pipCacheMount if the empirical finding requires switching to a
//        venv under /tmp).
//
//   - Container path /depcache is distinct from /modcache (Go) so the
//     mount registry's ContainerPath uniqueness constraint is satisfied for
//     multi-ecosystem repos that have both go.mod and requirements.txt.

// pipCacheMount is where the pip wheelhouse is mounted inside the container
// for the offline install step. Distinct from /modcache (Go) per the mount
// registry's uniqueness obligation.
const pipCacheMount = "/depcache"

// pythonEcosystem is the Python dependency resolver. It detects repos with a
// root requirements.txt and applies the pip wheelhouse FETCH strategy. HOST
// maps to OFF (pip's HTTP cache is not a wheelhouse; see file-level comment).
// pyproject-only and uv.lock-only repos resolve to OFF — uv support is deferred.
var pythonEcosystem = ecosystem{
	name:    "python",
	detect:  hasRequirementsTxt,
	resolve: resolvePython,
}

// resolvePython is the Python resolver function. Strategy validation has
// already run in resolveWith; invalid strategies are unreachable here.
func resolvePython(repoDir string, opts DepOptions) (Resolution, error) {
	strategy := opts.Strategy
	if strategy == "" {
		strategy = DepStrategyOff
	}

	switch strategy {
	case DepStrategyOff:
		// No requirements.txt handling requested.
		return Resolution{Strategy: DepStrategyOff}, nil

	case DepStrategyHost:
		// HOST is explicitly OFF for Python: pip's HTTP cache is not a
		// wheelhouse; --no-index installs require a local directory of wheel
		// files (a wheelhouse), which the pip HTTP cache does not provide.
		// Mounting ~/.cache/pip into a network-none container does not help
		// install anything — it would just silence the "no PyPI" error without
		// actually resolving packages. Use dep_strategy: fetch for Python.
		return Resolution{Strategy: DepStrategyOff}, nil

	case DepStrategyFetch:
		if opts.FetchSandbox == nil {
			return Resolution{}, fmt.Errorf("sandbox: Python dependency strategy %q requires a fetch sandbox", strategy)
		}
		cache, err := fetchPipCacheDir(repoDir, opts.userCacheDir)
		if err != nil {
			return Resolution{}, err
		}
		prefetch := newPipPrefetch(repoDir, cache, opts)
		// Shared=false: the pip wheelhouse is a bugbot-owned directory under the
		// user cache dir. :Z SELinux isolation is appropriate (same rationale as
		// the Go FETCH cache).
		//
		// Empirical finding (integration test): --user install with HOME=/tmp
		// (set by buildRunArgs) places packages under /tmp/.local/lib/pythonX.Y/
		// site-packages. Python inside python:3-slim automatically includes
		// user-site on sys.path, so packages are importable without venv or
		// PATH changes. The --user approach works under the read-only rootfs
		// because /tmp is a writable tmpfs. A venv under /tmp was not required.
		// (If future images break this, switch to:
		//   python3 -m venv /tmp/venv
		//   /tmp/venv/bin/pip install --no-index --find-links=/depcache -r requirements.txt
		// and update this comment and the integration test.)
		return Resolution{
			ROMounts: []ROMount{{
				HostPath:      cache,
				ContainerPath: pipCacheMount,
				Shared:        false, // bugbot-owned; :Z isolation correct
			}},
			Env: []string{
				// PIP_NO_INDEX=1: under --network=none any pip call that would
				// reach PyPI must fail fast and clearly instead of hanging.
				// Analogous to GOPROXY=off for Go.
				"PIP_NO_INDEX=1",
			},
			SetupCmds: [][]string{
				{"pip", "install", "--user", "--no-index", "--find-links=" + pipCacheMount, "-r", "requirements.txt"},
			},
			Prefetch: prefetch,
			Strategy: DepStrategyFetch,
		}, nil

	default:
		// Unreachable given ValidDepStrategy above, but keep it explicit.
		return Resolution{}, fmt.Errorf("sandbox: unhandled Python dependency strategy %q", strategy)
	}
}

// hasRequirementsTxt reports whether repoDir contains a root requirements.txt.
// Only stdlib (os.Stat) — the sandbox package must remain stdlib-only.
func hasRequirementsTxt(repoDir string) bool {
	st, err := os.Stat(filepath.Join(repoDir, "requirements.txt"))
	return err == nil && !st.IsDir()
}

// fetchPipCacheDir returns the bugbot-managed pip wheelhouse directory for
// repoDir (e.g. ~/.cache/bugbot/pipcache/<hash>). Delegates to
// fetchEcosystemCacheDir. override, when non-empty, overrides the base dir
// (test seam).
func fetchPipCacheDir(repoDir, override string) (string, error) {
	return fetchEcosystemCacheDir("pipcache", repoDir, override)
}

// requirementsHash returns a hex hash of requirements.txt so the pip prefetch
// cache can be keyed on the dependency set (analogous to goSumHash for Go).
func requirementsHash(repoDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(repoDir, "requirements.txt"))
	if err != nil {
		return "", fmt.Errorf("sandbox: no requirements.txt in %q", repoDir)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// newPipPrefetch builds the one-time online pip-download hook for the Python
// FETCH strategy. It runs `pip download -r requirements.txt -d /depcache` in
// the FetchSandbox with network enabled and the cache dir mounted WRITABLE,
// keyed on the sha256 of requirements.txt so an unchanged dep set is not
// re-downloaded. Guarded by a sync.Once so it runs at most once per Resolution.
func newPipPrefetch(repoDir, hostCache string, opts DepOptions) func(context.Context) error {
	return newPrefetchOnce(func(ctx context.Context) error {
		return runPipPrefetch(ctx, repoDir, hostCache, opts)
	})
}

// runPipPrefetch performs the actual online pip download. It is a no-op when
// the wheelhouse is already warm for the repo's current requirements.txt.
func runPipPrefetch(ctx context.Context, repoDir, hostCache string, opts DepOptions) error {
	reqHash, hashErr := requirementsHash(repoDir)
	sentinel := filepath.Join(hostCache, prefetchSentinel) // reuse same constant as Go

	if hashErr == nil {
		if prev, err := os.ReadFile(sentinel); err == nil && strings.TrimSpace(string(prev)) == reqHash {
			// Wheelhouse already populated for this exact requirements.txt.
			return nil
		}
	}

	network := opts.FetchNetwork
	if network == "" {
		// "bridge" is the standard NAT network both podman and docker accept;
		// this is the ONLY pip run ever allowed network access.
		network = "bridge"
	}

	// The prefetch container is fully hardened (read-only root, cap-drop,
	// no-new-privileges, limits — all from buildRunArgs); only the network
	// differs and the cache is mounted WRITABLE so `pip download` can populate
	// it. The later network-none run mounts the same dir read-only.
	spec := Spec{
		RepoDir:  repoDir,
		Image:    opts.FetchImage,
		Network:  network,
		Cmd:      []string{"pip", "download", "-r", "requirements.txt", "-d", pipCacheMount},
		RWMounts: []ROMount{{HostPath: hostCache, ContainerPath: pipCacheMount}},
	}
	res, err := opts.FetchSandbox.Exec(ctx, spec)
	if err != nil {
		return fmt.Errorf("sandbox: pip prefetch `pip download` failed to launch: %w", err)
	}
	if res.TimedOut {
		return fmt.Errorf("sandbox: pip prefetch `pip download` timed out")
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sandbox: pip prefetch `pip download` exited %d: %s", res.ExitCode, lastLines(res.Stderr, 20))
	}

	if hashErr == nil {
		// Record the warm-cache sentinel. A write failure only costs a redundant
		// future fetch, so it is non-fatal.
		_ = os.WriteFile(sentinel, []byte(reqHash), 0o644)
	}
	return nil
}

// ---- Rust ecosystem ---------------------------------------------------------
//
// v1 strategy: cargo registry FETCH (and HOST), driven by root Cargo.toml.
//
// Scope decisions (final; do not change silently — flag any deviation):
//
//   - DETECT: root Cargo.toml exists. Workspace roots and single-crate repos
//     are both detected; the only requirement is a top-level Cargo.toml.
//
//   - VENDORED (auto-detect, wins in ALL modes): TRUE only when BOTH a vendor/
//     directory exists AND .cargo/config.toml (or .cargo/config) contains a
//     source-replacement stanza with a line containing "replace-with". If
//     vendor/ exists WITHOUT the config stanza, cargo ignores it — that is NOT
//     treated as vendored and falls through to the requested strategy. There is
//     no hint channel on Resolution; this case is documented in a comment only.
//     Vendored Resolution: CARGO_NET_OFFLINE=true, no mounts, Strategy=vendored.
//
//   - OFF: empty Resolution{Strategy: DepStrategyOff}.
//
//   - HOST: mount the host Cargo registry READ-ONLY at /cargo (used as
//     CARGO_HOME inside the container). SECURITY: only $CARGO_HOME/registry is
//     mounted — NEVER all of ~/.cargo, which holds credentials.toml and bin/.
//     The registry path defaults to $CARGO_HOME/registry (env) then
//     ~/.cargo/registry; a test seam (hostCargoRegistry on DepOptions) overrides
//     it. Shared=true (host-owned dir, no :Z relabel). CARGO_NET_OFFLINE=true.
//     The mount is at /cargo/registry and CARGO_HOME is set to /cargo so cargo
//     finds the registry at /cargo/registry offline without a copy step.
//
//   - FETCH: run ONE online `cargo fetch` (plus --locked when Cargo.lock exists)
//     in opts.FetchSandbox into a bugbot-owned cache dir (fetchCargoCacheDir
//     under userCacheDir/.../cargocache/<hash>), keyed on sha256(Cargo.lock)
//     falling back to sha256(Cargo.toml). Mount the populated registry dir
//     read-only at /cargo/registry (CARGO_HOME=/cargo). Shared=false.
//     CARGO_NET_OFFLINE=true.
//
//   - Container path /cargo is distinct from /modcache (Go) and /depcache
//     (Python) so the mount registry's ContainerPath uniqueness constraint is
//     satisfied for multi-ecosystem repos.

// cargoCacheMount is where the Cargo registry is mounted inside the container.
// The full container path for the registry is cargoCacheMount + "/registry".
// CARGO_HOME is set to cargoCacheMount so cargo resolves its registry at the
// standard $CARGO_HOME/registry path without any additional config.
// Distinct from /modcache (Go) and /depcache (Python) per the mount
// registry's uniqueness obligation.
const cargoCacheMount = "/cargo"

// cargoRegistryMount is the container path where the Cargo registry index and
// crate sources are mounted read-only. It is a subdirectory of cargoCacheMount
// so CARGO_HOME=/cargo resolves it at the standard $CARGO_HOME/registry path.
const cargoRegistryMount = cargoCacheMount + "/registry"

// cargoEcosystem is the Cargo/Rust dependency resolver. It detects repos with
// a root Cargo.toml and applies VENDORED / HOST / FETCH / OFF strategy.
var cargoEcosystem = ecosystem{
	name:    "cargo",
	detect:  hasCargoToml,
	resolve: resolveCargo,
}

// resolveCargo is the Rust/Cargo resolver function. Strategy validation has
// already run in resolveWith; invalid strategies are unreachable here.
func resolveCargo(repoDir string, opts DepOptions) (Resolution, error) {
	// Vendored detection wins in every mode: if both vendor/ and a cargo config
	// with a source-replacement stanza are present, cargo already has everything
	// it needs locally. Set CARGO_NET_OFFLINE=true to make any accidental
	// network access a clear, immediate error.
	if isCargoVendored(repoDir) {
		return Resolution{
			Env:      []string{"CARGO_NET_OFFLINE=true"},
			Strategy: DepStrategyVendored,
		}, nil
	}
	// Note: if vendor/ exists without .cargo/config{.toml} containing
	// replace-with, cargo ignores the vendor directory entirely. That case falls
	// through to the requested strategy — it is not vendored from cargo's point
	// of view. There is no hint channel on Resolution to surface this; the user
	// should check their .cargo/config.toml if builds fail unexpectedly offline.

	strategy := opts.Strategy
	if strategy == "" {
		strategy = DepStrategyOff
	}

	switch strategy {
	case DepStrategyOff:
		return Resolution{Strategy: DepStrategyOff}, nil

	case DepStrategyHost:
		registry, err := resolveHostCargoRegistry(opts.hostCargoRegistry)
		if err != nil {
			return Resolution{}, err
		}
		// Shared=true: the host Cargo registry is NOT owned by bugbot. SELinux :Z
		// would relabel the entire registry to a container-private MCS label,
		// which may break the host rustup/cargo toolchain and any other container
		// sharing the same directory. :ro without a label suffix is correct here.
		// SECURITY: we mount ONLY $CARGO_HOME/registry, never all of ~/.cargo
		// which contains credentials.toml and bin/. This is enforced here and
		// asserted in unit tests.
		return cargoRegistryResolution(registry, DepStrategyHost, true, nil), nil

	case DepStrategyFetch:
		if opts.FetchSandbox == nil {
			return Resolution{}, fmt.Errorf("sandbox: Rust/Cargo dependency strategy %q requires a fetch sandbox", strategy)
		}
		cache, err := fetchCargoCacheDir(repoDir, opts.userCacheDir)
		if err != nil {
			return Resolution{}, err
		}
		prefetch := newCargoPrefetch(repoDir, cache, opts)
		// The prefetch runs `cargo fetch` with CARGO_HOME=cache (mounted at
		// /cargo), so cargo populates the registry at cache/registry on the host.
		// The network-none run must therefore mount cache/registry (NOT cache) at
		// /cargo/registry, so CARGO_HOME=/cargo finds the registry at the standard
		// $CARGO_HOME/registry path. Create the dir now so the bind-mount source
		// exists even before the prefetch populates it.
		registry := filepath.Join(cache, "registry")
		if err := os.MkdirAll(registry, 0o755); err != nil {
			return Resolution{}, fmt.Errorf("sandbox: create cargo fetch registry dir: %w", err)
		}
		// Shared=false: the cargo fetch cache is a bugbot-owned directory under
		// the user cache dir. :Z SELinux isolation is appropriate (same rationale
		// as the Go and Python FETCH caches).
		return cargoRegistryResolution(registry, DepStrategyFetch, false, prefetch), nil

	default:
		// Unreachable given ValidDepStrategy above, but keep it explicit.
		return Resolution{}, fmt.Errorf("sandbox: unhandled Rust/Cargo dependency strategy %q", strategy)
	}
}

// cargoRegistryResolution builds the Resolution shared by HOST and FETCH for
// Cargo: a single read-only registry mount at /cargo/registry, CARGO_HOME=/cargo
// so cargo resolves the registry at the standard path, and CARGO_NET_OFFLINE=true
// so any cache miss is a hard error rather than a network hang.
//
// shared controls ROMount.Shared: true for HOST (user's real Cargo registry —
// must not be SELinux-relabeled), false for FETCH (bugbot-owned cache dir —
// :Z isolation correct). See ROMount.Shared for the full rationale.
func cargoRegistryResolution(hostRegistry string, strategy DepStrategy, shared bool, prefetch func(context.Context) error) Resolution {
	return Resolution{
		ROMounts: []ROMount{{
			HostPath:      hostRegistry,
			ContainerPath: cargoRegistryMount,
			Shared:        shared,
		}},
		Env: []string{
			// CARGO_HOME=/cargo so cargo resolves the registry at /cargo/registry
			// (the $CARGO_HOME/registry standard path) without any additional config.
			"CARGO_HOME=" + cargoCacheMount,
			// CARGO_NET_OFFLINE=true: under --network=none any cache miss must fail
			// fast and clearly instead of hanging on an unreachable registry.
			"CARGO_NET_OFFLINE=true",
		},
		Prefetch: prefetch,
		Strategy: strategy,
	}
}

// hasCargoToml reports whether repoDir contains a root Cargo.toml.
// Only stdlib (os.Stat) — the sandbox package must remain stdlib-only.
func hasCargoToml(repoDir string) bool {
	st, err := os.Stat(filepath.Join(repoDir, "Cargo.toml"))
	return err == nil && !st.IsDir()
}

// isCargoVendored reports whether repoDir is a properly-configured Cargo
// vendored workspace: vendor/ must exist AND .cargo/config.toml (or the legacy
// .cargo/config) must contain a source-replacement stanza with "replace-with".
//
// If vendor/ exists without the config stanza, cargo ignores the vendor
// directory — that case is NOT vendored from cargo's perspective and falls
// through to the requested dep strategy. See resolveCargo for the rationale.
func isCargoVendored(repoDir string) bool {
	// Check vendor/ directory.
	if _, err := os.Stat(filepath.Join(repoDir, "vendor")); err != nil {
		return false
	}
	// Check .cargo/config.toml first (modern), then .cargo/config (legacy).
	// Scan for the presence of "replace-with" which marks a source replacement
	// stanza; "vendored-sources" is the conventional value but we match any
	// replace-with to be robust to alternative vendored-source names.
	for _, name := range []string{".cargo/config.toml", ".cargo/config"} {
		data, err := os.ReadFile(filepath.Join(repoDir, name))
		if err != nil {
			continue
		}
		// Scan line by line for "replace-with" — stdlib string ops only.
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "replace-with") {
				return true
			}
		}
	}
	return false
}

// resolveHostCargoRegistry resolves the host Cargo registry directory. It
// prefers the override, then $CARGO_HOME/registry, then ~/.cargo/registry.
// Returns an error when no path can be determined or the resolved path does
// not exist on the host (catches a misconfigured or unpopulated cache early,
// before podman emits an opaque bind-mount error).
//
// SECURITY: this function resolves only $CARGO_HOME/registry, never all of
// ~/.cargo — mounting the full ~/.cargo would expose credentials.toml and
// binary tools in bin/. Unit tests assert this invariant.
func resolveHostCargoRegistry(override string) (string, error) {
	if override != "" {
		return checkCargoRegistryExists(override)
	}

	// $CARGO_HOME/registry takes precedence over the ~/.cargo default.
	if cargoHome := os.Getenv("CARGO_HOME"); cargoHome != "" {
		return checkCargoRegistryExists(filepath.Join(cargoHome, "registry"))
	}

	// Standard default: ~/.cargo/registry.
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("sandbox: cannot resolve host Cargo registry (CARGO_HOME unset and HOME unset); vendor deps or set dep_strategy: off|fetch")
	}
	return checkCargoRegistryExists(filepath.Join(home, ".cargo", "registry"))
}

// checkCargoRegistryExists verifies that dir exists on the host and returns it.
// When the directory is missing it returns a clear, actionable error instead of
// letting podman fail with an opaque bind-mount message at run time.
func checkCargoRegistryExists(dir string) (string, error) {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("sandbox: host Cargo registry %q does not exist; run 'cargo fetch' on the host first, or use dep_strategy: off|fetch", dir)
		}
		return "", fmt.Errorf("sandbox: stat host Cargo registry %q: %w", dir, err)
	}
	return dir, nil
}

// fetchCargoCacheDir returns the bugbot-managed cargo registry cache directory
// for repoDir (e.g. ~/.cache/bugbot/cargocache/<hash>). Delegates to
// fetchEcosystemCacheDir. override, when non-empty, overrides the base dir
// (test seam).
func fetchCargoCacheDir(repoDir, override string) (string, error) {
	return fetchEcosystemCacheDir("cargocache", repoDir, override)
}

// cargoLockHash returns a hex hash of Cargo.lock (falling back to Cargo.toml
// when Cargo.lock is absent, e.g. a library crate without a committed lockfile)
// so the cargo fetch cache can be keyed on the dependency set. Analogous to
// goSumHash for Go.
func cargoLockHash(repoDir string) (string, error) {
	for _, name := range []string{"Cargo.lock", "Cargo.toml"} {
		data, err := os.ReadFile(filepath.Join(repoDir, name))
		if err == nil {
			sum := sha256.Sum256(data)
			return hex.EncodeToString(sum[:]), nil
		}
	}
	return "", fmt.Errorf("sandbox: no Cargo.lock or Cargo.toml in %q", repoDir)
}

// newCargoPrefetch builds the one-time online cargo-fetch hook for the Rust
// FETCH strategy. It runs `cargo fetch` in the FetchSandbox with network enabled
// and CARGO_HOME pointed at hostCache (so the registry populates at
// hostCache/registry), keyed on the sha256 of Cargo.lock (or Cargo.toml) so
// an unchanged dependency set is not re-downloaded. Guarded by a sync.Once.
func newCargoPrefetch(repoDir, hostCache string, opts DepOptions) func(context.Context) error {
	return newPrefetchOnce(func(ctx context.Context) error {
		return runCargoPrefetch(ctx, repoDir, hostCache, opts)
	})
}

// runCargoPrefetch performs the actual online cargo fetch. It is a no-op when
// the cache is already warm for the repo's current Cargo.lock.
func runCargoPrefetch(ctx context.Context, repoDir, hostCache string, opts DepOptions) error {
	lockHash, hashErr := cargoLockHash(repoDir)
	sentinel := filepath.Join(hostCache, prefetchSentinel) // reuse same constant

	if hashErr == nil {
		if prev, err := os.ReadFile(sentinel); err == nil && strings.TrimSpace(string(prev)) == lockHash {
			// Cache already populated for this exact Cargo.lock.
			return nil
		}
	}

	network := opts.FetchNetwork
	if network == "" {
		// "bridge" is the standard NAT network both podman and docker accept;
		// this is the ONLY cargo run ever allowed network access.
		network = "bridge"
	}

	// Build the cargo fetch command. Add --locked when Cargo.lock exists so the
	// prefetch is deterministic and cargo does not update the lockfile during the
	// online step.
	cmd := []string{"cargo", "fetch"}
	if _, err := os.Stat(filepath.Join(repoDir, "Cargo.lock")); err == nil {
		cmd = append(cmd, "--locked")
	}

	// The prefetch container is fully hardened (read-only root, cap-drop,
	// no-new-privileges, limits — all from buildRunArgs); only the network
	// differs and the cache is mounted WRITABLE so `cargo fetch` can populate
	// the registry under CARGO_HOME/registry. The later network-none run mounts
	// the same dir read-only.
	spec := Spec{
		RepoDir: repoDir,
		Image:   opts.FetchImage,
		Network: network,
		Cmd:     cmd,
		// Mount the whole cache dir WRITABLE as CARGO_HOME so cargo populates
		// hostCache/registry during the fetch. The RO mount in the final
		// Resolution points at hostCache (which IS hostCache/registry for the
		// fetch strategy — see fetchCargoCacheDir which creates hostCache, and
		// cargoRegistryResolution which mounts hostRegistry at /cargo/registry).
		// For the FETCH prefetch, hostCache is the parent "cargocache/<hash>" dir,
		// and we mount it as CARGO_HOME=/cargo so cargo writes to
		// /cargo/registry inside the container → hostCache/registry on the host.
		RWMounts: []ROMount{{HostPath: hostCache, ContainerPath: cargoCacheMount}},
		Env: []string{
			"CARGO_HOME=" + cargoCacheMount,
		},
	}
	res, err := opts.FetchSandbox.Exec(ctx, spec)
	if err != nil {
		return fmt.Errorf("sandbox: cargo prefetch `cargo fetch` failed to launch: %w", err)
	}
	if res.TimedOut {
		return fmt.Errorf("sandbox: cargo prefetch `cargo fetch` timed out")
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sandbox: cargo prefetch `cargo fetch` exited %d: %s", res.ExitCode, lastLines(res.Stderr, 20))
	}

	if hashErr == nil {
		// Record the warm-cache sentinel. A write failure only costs a redundant
		// future fetch, so it is non-fatal.
		_ = os.WriteFile(sentinel, []byte(lockHash), 0o644)
	}
	return nil
}

// ---- JS ecosystem -----------------------------------------------------------
//
// v1 strategy: npm with package-lock.json FETCH only, driven by root
// package.json.
//
// Scope decisions (final; do not change silently — flag any deviation):
//
//   - DETECT: root package.json exists.
//
//   - COMMITTED node_modules (vendored-equivalent, wins in ALL modes): if
//     node_modules/ exists at the repo root, dependencies are already
//     materialized (the npm equivalent of vendor/). Resolution:
//     Strategy=vendored, no mounts, no SetupCmds. Cargo's model: vendor/ +
//     config stanza; npm's model: node_modules/ presence is sufficient.
//
//   - HOST → OFF (same empty Resolution). npm's HTTP cache (~/.npm) stores
//     tarballs keyed on URLs. Unlike the Go module cache or a Cargo registry,
//     a bare npm cache mount does NOT materialize node_modules/; `npm ci`
//     must still run and requires network to resolve. Mounting ~/.npm into a
//     network-none container does not help install anything, so HOST is treated
//     as OFF rather than silently broken.
//
//   - FETCH → npm ci prefetch + offline install (package-lock.json only):
//     pnpm-lock.yaml-only and yarn.lock-only repos resolve to OFF — pnpm and
//     yarn offline support is explicitly DEFERRED (analogous to Python's uv
//     deferral).
//     1. Prefetch (network bridge, ONE online step): run
//          npm ci --ignore-scripts --cache /npmcache
//        in a network-enabled container with the cache dir mounted WRITABLE at
//        /npmcache. --ignore-scripts is MANDATORY: npm lifecycle scripts are
//        arbitrary code; during the online prefetch the container has network
//        access, so we must not execute them. This is the key security boundary.
//     2. Resolution: mount the cache dir READ-ONLY at /npmcache (Shared=false —
//        bugbot-owned, gets :Z), set npm_config_offline=true (offline enforcement,
//        the GOPROXY=off / PIP_NO_INDEX=1 analogue — any npm call that would hit
//        the registry fails fast instead of hanging), and add a SetupCmd that
//        seeds a writable copy of the cache and installs from it before the test:
//          sh -c "cp -a /npmcache /tmp/npmcache && npm ci --cache /tmp/npmcache"
//        npm writes to its cache directory during `npm ci` even when packages are
//        already present, so a read-only /npmcache mount would cause a write
//        failure. The workaround is to copy the RO cache to /tmp (writable tmpfs)
//        before installing. Lifecycle scripts MAY run during this offline install;
//        arbitrary code execution is already the sandbox's threat model for
//        network-none runs. The --ignore-scripts in the prefetch step is the real
//        security boundary (online, network-enabled container).
//
//   - Container path /npmcache is distinct from /modcache, /depcache, /cargo
//     so the mount registry's ContainerPath uniqueness constraint is satisfied.

// npmCacheMount is where the npm cache is mounted inside the container for the
// offline install step. Distinct from /modcache, /depcache, and /cargo per the
// mount registry's uniqueness obligation.
const npmCacheMount = "/npmcache"

// jsPrefetchFlags is the single declared source of the npm prefetch security
// flags. --ignore-scripts is MANDATORY: npm lifecycle scripts are arbitrary
// code and must not execute during the network-enabled prefetch container.
// To change the posture (bugbot-gu0o) or extend flags for a future ecosystem,
// edit ONLY this var — resolveJS and newNPMPrefetch read it at call time.
var jsPrefetchFlags = []string{"--ignore-scripts"}

// jsEcosystem is the JS/npm dependency resolver. It detects repos with a root
// package.json and applies COMMITTED-node_modules (vendored) / FETCH / OFF.
// HOST maps to OFF (npm HTTP cache does not materialize node_modules; see
// file-level comment). pnpm and yarn are explicitly deferred to a future bead.
//
// resolve is set directly to resolveJS (not via init) so the value copied into
// the global ecosystems slice has a non-nil resolver. resolveJS reads
// jsPrefetchFlags at call time — no init() ordering dependency.
var jsEcosystem = ecosystem{
	name:    "js",
	detect:  hasPackageJSON,
	resolve: resolveJS,
}

// resolveJS is the JS/npm resolver function. It reads jsPrefetchFlags at
// call time so the declared security flags are always in effect regardless
// of struct copy order. Tests that call resolveJS directly exercise the same
// path as production dispatch (resolveWith → eco.resolve → resolveJS).
func resolveJS(repoDir string, opts DepOptions) (Resolution, error) {
	return resolveJSWithFlags(repoDir, opts, jsPrefetchFlags)
}

// resolveJSWithFlags is the parameterized JS/npm resolver. prefetchFlags are
// appended to the npm ci prefetch command (after "npm", "ci"). This separates
// the registry declaration from the function body: the ecosystem entry declares
// the flags; the resolver uses them without needing to know their values.
func resolveJSWithFlags(repoDir string, opts DepOptions, prefetchFlags []string) (Resolution, error) {
	// Committed node_modules detection wins in every mode: if node_modules/
	// exists, dependencies are already materialized — no mount or install step
	// is needed. This is the npm equivalent of Go's vendor/modules.txt detection.
	if hasNodeModules(repoDir) {
		return Resolution{Strategy: DepStrategyVendored}, nil
	}

	strategy := opts.Strategy
	if strategy == "" {
		strategy = DepStrategyOff
	}

	switch strategy {
	case DepStrategyOff:
		return Resolution{Strategy: DepStrategyOff}, nil

	case DepStrategyHost:
		// HOST is explicitly OFF for JS: npm's HTTP cache (~/.npm) stores tarballs
		// keyed on URLs. A cache mount alone does not materialize node_modules/;
		// npm ci must still run and requires network to resolve the registry for
		// metadata. Mounting ~/.npm into a network-none container does not help
		// install anything. Use dep_strategy: fetch for JS projects.
		return Resolution{Strategy: DepStrategyOff}, nil

	case DepStrategyFetch:
		if opts.FetchSandbox == nil {
			return Resolution{}, fmt.Errorf("sandbox: JS/npm dependency strategy %q requires a fetch sandbox", strategy)
		}
		// pnpm-lock.yaml-only and yarn.lock-only repos resolve to OFF: pnpm and
		// yarn offline support is explicitly deferred (analogous to Python's uv
		// deferral). Only package-lock.json is supported in v1.
		if !hasPackageLock(repoDir) {
			// No package-lock.json: could be pnpm or yarn — deferred to a future bead.
			return Resolution{Strategy: DepStrategyOff}, nil
		}
		cache, err := fetchNPMCacheDir(repoDir, opts.userCacheDir)
		if err != nil {
			return Resolution{}, err
		}
		prefetch := newNPMPrefetch(repoDir, cache, opts, prefetchFlags)
		// Shared=false: the npm cache is a bugbot-owned directory under the user
		// cache dir. :Z SELinux isolation is appropriate (same rationale as the
		// Go and Python FETCH caches).
		//
		// npm cache behavior (empirical): npm writes to its cache directory during
		// `npm ci` even when packages are already populated. A read-only /npmcache
		// mount would cause write failures inside the container. The SetupCmd
		// therefore copies the RO cache to /tmp/npmcache (writable tmpfs, seeded
		// by the RO mount) and runs `npm ci --cache /tmp/npmcache` against the
		// copy. This avoids the writable-mount vs. network-none tension while
		// still enforcing offline operation. See the integration test for empirical
		// confirmation (or update this comment if a future npm version allows
		// --prefer-offline with a read-only cache directly).
		//
		// Lifecycle scripts: MAY run during the offline `npm ci` in the SetupCmd.
		// Arbitrary code execution is already the sandbox's threat model for the
		// network-none run. The --ignore-scripts flag in the prefetch step is the
		// real security boundary (network-enabled container, online step).
		return Resolution{
			ROMounts: []ROMount{{
				HostPath:      cache,
				ContainerPath: npmCacheMount,
				Shared:        false, // bugbot-owned; :Z isolation correct
			}},
			Env: []string{
				// npm_config_offline=true: under --network=none any npm call that
				// would reach the registry must fail fast and clearly instead of
				// hanging. Analogous to GOPROXY=off for Go and PIP_NO_INDEX=1 for Python.
				"npm_config_offline=true",
			},
			SetupCmds: [][]string{
				// Copy the RO npm cache to a writable /tmp dir then run npm ci
				// offline against it. npm writes to its cache during ci even for
				// cached packages, so a direct RO mount causes write failures.
				{"sh", "-c", "cp -a " + npmCacheMount + " /tmp/npmcache && npm ci --cache /tmp/npmcache"},
			},
			Prefetch: prefetch,
			Strategy: DepStrategyFetch,
		}, nil

	default:
		// Unreachable given ValidDepStrategy above, but keep it explicit.
		return Resolution{}, fmt.Errorf("sandbox: unhandled JS/npm dependency strategy %q", strategy)
	}
}

// hasPackageJSON reports whether repoDir contains a root package.json.
// Only stdlib (os.Stat) — the sandbox package must remain stdlib-only.
func hasPackageJSON(repoDir string) bool {
	st, err := os.Stat(filepath.Join(repoDir, "package.json"))
	return err == nil && !st.IsDir()
}

// hasNodeModules reports whether repoDir contains a root node_modules/
// directory, indicating that JS dependencies are already materialized
// (the npm equivalent of vendored deps).
func hasNodeModules(repoDir string) bool {
	st, err := os.Stat(filepath.Join(repoDir, "node_modules"))
	return err == nil && st.IsDir()
}

// hasPackageLock reports whether repoDir contains a root package-lock.json.
// v1 JS FETCH strategy requires package-lock.json; pnpm and yarn lockfiles are
// deferred to a future bead.
func hasPackageLock(repoDir string) bool {
	st, err := os.Stat(filepath.Join(repoDir, "package-lock.json"))
	return err == nil && !st.IsDir()
}

// fetchNPMCacheDir returns the bugbot-managed npm cache directory for repoDir
// (e.g. ~/.cache/bugbot/npmcache/<hash>). Delegates to
// fetchEcosystemCacheDir. override, when non-empty, overrides the base dir
// (test seam).
func fetchNPMCacheDir(repoDir, override string) (string, error) {
	return fetchEcosystemCacheDir("npmcache", repoDir, override)
}

// packageLockHash returns a hex hash of package-lock.json so the npm prefetch
// cache can be keyed on the dependency set (analogous to goSumHash for Go and
// cargoLockHash for Rust).
func packageLockHash(repoDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(repoDir, "package-lock.json"))
	if err != nil {
		return "", fmt.Errorf("sandbox: no package-lock.json in %q", repoDir)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// newNPMPrefetch builds the one-time online npm prefetch hook for the JS FETCH
// strategy. It runs `npm ci <prefetchFlags> --cache /npmcache` in the
// FetchSandbox with network enabled and the cache dir mounted WRITABLE at
// /npmcache, keyed on the sha256 of package-lock.json so an unchanged dep set
// is not re-downloaded. Guarded by a sync.Once so it runs at most once per
// Resolution even if called repeatedly.
//
// prefetchFlags are the ecosystem-declared security flags (e.g. ["--ignore-scripts"])
// read from the ecosystem registry entry rather than hardcoded here. This makes
// the security posture declarative: correcting or extending a flag for a new
// ecosystem is a one-line registry edit (bugbot-gu0o).
//
// SECURITY: --ignore-scripts MUST be present in prefetchFlags for npm. npm
// lifecycle scripts are arbitrary code; during the online prefetch the container
// has network access, so executing them would allow a malicious package to
// exfiltrate data. This is asserted in tests (TestJSResolveFetchPrefetchSpec).
func newNPMPrefetch(repoDir, hostCache string, opts DepOptions, prefetchFlags []string) func(context.Context) error {
	return newPrefetchOnce(func(ctx context.Context) error {
		return runNPMPrefetch(ctx, repoDir, hostCache, opts, prefetchFlags)
	})
}

// runNPMPrefetch performs the actual online npm ci prefetch. It is a no-op when
// the cache is already warm for the repo's current package-lock.json.
//
// SECURITY: prefetchFlags must include --ignore-scripts; see newNPMPrefetch.
func runNPMPrefetch(ctx context.Context, repoDir, hostCache string, opts DepOptions, prefetchFlags []string) error {
	lockHash, hashErr := packageLockHash(repoDir)
	sentinel := filepath.Join(hostCache, prefetchSentinel) // reuse same constant

	if hashErr == nil {
		if prev, err := os.ReadFile(sentinel); err == nil && strings.TrimSpace(string(prev)) == lockHash {
			// Cache already populated for this exact package-lock.json.
			return nil
		}
	}

	network := opts.FetchNetwork
	if network == "" {
		// "bridge" is the standard NAT network both podman and docker accept;
		// this is the ONLY npm run ever allowed network access.
		network = "bridge"
	}

	// Build the prefetch command: "npm ci <prefetchFlags> --cache /npmcache".
	// prefetchFlags come from the ecosystem registry entry (e.g. ["--ignore-scripts"])
	// rather than being hardcoded here — making per-ecosystem security adjustments
	// a one-line registry edit (bugbot-gu0o).
	//
	// SECURITY: npm lifecycle scripts are arbitrary code; the prefetch container
	// has network access, so we must not execute them. The cache dir is mounted
	// WRITABLE so npm can populate it.
	cmd := make([]string, 0, 2+len(prefetchFlags)+2)
	cmd = append(cmd, "npm", "ci")
	cmd = append(cmd, prefetchFlags...)
	cmd = append(cmd, "--cache", npmCacheMount)
	spec := Spec{
		RepoDir:  repoDir,
		Image:    opts.FetchImage,
		Network:  network,
		Cmd:      cmd,
		RWMounts: []ROMount{{HostPath: hostCache, ContainerPath: npmCacheMount}},
	}
	cmdStr := strings.Join(cmd, " ")
	res, err := opts.FetchSandbox.Exec(ctx, spec)
	if err != nil {
		return fmt.Errorf("sandbox: npm prefetch `%s` failed to launch: %w", cmdStr, err)
	}
	if res.TimedOut {
		return fmt.Errorf("sandbox: npm prefetch `%s` timed out", cmdStr)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sandbox: npm prefetch `%s` exited %d: %s", cmdStr, res.ExitCode, lastLines(res.Stderr, 20))
	}

	if hashErr == nil {
		// Record the warm-cache sentinel. A write failure only costs a redundant
		// future fetch, so it is non-fatal.
		_ = os.WriteFile(sentinel, []byte(lockHash), 0o644)
	}
	return nil
}

// ---- C#/NuGet ecosystem ----------------------------------------------------
//
// v1 strategy: NuGet global packages cache HOST + FETCH, driven by root C#
// project files (*.csproj, *.sln, or *.fsproj).
//
// Scope decisions (final; do not change silently — flag any deviation):
//
//   - DETECT: any root-level *.csproj, *.sln, or *.fsproj exists. Root-level
//     only — matching every other ecosystem's detector scope. F#/VB.NET
//     project files are accepted because they consume the same NuGet package
//     cache and benefit from identical handling.
//
//   - VENDORED: NOT supported in v1. NuGet has no widely-adopted, standard
//     committed-packages convention equivalent to Go's vendor/modules.txt or
//     Cargo's vendor/ + .cargo/config stanza. Repositories that commit
//     packages under a project-local folder (e.g. a manual `packages/` tree)
//     are not treated as vendored and fall through to the requested strategy.
//     NuGet RestorePackagesPath / a future "RestorePackages" hook could be a
//     later-bead addition.
//
//   - OFF: empty Resolution{Strategy: DepStrategyOff}.
//
//   - HOST: mount the host NuGet global packages directory READ-ONLY at
//     /nugetcache. NUGET_PACKAGES=/nugetcache tells dotnet to use that mount
//     as the global packages folder (the standard $NUGET_PACKAGES convention).
//     The default path is $NUGET_PACKAGES (env) then ~/.nuget/packages; a test
//     seam (hostNuGetPackages on DepOptions) overrides it. Shared=true
//     (host-owned dir, no :Z relabel). There is no clean NuGet equivalent of
//     GOPROXY=off / CARGO_NET_OFFLINE=true / npm_config_offline / PIP_NO_INDEX=1;
//     offline enforcement IS the network=none run itself: a NuGet package
//     missing from the cache hits the unreachable nuget.org source and fails
//     fast (the dotnet restore call exits non-zero with a clear "package not
//     found" diagnostic).
//
//   - FETCH: run ONE online `dotnet restore` (plus --locked-mode when
//     packages.lock.json exists) in opts.FetchSandbox into a bugbot-owned cache
//     dir (fetchNuGetCacheDir under userCacheDir/.../nugetcache/<hash>), keyed
//     on sha256(packages.lock.json) falling back to the sha256 of the
//     concatenated sorted contents of the root *.csproj files (so the key
//     reflects the dependency set when no lockfile is present). Mount the
//     populated cache dir read-only at /nugetcache with NUGET_PACKAGES=
//     /nugetcache. Shared=false (bugbot-owned dir, gets :Z isolation). Same
//     offline-enforcement caveat as HOST: no GOPROXY=off analog is invented.
//
//   - Container path /nugetcache is distinct from /modcache, /depcache,
//     /cargo/registry, and /npmcache so the mount registry's ContainerPath
//     uniqueness constraint is satisfied for multi-ecosystem repos.

// nugetCacheMount is where the NuGet global packages cache is mounted inside
// the container (HOST and FETCH strategies). NUGET_PACKAGES is set to this path
// so dotnet resolves packages at the standard $NUGET_PACKAGES location.
// Distinct from /modcache, /depcache, /cargo/registry, and /npmcache per the
// mount registry's uniqueness obligation.
const nugetCacheMount = "/nugetcache"

// nugetEcosystem is the C#/NuGet dependency resolver. It detects repos with a
// root *.csproj, *.sln, or *.fsproj and applies HOST / FETCH / OFF strategy.
// v1 has no vendored convention for NuGet; see the file-level comment.
var nugetEcosystem = ecosystem{
	name:    "nuget",
	detect:  hasCSharpProject,
	resolve: resolveNuGet,
}

// resolveNuGet is the C#/NuGet resolver function. Strategy validation has
// already run in resolveWith; invalid strategies are unreachable here.
func resolveNuGet(repoDir string, opts DepOptions) (Resolution, error) {
	// No vendored detection in v1: NuGet has no standard committed-packages
	// convention. See the file-level scope-decisions comment.

	strategy := opts.Strategy
	if strategy == "" {
		strategy = DepStrategyOff
	}

	switch strategy {
	case DepStrategyOff:
		return Resolution{Strategy: DepStrategyOff}, nil

	case DepStrategyHost:
		packages, err := resolveHostNuGetPackages(opts.hostNuGetPackages)
		if err != nil {
			return Resolution{}, err
		}
		// Shared=true: the host NuGet packages cache is NOT owned by bugbot.
		// SELinux :Z would relabel the entire cache to a container-private MCS
		// label, which may break the host dotnet toolchain and any other
		// container sharing the same directory. :ro without a label suffix is
		// correct here.
		return nugetResolution(packages, DepStrategyHost, true, nil), nil

	case DepStrategyFetch:
		if opts.FetchSandbox == nil {
			return Resolution{}, fmt.Errorf("sandbox: C#/NuGet dependency strategy %q requires a fetch sandbox", strategy)
		}
		cache, err := fetchNuGetCacheDir(repoDir, opts.userCacheDir)
		if err != nil {
			return Resolution{}, err
		}
		prefetch := newNuGetPrefetch(repoDir, cache, opts)
		// Shared=false: the NuGet fetch cache is a bugbot-owned directory under
		// the user cache dir. :Z SELinux isolation is appropriate (same rationale
		// as the Go, Python, Rust, and JS FETCH caches).
		return nugetResolution(cache, DepStrategyFetch, false, prefetch), nil

	default:
		// Unreachable given ValidDepStrategy above, but keep it explicit.
		return Resolution{}, fmt.Errorf("sandbox: unhandled C#/NuGet dependency strategy %q", strategy)
	}
}

// nugetResolution builds the Resolution shared by HOST and FETCH for C#/NuGet:
// a single read-only mount at /nugetcache plus NUGET_PACKAGES=/nugetcache so
// dotnet resolves packages at the standard $NUGET_PACKAGES location.
//
// shared controls ROMount.Shared: true for HOST (user's real NuGet packages
// cache — must not be SELinux-relabeled), false for FETCH (bugbot-owned cache
// dir — :Z isolation correct). See ROMount.Shared for the full rationale.
//
// Offline enforcement: NuGet has no clean GOPROXY=off / CARGO_NET_OFFLINE=
// analog. The --network=none run itself enforces offline operation: any NuGet
// call that would resolve a missing package hits the unreachable nuget.org
// source and fails fast with a clear diagnostic. We deliberately do NOT invent
// a fake env flag here — the OS-level network=none is the enforcement point.
func nugetResolution(hostPackages string, strategy DepStrategy, shared bool, prefetch func(context.Context) error) Resolution {
	return Resolution{
		ROMounts: []ROMount{{
			HostPath:      hostPackages,
			ContainerPath: nugetCacheMount,
			Shared:        shared,
		}},
		Env: []string{
			// NUGET_PACKAGES=/nugetcache so dotnet resolves the global packages
			// folder at the standard $NUGET_PACKAGES location (the only mount we
			// provide).
			"NUGET_PACKAGES=" + nugetCacheMount,
		},
		Prefetch: prefetch,
		Strategy: strategy,
	}
}

// hasCSharpProject reports whether repoDir contains any root-level C#/F#
// project file (*.csproj, *.sln, or *.fsproj). Root-level only, matching the
// scope of every other ecosystem's detector. Only stdlib (os.ReadDir) — the
// sandbox package must remain stdlib-only.
func hasCSharpProject(repoDir string) bool {
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".csproj") ||
			strings.HasSuffix(name, ".sln") ||
			strings.HasSuffix(name, ".fsproj") {
			return true
		}
	}
	return false
}

// resolveHostNuGetPackages resolves the host NuGet global packages directory.
// It prefers the override, then $NUGET_PACKAGES, then ~/.nuget/packages.
// Returns an error when no path can be determined (HOME unset AND env unset) or
// when the resolved path does not exist on the host (catches a misconfigured
// or unpopulated cache early, before podman emits an opaque bind-mount error).
func resolveHostNuGetPackages(override string) (string, error) {
	if override != "" {
		return checkNuGetPackagesExists(override)
	}

	// $NUGET_PACKAGES takes precedence over the ~/.nuget/packages default.
	if env := os.Getenv("NUGET_PACKAGES"); env != "" {
		return checkNuGetPackagesExists(env)
	}

	// Standard default: ~/.nuget/packages.
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("sandbox: cannot resolve host NuGet packages (NUGET_PACKAGES unset and HOME unset); use dep_strategy: off|fetch")
	}
	return checkNuGetPackagesExists(filepath.Join(home, ".nuget", "packages"))
}

// checkNuGetPackagesExists verifies that dir exists on the host and returns it.
// When the directory is missing it returns a clear, actionable error instead of
// letting podman fail with an opaque bind-mount message at run time.
func checkNuGetPackagesExists(dir string) (string, error) {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("sandbox: host NuGet packages %q does not exist; run `dotnet restore` on the host first, or use dep_strategy: off|fetch", dir)
		}
		return "", fmt.Errorf("sandbox: stat host NuGet packages %q: %w", dir, err)
	}
	return dir, nil
}

// fetchNuGetCacheDir returns the bugbot-managed NuGet global packages cache
// directory for repoDir (e.g. ~/.cache/bugbot/nugetcache/<hash>). Delegates to
// fetchEcosystemCacheDir. override, when non-empty, overrides the base dir
// (test seam).
func fetchNuGetCacheDir(repoDir, override string) (string, error) {
	return fetchEcosystemCacheDir("nugetcache", repoDir, override)
}

// nugetLockHash returns a hex hash of packages.lock.json (falling back to the
// sha256 of the concatenated sorted contents of the root *.csproj/*.fsproj
// files when packages.lock.json is absent, e.g. a project without a committed
// lockfile) so the NuGet fetch cache can be keyed on the dependency set.
// Analogous to cargoLockHash for Rust.
func nugetLockHash(repoDir string) (string, error) {
	if data, err := os.ReadFile(filepath.Join(repoDir, "packages.lock.json")); err == nil {
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:]), nil
	}
	// Fallback: hash the concatenated sorted contents of every root-level
	// *.csproj/*.fsproj file so the key reflects the declared dependency set.
	// Both are MSBuild PackageReference sources. *.sln is NOT parsed — it only
	// references projects, not packages — so a root-.sln-only repo (projects in
	// subdirs) yields no key and re-fetches each cycle (safe, never stale);
	// recursive subdir walking is a documented v1 limitation.
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return "", fmt.Errorf("sandbox: cannot read %q for nuget lock hash: %w", repoDir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".csproj") || strings.HasSuffix(name, ".fsproj") {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("sandbox: no packages.lock.json or *.csproj/*.fsproj in %q", repoDir)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(repoDir, n))
		if err != nil {
			return "", fmt.Errorf("sandbox: read %s for nuget lock hash: %w", n, err)
		}
		h.Write([]byte(n))
		h.Write([]byte{'\n'})
		h.Write(data)
		if !strings.HasSuffix(string(data), "\n") {
			h.Write([]byte{'\n'})
		}
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:]), nil
}

// newNuGetPrefetch builds the one-time online dotnet-restore hook for the
// C#/NuGet FETCH strategy. It runs `dotnet restore` in the FetchSandbox with
// network enabled and NUGET_PACKAGES pointed at hostCache (mounted at
// /nugetcache), keyed on the sha256 of packages.lock.json (or the *.csproj
// fallback hash) so an unchanged dependency set is not re-downloaded. Guarded
// by a sync.Once so it runs at most once per Resolution.
func newNuGetPrefetch(repoDir, hostCache string, opts DepOptions) func(context.Context) error {
	return newPrefetchOnce(func(ctx context.Context) error {
		return runNuGetPrefetch(ctx, repoDir, hostCache, opts)
	})
}

// runNuGetPrefetch performs the actual online dotnet restore. It is a no-op
// when the cache is already warm for the repo's current packages.lock.json
// (or *.csproj fallback hash).
func runNuGetPrefetch(ctx context.Context, repoDir, hostCache string, opts DepOptions) error {
	lockHash, hashErr := nugetLockHash(repoDir)
	sentinel := filepath.Join(hostCache, prefetchSentinel) // reuse same constant

	if hashErr == nil {
		if prev, err := os.ReadFile(sentinel); err == nil && strings.TrimSpace(string(prev)) == lockHash {
			// Cache already populated for this exact packages.lock.json (or
			// *.csproj set).
			return nil
		}
	}

	network := opts.FetchNetwork
	if network == "" {
		// "bridge" is the standard NAT network both podman and docker accept;
		// this is the ONLY NuGet run ever allowed network access.
		network = "bridge"
	}

	// Build the dotnet restore command. Add --locked-mode when
	// packages.lock.json exists so the prefetch is deterministic and dotnet
	// does not update the lockfile during the online step. Mirrors Cargo's
	// --locked behavior.
	cmd := []string{"dotnet", "restore"}
	if _, err := os.Stat(filepath.Join(repoDir, "packages.lock.json")); err == nil {
		cmd = append(cmd, "--locked-mode")
	}

	// The prefetch container is fully hardened (read-only root, cap-drop,
	// no-new-privileges, limits — all from buildRunArgs); only the network
	// differs and the cache is mounted WRITABLE so `dotnet restore` can
	// populate NUGET_PACKAGES inside the container. The later network-none run
	// mounts the same dir read-only.
	spec := Spec{
		RepoDir: repoDir,
		Image:   opts.FetchImage,
		Network: network,
		Cmd:     cmd,
		// Mount the cache dir WRITABLE as NUGET_PACKAGES so dotnet restore
		// populates hostCache on the host. The RO mount in the final Resolution
		// points at hostCache (which IS hostCache for the fetch strategy —
		// see fetchNuGetCacheDir and nugetResolution).
		RWMounts: []ROMount{{HostPath: hostCache, ContainerPath: nugetCacheMount}},
		Env: []string{
			"NUGET_PACKAGES=" + nugetCacheMount,
		},
	}
	res, err := opts.FetchSandbox.Exec(ctx, spec)
	if err != nil {
		return fmt.Errorf("sandbox: NuGet prefetch `dotnet restore` failed to launch: %w", err)
	}
	if res.TimedOut {
		return fmt.Errorf("sandbox: NuGet prefetch `dotnet restore` timed out")
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sandbox: NuGet prefetch `dotnet restore` exited %d: %s", res.ExitCode, lastLines(res.Stderr, 20))
	}

	if hashErr == nil {
		// Record the warm-cache sentinel. A write failure only costs a redundant
		// future fetch, so it is non-fatal.
		_ = os.WriteFile(sentinel, []byte(lockHash), 0o644)
	}
	return nil
}

// ---- JVM ecosystems (Maven and Gradle) --------------------------------------
//
// Two distinct ecosystem entries are used rather than one combined "jvm" entry
// because Maven (~/.m2/repository) and Gradle (~/.gradle/caches) have entirely
// different HOST cache layouts and container paths. A single entry would either
// have to mount two directories (complicating strategy orthogonality) or silently
// ignore the non-dominant tool. Two entries compose cleanly via resolveWith's
// append semantics, giving a polyglot Maven+Gradle repo correct mounts from both.
//
// ---- Maven ecosystem -------------------------------------------------------
//
// v1 strategy: Maven local repository HOST + FETCH, driven by root pom.xml.
//
// Scope decisions (final; do not change silently — flag any deviation):
//
//   - DETECT: root pom.xml exists. Multi-module Maven projects always have a
//     root pom.xml (the aggregate POM); single-module projects also have one.
//     Root-level only — matching every other ecosystem's detector scope.
//
//   - VENDORED: NOT supported in v1. Maven has no widely-adopted, standard
//     committed-repository convention equivalent to Go's vendor/modules.txt or
//     Cargo's vendor/ + .cargo/config stanza. Projects that commit a
//     repository/ tree are rare and non-standard. NOT-supported-in-v1.
//
//   - OFF: empty Resolution{Strategy: DepStrategyOff}.
//
//   - HOST: mount the host Maven local repository READ-ONLY at /m2cache.
//     MAVEN_OPTS=-Dmaven.repo.local=/m2cache tells Maven to use that mount as
//     the local repository (the standard -Dmaven.repo.local convention).
//     The default path is $HOME/.m2/repository; a test seam
//     (hostMavenRepository on DepOptions) overrides it. Shared=true
//     (host-owned dir, no :Z relabel). Offline enforcement: Maven has no exact
//     GOPROXY=off analog; --offline (-o) would need a SetupCmd injection and
//     is fragile for multi-module builds. We follow the NuGet precedent —
//     --network=none IS the enforcement: a missing artifact hits the
//     unreachable repository and fails fast with a clear diagnostic. We
//     deliberately do NOT set -o / --offline to avoid breaking builds that
//     call mvn in their test scripts with extra goals.
//
//   - FETCH: run ONE online `mvn -B dependency:go-offline` in opts.FetchSandbox
//     into a bugbot-owned cache dir (fetchMavenCacheDir under
//     userCacheDir/.../m2cache/<hash>), keyed on sha256(pom.xml) (root pom
//     only — recursive module collection requires a running JVM and is
//     disproportionate for a v1 key; root pom captures the declared dep set
//     for single-module projects and the aggregate for multi-module). Mount the
//     populated repository dir read-only at /m2cache with
//     MAVEN_OPTS=-Dmaven.repo.local=/m2cache. Shared=false (bugbot-owned dir,
//     gets :Z isolation). Same offline enforcement as HOST: network=none is
//     the boundary.
//
//   - SECURITY (prefetch): `mvn -B dependency:go-offline` instantiates POM
//     plugins and any .mvn/extensions.xml build extensions, executing repo-
//     controlled Java code ONLINE. There is no Maven analog to npm's
//     --ignore-scripts. Accepted under the bugbot-gu0o posture; see
//     runMavenPrefetch for the full rationale.
//
//   - Container path /m2cache is distinct from /modcache, /depcache,
//     /cargo/registry, /npmcache, /nugetcache, and /gradlecache so the mount
//     registry's ContainerPath uniqueness constraint is satisfied.

// m2CacheMount is where the Maven local repository is mounted inside the
// container (HOST and FETCH strategies). MAVEN_OPTS=-Dmaven.repo.local= is set
// to this path so Maven resolves artifacts at the standard local repository
// location without additional configuration.
// Distinct from all other ecosystem mount paths per the mount registry's
// uniqueness obligation.
const m2CacheMount = "/m2cache"

// mavenEcosystem is the Maven dependency resolver. It detects repos with a root
// pom.xml and applies HOST / FETCH / OFF strategy. v1 has no vendored convention
// for Maven; see the file-level comment.
var mavenEcosystem = ecosystem{
	name:    "maven",
	detect:  hasPomXML,
	resolve: resolveMaven,
}

// resolveMaven is the Maven resolver function. Strategy validation has already
// run in resolveWith; invalid strategies are unreachable here.
func resolveMaven(repoDir string, opts DepOptions) (Resolution, error) {
	// No vendored detection in v1: Maven has no standard committed-repository
	// convention. See the file-level scope-decisions comment.

	strategy := opts.Strategy
	if strategy == "" {
		strategy = DepStrategyOff
	}

	switch strategy {
	case DepStrategyOff:
		return Resolution{Strategy: DepStrategyOff}, nil

	case DepStrategyHost:
		repo, err := resolveHostMavenRepository(opts.hostMavenRepository)
		if err != nil {
			return Resolution{}, err
		}
		// Shared=true: the host Maven local repository is NOT owned by bugbot.
		// SELinux :Z would relabel the entire repository to a container-private
		// MCS label, which may break the host toolchain. :ro without a label
		// suffix is correct here.
		return mavenResolution(repo, DepStrategyHost, true, nil), nil

	case DepStrategyFetch:
		if opts.FetchSandbox == nil {
			return Resolution{}, fmt.Errorf("sandbox: Maven dependency strategy %q requires a fetch sandbox", strategy)
		}
		cache, err := fetchMavenCacheDir(repoDir, opts.userCacheDir)
		if err != nil {
			return Resolution{}, err
		}
		prefetch := newMavenPrefetch(repoDir, cache, opts)
		// Shared=false: the Maven fetch cache is a bugbot-owned directory under
		// the user cache dir. :Z SELinux isolation is appropriate.
		return mavenResolution(cache, DepStrategyFetch, false, prefetch), nil

	default:
		return Resolution{}, fmt.Errorf("sandbox: unhandled Maven dependency strategy %q", strategy)
	}
}

// mavenResolution builds the Resolution shared by HOST and FETCH for Maven:
// a single read-only mount at /m2cache plus MAVEN_OPTS so Maven uses it as the
// local repository.
//
// shared controls ROMount.Shared: true for HOST (user's real Maven repository —
// must not be SELinux-relabeled), false for FETCH (bugbot-owned cache dir —
// :Z isolation correct). See ROMount.Shared for the full rationale.
//
// Offline enforcement: Maven has no clean GOPROXY=off analog. The --network=none
// run itself enforces offline operation: any Maven call that would resolve a
// missing artifact hits the unreachable repository server and fails fast with a
// clear diagnostic. We deliberately do NOT set -o / --offline — the OS-level
// network=none is the enforcement point (same approach as NuGet).
func mavenResolution(hostRepo string, strategy DepStrategy, shared bool, prefetch func(context.Context) error) Resolution {
	return Resolution{
		ROMounts: []ROMount{{
			HostPath:      hostRepo,
			ContainerPath: m2CacheMount,
			Shared:        shared,
		}},
		Env: []string{
			// MAVEN_OPTS=-Dmaven.repo.local=/m2cache so Maven resolves the local
			// repository at the mounted path. This is the standard Maven property
			// for overriding the local repository location.
			"MAVEN_OPTS=-Dmaven.repo.local=" + m2CacheMount,
		},
		Prefetch: prefetch,
		Strategy: strategy,
	}
}

// hasPomXML reports whether repoDir contains a root pom.xml.
// Only stdlib (os.Stat) — the sandbox package must remain stdlib-only.
func hasPomXML(repoDir string) bool {
	_, err := os.Stat(filepath.Join(repoDir, "pom.xml"))
	return err == nil
}

// resolveHostMavenRepository resolves the host Maven local repository directory.
// It prefers the override, then $HOME/.m2/repository.
// Returns an error when no path can be determined (HOME unset) or when the
// resolved path does not exist on the host (catches a misconfigured or
// unpopulated repository early, before podman emits an opaque bind-mount error).
func resolveHostMavenRepository(override string) (string, error) {
	if override != "" {
		return checkMavenRepositoryExists(override)
	}

	// Maven does not have a canonical env var for the local repository path
	// (MAVEN_OPTS/-Dmaven.repo.local is a runtime override, not a discovery
	// path). Fall through to the standard default: ~/.m2/repository.
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("sandbox: cannot resolve host Maven repository (HOME unset); use dep_strategy: off|fetch")
	}
	return checkMavenRepositoryExists(filepath.Join(home, ".m2", "repository"))
}

// checkMavenRepositoryExists verifies that dir exists on the host and returns it.
// When the directory is missing it returns a clear, actionable error instead of
// letting podman fail with an opaque bind-mount error at run time.
func checkMavenRepositoryExists(dir string) (string, error) {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("sandbox: host Maven repository %q does not exist; run `mvn dependency:go-offline` on the host first, or use dep_strategy: off|fetch", dir)
		}
		return "", fmt.Errorf("sandbox: stat host Maven repository %q: %w", dir, err)
	}
	return dir, nil
}

// fetchMavenCacheDir returns the bugbot-managed Maven local repository cache
// directory for repoDir (e.g. ~/.cache/bugbot/m2cache/<hash>). Delegates to
// fetchEcosystemCacheDir. override, when non-empty, overrides the base dir
// (test seam).
func fetchMavenCacheDir(repoDir, override string) (string, error) {
	return fetchEcosystemCacheDir("m2cache", repoDir, override)
}

// mavenPomHash returns a hex hash of the root pom.xml so the Maven fetch cache
// can be keyed on the dependency set. Root pom only (v1): multi-module
// recursive collection requires a running JVM and is disproportionate for a
// cache key; the root aggregate POM captures the declared dependency set for
// most common cases. Analogous to goSumHash for Go and cargoLockHash for Rust.
func mavenPomHash(repoDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(repoDir, "pom.xml"))
	if err != nil {
		return "", fmt.Errorf("sandbox: read pom.xml for maven lock hash: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// newMavenPrefetch builds the one-time online mvn prefetch hook for the Maven
// FETCH strategy. It runs `mvn -B dependency:go-offline` in the FetchSandbox
// with network enabled and MAVEN_OPTS pointed at hostCache (mounted at
// /m2cache), keyed on the sha256 of pom.xml so an unchanged dependency set
// is not re-downloaded. Guarded by a sync.Once so it runs at most once per
// Resolution.
func newMavenPrefetch(repoDir, hostCache string, opts DepOptions) func(context.Context) error {
	return newPrefetchOnce(func(ctx context.Context) error {
		return runMavenPrefetch(ctx, repoDir, hostCache, opts)
	})
}

// runMavenPrefetch performs the actual online mvn dependency:go-offline. It is
// a no-op when the cache is already warm for the repo's current pom.xml.
//
// SECURITY: `mvn -B dependency:go-offline` loads the Maven project model (POM),
// which instantiates configured POM plugins and any build extensions declared in
// .mvn/extensions.xml. This executes repo-controlled Java code ONLINE (network-
// enabled container). There is no Maven analog to npm's --ignore-scripts; the
// POM lifecycle is always evaluated. This is accepted under the bugbot-gu0o
// posture (same document-and-accept decision as pip's `pip download` executing
// setup.py and dotnet's `dotnet restore` running NuGet scripts). The threat is
// mitigated by the container's other hardening (cap-drop ALL, no-new-privileges,
// read-only root, pid limit) and by the absence of secret-bearing mounts during
// the prefetch run.
func runMavenPrefetch(ctx context.Context, repoDir, hostCache string, opts DepOptions) error {
	lockHash, hashErr := mavenPomHash(repoDir)
	sentinel := filepath.Join(hostCache, prefetchSentinel)

	if hashErr == nil {
		if prev, err := os.ReadFile(sentinel); err == nil && strings.TrimSpace(string(prev)) == lockHash {
			// Cache already populated for this exact pom.xml.
			return nil
		}
	}

	network := opts.FetchNetwork
	if network == "" {
		network = "bridge"
	}

	// `mvn -B dependency:go-offline` downloads all compile/runtime/test
	// dependencies declared in the POM into the local repository. -B is
	// batch mode (no interactive prompts).
	cmd := []string{"mvn", "-B", "dependency:go-offline"}

	// The prefetch container is fully hardened; only network and the writable
	// cache mount differ from the network-none run.
	spec := Spec{
		RepoDir: repoDir,
		Image:   opts.FetchImage,
		Network: network,
		Cmd:     cmd,
		// Mount the cache dir WRITABLE as /m2cache so mvn populates it.
		RWMounts: []ROMount{{HostPath: hostCache, ContainerPath: m2CacheMount}},
		Env: []string{
			"MAVEN_OPTS=-Dmaven.repo.local=" + m2CacheMount,
		},
	}
	res, err := opts.FetchSandbox.Exec(ctx, spec)
	if err != nil {
		return fmt.Errorf("sandbox: Maven prefetch `mvn dependency:go-offline` failed to launch: %w", err)
	}
	if res.TimedOut {
		return fmt.Errorf("sandbox: Maven prefetch `mvn dependency:go-offline` timed out")
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sandbox: Maven prefetch `mvn dependency:go-offline` exited %d: %s", res.ExitCode, lastLines(res.Stderr, 20))
	}

	if hashErr == nil {
		_ = os.WriteFile(sentinel, []byte(lockHash), 0o644)
	}
	return nil
}

// ---- Gradle ecosystem -------------------------------------------------------
//
// v1 strategy: Gradle caches FETCH only, driven by root build.gradle,
// build.gradle.kts, settings.gradle, or settings.gradle.kts.
//
// Scope decisions (final; do not change silently — flag any deviation):
//
//   - DETECT: any of build.gradle, build.gradle.kts, settings.gradle, or
//     settings.gradle.kts exists at the root. Root-level only — matching
//     every other ecosystem's detector scope.
//
//   - VENDORED: NOT supported in v1. Gradle has no widely-adopted,
//     standard committed-cache convention. NOT-supported-in-v1.
//
//   - OFF: empty Resolution{Strategy: DepStrategyOff}.
//
//   - HOST → OFF. CAUTION: Gradle's ~/.gradle/caches directory is
//     lock-heavy. Gradle maintains file-locks inside the caches directory
//     even for read-only dependency resolution, because it uses journal
//     files and lock files to coordinate concurrent builds. Mounting it
//     read-only inside a container causes Gradle to fail with "Could not
//     acquire lock on <lock-file>" errors on the very first `gradle test`
//     invocation. This is fundamentally different from Go's module cache
//     (lock-free after population) or Maven's repository (also lock-free).
//     We cannot verify the workaround empirically without a running JDK+Gradle
//     image in the test environment, so we follow the same decision as
//     Python/npm for their broken-HOST cases: HOST is treated as OFF rather
//     than silently broken. A future bead can revisit with a live JDK image
//     if Gradle adds read-only-cache support or if a copy-to-writable
//     HOST variant becomes worth the complexity.
//
//   - FETCH → Gradle dependency cache prefetch + writable-copy run:
//     1. Prefetch (network bridge, ONE online step): run
//          gradle dependencies --no-daemon -q
//        in a network-enabled container with the cache dir mounted WRITABLE
//        at /gradlecache. GRADLE_USER_HOME=/gradlecache directs Gradle to
//        populate caches/ and wrapper/ there.
//     2. Resolution: mount the same dir READ-ONLY at /gradlecache
//        (Shared=false — bugbot-owned, gets :Z), and add two SetupCmds that
//        copy the RO cache to a disk-backed workspace location before build:
//          mkdir -p /workspace/.bugbot-gradle-home
//          cp -a /gradlecache/. /workspace/.bugbot-gradle-home
//        then set GRADLE_USER_HOME=/workspace/.bugbot-gradle-home (via env).
//        WHY WORKSPACE, NOT /tmp: Gradle caches for a real project routinely
//        exceed hundreds of MB; the /tmp tmpfs is capped at 512 MB
//        (buildRunArgs: --tmpfs /tmp:size=512m) and a cache copy that exceeds
//        it causes exit 125 → environment_error → silent recall loss (same
//        failure mode as Go builds before goCacheDir was introduced). The
//        disk-backed /workspace has no such cap. The copy target is
//        dot-prefixed (/workspace/.bugbot-gradle-home) so `gradle test ./...`
//        and similar recursive globs skip it (Gradle, like Go, ignores
//        directories beginning with "." or "_").
//        --no-daemon in both prefetch and build prevents a Gradle daemon from
//        persisting between runs (correct for ephemeral containers).
//
//   - SECURITY (prefetch): Gradle executes settings.gradle / build.gradle
//     (Groovy/Kotlin DSL) at configuration time. This is inherent to Gradle's
//     build model: there is no --ignore-scripts analog — configuration code
//     always runs. During the prefetch the container has network access, so
//     malicious configuration code could exfiltrate data or contact external
//     services. This is within the bugbot-gu0o posture (accepted per the
//     document-and-accept decision for all prefetch ecosystems that execute
//     repo-controlled code online). The threat is mitigated by the container's
//     other hardening (cap-drop ALL, no-new-privileges, read-only root, pid
//     limit) and by the absence of secret-bearing mounts during the prefetch.
//
//   - Lock-hash key: sha256(gradle.lockfile) if present at root, else
//     sha256 of sorted concatenated contents of build.gradle, build.gradle.kts,
//     settings.gradle, settings.gradle.kts (whichever exist). Fallback
//     analogous to nugetLockHash's *.csproj fallback.
//
//   - Container path /gradlecache is distinct from /modcache, /depcache,
//     /cargo/registry, /npmcache, /nugetcache, and /m2cache so the mount
//     registry's ContainerPath uniqueness constraint is satisfied.

// gradleCacheMount is where the populated Gradle user home is mounted
// READ-ONLY inside the container (FETCH strategy staging point). At run time a
// SetupCmd copies it to gradleHomeDir (disk-backed /workspace) so Gradle can
// acquire file-locks freely. Distinct from all other ecosystem mount paths per
// the mount registry's uniqueness obligation.
const gradleCacheMount = "/gradlecache"

// gradleHomeDir is the disk-backed writable copy of the Gradle user home
// created inside the container before each run. It lives under /workspace (the
// per-run, disk-backed copy — no size cap) rather than /tmp (512 MB tmpfs) so
// large Gradle caches do not overflow the tmpfs and cause exit 125 /
// environment_error. The dot-prefix causes Gradle recursive globs to skip it
// (same convention as goCacheDir / goTmpDir).
const gradleHomeDir = workspaceMount + "/.bugbot-gradle-home"

// gradleEcosystem is the Gradle dependency resolver. It detects repos with a
// root build.gradle / build.gradle.kts / settings.gradle / settings.gradle.kts
// and applies FETCH / OFF strategy. HOST maps to OFF (Gradle cache is lock-heavy
// under a read-only mount; see the file-level scope-decisions comment).
// v1 has no vendored convention for Gradle.
var gradleEcosystem = ecosystem{
	name:    "gradle",
	detect:  hasGradleBuildFile,
	resolve: resolveGradle,
}

// resolveGradle is the Gradle resolver function. Strategy validation has already
// run in resolveWith; invalid strategies are unreachable here.
func resolveGradle(repoDir string, opts DepOptions) (Resolution, error) {
	// No vendored detection in v1; see file-level scope-decisions comment.

	strategy := opts.Strategy
	if strategy == "" {
		strategy = DepStrategyOff
	}

	switch strategy {
	case DepStrategyOff:
		return Resolution{Strategy: DepStrategyOff}, nil

	case DepStrategyHost:
		// HOST → OFF: Gradle's cache is lock-heavy and fights a read-only mount.
		// See the file-level scope-decisions comment for the rationale.
		return Resolution{Strategy: DepStrategyOff}, nil

	case DepStrategyFetch:
		if opts.FetchSandbox == nil {
			return Resolution{}, fmt.Errorf("sandbox: Gradle dependency strategy %q requires a fetch sandbox", strategy)
		}
		cache, err := fetchGradleCacheDir(repoDir, opts.userCacheDir)
		if err != nil {
			return Resolution{}, err
		}
		prefetch := newGradlePrefetch(repoDir, cache, opts)
		// Shared=false: the Gradle fetch cache is bugbot-owned. :Z SELinux
		// isolation is appropriate.
		return gradleResolution(cache, prefetch), nil

	default:
		return Resolution{}, fmt.Errorf("sandbox: unhandled Gradle dependency strategy %q", strategy)
	}
}

// gradleResolution builds the Resolution for Gradle FETCH: a read-only mount of
// the populated cache at /gradlecache plus two SetupCmds that create a writable
// copy under the disk-backed /workspace before the build. GRADLE_USER_HOME is
// set to that copy so Gradle can acquire file-locks freely.
//
// WHY /workspace, NOT /tmp: Gradle caches for real projects routinely exceed
// hundreds of MB. The /tmp tmpfs is capped at 512 MB (buildRunArgs:
// --tmpfs /tmp:size=512m); a copy that overflows it causes exit 125 →
// environment_error → silent recall loss. /workspace is a per-run, disk-backed
// copy with no size cap. The copy target is dot-prefixed (gradleHomeDir =
// /workspace/.bugbot-gradle-home) so recursive build globs skip it.
//
// The two-step mkdir+cp is necessary rather than a single cp -a because
// /workspace/.bugbot-gradle-home must be created explicitly before the copy
// (cp -a /gradlecache /workspace/.bugbot-gradle-home would create a nested
// /workspace/.bugbot-gradle-home/gradlecache/ subdirectory, mirroring the
// cp -a /src /dst semantics when dst does not exist vs when it does).
func gradleResolution(hostCache string, prefetch func(context.Context) error) Resolution {
	return Resolution{
		ROMounts: []ROMount{{
			HostPath:      hostCache,
			ContainerPath: gradleCacheMount,
			Shared:        false, // bugbot-owned dir, :Z isolation correct
		}},
		Env: []string{
			// GRADLE_USER_HOME points Gradle at the disk-backed writable copy
			// produced by the SetupCmds below. NOT /tmp — see function comment.
			"GRADLE_USER_HOME=" + gradleHomeDir,
		},
		SetupCmds: [][]string{
			// Create the destination directory before the copy. mkdir -p is safe
			// to repeat if the dir already exists (e.g. operator SetupCmds ran
			// first and created it, which cannot happen in practice, but is
			// idempotent regardless).
			{"mkdir", "-p", gradleHomeDir},
			// Copy the read-only Gradle user home into the writable workspace.
			// `cp -a /gradlecache/. dst` copies CONTENTS (not the dir itself)
			// into the pre-created destination. Gradle acquires file-locks inside
			// GRADLE_USER_HOME; the RO /gradlecache mount would cause "Could not
			// acquire lock" failures without this copy.
			{"sh", "-c", "cp -a " + gradleCacheMount + "/. " + gradleHomeDir},
		},
		Prefetch: prefetch,
		Strategy: DepStrategyFetch,
	}
}

// hasGradleBuildFile reports whether repoDir contains any of the standard
// Gradle root build files: build.gradle, build.gradle.kts, settings.gradle,
// or settings.gradle.kts. Root-level only — matching every other ecosystem's
// detector scope. Only stdlib (os.Stat) — the sandbox package must remain
// stdlib-only.
func hasGradleBuildFile(repoDir string) bool {
	for _, name := range []string{
		"build.gradle",
		"build.gradle.kts",
		"settings.gradle",
		"settings.gradle.kts",
	} {
		if _, err := os.Stat(filepath.Join(repoDir, name)); err == nil {
			return true
		}
	}
	return false
}

// fetchGradleCacheDir returns the bugbot-managed Gradle user-home cache
// directory for repoDir (e.g. ~/.cache/bugbot/gradlecache/<hash>). Delegates
// to fetchEcosystemCacheDir. override, when non-empty, overrides the base dir
// (test seam).
func fetchGradleCacheDir(repoDir, override string) (string, error) {
	return fetchEcosystemCacheDir("gradlecache", repoDir, override)
}

// gradleLockHash returns a hex hash of gradle.lockfile (falling back to the
// sha256 of the sorted concatenated contents of the root Gradle build files
// when gradle.lockfile is absent) so the Gradle fetch cache can be keyed on
// the dependency set. Analogous to nugetLockHash's *.csproj fallback pattern.
func gradleLockHash(repoDir string) (string, error) {
	if data, err := os.ReadFile(filepath.Join(repoDir, "gradle.lockfile")); err == nil {
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:]), nil
	}
	// Fallback: hash the sorted concatenated contents of all root Gradle build
	// files that exist.
	buildFiles := []string{
		"build.gradle",
		"build.gradle.kts",
		"settings.gradle",
		"settings.gradle.kts",
	}
	var present []string
	for _, f := range buildFiles {
		if _, err := os.Stat(filepath.Join(repoDir, f)); err == nil {
			present = append(present, f)
		}
	}
	if len(present) == 0 {
		return "", fmt.Errorf("sandbox: no gradle.lockfile or Gradle build files in %q", repoDir)
	}
	// Already in canonical order (same as buildFiles); sort anyway for safety.
	sort.Strings(present)
	h := sha256.New()
	for _, n := range present {
		data, err := os.ReadFile(filepath.Join(repoDir, n))
		if err != nil {
			return "", fmt.Errorf("sandbox: read %s for gradle lock hash: %w", n, err)
		}
		h.Write([]byte(n))
		h.Write([]byte{'\n'})
		h.Write(data)
		if !strings.HasSuffix(string(data), "\n") {
			h.Write([]byte{'\n'})
		}
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:]), nil
}

// newGradlePrefetch builds the one-time online Gradle prefetch hook for the
// FETCH strategy. It runs `gradle dependencies --no-daemon` in the FetchSandbox
// with network enabled and GRADLE_USER_HOME pointed at hostCache (mounted at
// /gradlecache), keyed on the sha256 of gradle.lockfile (or the Gradle build
// files fallback) so an unchanged dependency set is not re-downloaded. Guarded
// by a sync.Once so it runs at most once per Resolution.
func newGradlePrefetch(repoDir, hostCache string, opts DepOptions) func(context.Context) error {
	return newPrefetchOnce(func(ctx context.Context) error {
		return runGradlePrefetch(ctx, repoDir, hostCache, opts)
	})
}

// runGradlePrefetch performs the actual online Gradle dependency resolution.
// It is a no-op when the cache is already warm for the repo's current
// gradle.lockfile (or Gradle build files fallback hash).
//
// SECURITY: `gradle dependencies` evaluates settings.gradle and build.gradle
// (Groovy or Kotlin DSL) at configuration time. This executes repo-controlled
// code ONLINE (network-enabled container). There is no Gradle analog to npm's
// --ignore-scripts — configuration code always runs; it cannot be skipped
// without fundamentally changing how Gradle loads the project. This is accepted
// under the bugbot-gu0o posture (same document-and-accept decision as pip,
// dotnet, and Maven prefetches). The threat is mitigated by the container's
// other hardening (cap-drop ALL, no-new-privileges, read-only root, pid limit)
// and by the absence of secret-bearing mounts during the prefetch run.
func runGradlePrefetch(ctx context.Context, repoDir, hostCache string, opts DepOptions) error {
	lockHash, hashErr := gradleLockHash(repoDir)
	sentinel := filepath.Join(hostCache, prefetchSentinel)

	if hashErr == nil {
		if prev, err := os.ReadFile(sentinel); err == nil && strings.TrimSpace(string(prev)) == lockHash {
			// Cache already populated for this exact Gradle dependency set.
			return nil
		}
	}

	network := opts.FetchNetwork
	if network == "" {
		network = "bridge"
	}

	// `gradle dependencies --no-daemon` resolves all dependency configurations
	// and populates the Gradle user home (GRADLE_USER_HOME). --no-daemon
	// prevents a Gradle daemon from persisting between runs (correct for
	// ephemeral containers). -q suppresses excessive output.
	cmd := []string{"gradle", "dependencies", "--no-daemon", "-q"}

	spec := Spec{
		RepoDir: repoDir,
		Image:   opts.FetchImage,
		Network: network,
		Cmd:     cmd,
		// Mount the cache dir WRITABLE as GRADLE_USER_HOME so Gradle populates
		// caches/ and wrapper/ there.
		RWMounts: []ROMount{{HostPath: hostCache, ContainerPath: gradleCacheMount}},
		Env: []string{
			"GRADLE_USER_HOME=" + gradleCacheMount,
		},
	}
	res, err := opts.FetchSandbox.Exec(ctx, spec)
	if err != nil {
		return fmt.Errorf("sandbox: Gradle prefetch `gradle dependencies` failed to launch: %w", err)
	}
	if res.TimedOut {
		return fmt.Errorf("sandbox: Gradle prefetch `gradle dependencies` timed out")
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sandbox: Gradle prefetch `gradle dependencies` exited %d: %s", res.ExitCode, lastLines(res.Stderr, 20))
	}

	if hashErr == nil {
		_ = os.WriteFile(sentinel, []byte(lockHash), 0o644)
	}
	return nil
}

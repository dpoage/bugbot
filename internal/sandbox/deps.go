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
	case "", DepStrategyOff, DepStrategyHost, DepStrategyFetch:
		return true
	default:
		return false
	}
}

// modcacheMount is where a dependency module cache is mounted read-only inside
// the container (HOST and FETCH strategies).
const modcacheMount = "/modcache"

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

	// userCacheDir, when set, overrides the base directory for bugbot-managed
	// fetch caches (test seam). Empty uses os.UserCacheDir.
	userCacheDir string
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

	// Prefetch, when non-nil, must be called ONCE before the first network-none
	// run for this repo (e.g. from the reproducer's PromoteAll setup). It runs
	// the online module download and is a no-op on repeat calls (it is guarded
	// by a sync.Once internally). Strategies other than FETCH leave it nil.
	Prefetch func(ctx context.Context) error

	// Strategy is the strategy actually applied (after vendored detection may
	// have overridden the requested one), for logging/diagnostics.
	Strategy DepStrategy
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
// mounts/env/prefetch a run should use. It never errors on a non-Go repo or an
// unknown-but-empty strategy: such repos resolve to an empty Resolution (OFF).
//
// Order of precedence:
//  1. Non-Go repo (no go.mod) → empty Resolution regardless of strategy.
//  2. Vendored repo → VENDORED, regardless of the requested strategy (free,
//     safe, and authoritative).
//  3. Otherwise the requested strategy (OFF/HOST/FETCH).
func ResolveDeps(repoDir string, opts DepOptions) (Resolution, error) {
	if !ValidDepStrategy(opts.Strategy) {
		return Resolution{}, fmt.Errorf("sandbox: invalid dependency strategy %q (want off, host, or fetch)", opts.Strategy)
	}

	// Only Go repos are handled today; other ecosystems get the empty (OFF)
	// resolution until they grow their own branch here.
	if !hasGoModule(repoDir) {
		return Resolution{Strategy: DepStrategyOff}, nil
	}

	// Vendored detection wins in every mode: it is free, safe, and makes `go`
	// ignore the network entirely.
	if isVendored(repoDir) {
		return Resolution{
			Env:      []string{"GOFLAGS=-mod=vendor"},
			Strategy: "vendored",
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
// under the user cache dir (e.g. ~/.cache/bugbot/modcache/<hash>). Using the
// user cache dir rather than a dir inside the scanned repo keeps the repo tree
// clean (copyTree copies the whole repo, so an in-repo cache would bloat every
// workspace copy) and lets the cache survive across runs. The directory is
// created if missing.
func fetchCacheDir(repoDir, override string) (string, error) {
	base := override
	if base == "" {
		uc, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("sandbox: resolve user cache dir for fetch cache: %w", err)
		}
		base = filepath.Join(uc, "bugbot", "modcache")
	}
	dir := filepath.Join(base, repoHash(repoDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("sandbox: create fetch cache dir: %w", err)
	}
	return dir, nil
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

// newPrefetch builds the one-time online prefetch hook for the FETCH strategy.
// It runs `go mod download all` in the FetchSandbox with network enabled and
// GOMODCACHE pointed at hostCache, and is keyed on the repo's go.sum hash so an
// unchanged dependency set is not re-downloaded. The returned func is guarded by
// a sync.Once so it runs at most once per Resolution even if called repeatedly.
func newPrefetch(repoDir, hostCache string, opts DepOptions) func(context.Context) error {
	var once sync.Once
	var onceErr error
	return func(ctx context.Context) error {
		once.Do(func() {
			onceErr = runPrefetch(ctx, repoDir, hostCache, opts)
		})
		return onceErr
	}
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

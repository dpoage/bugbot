package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// DepSourceRoots is a per-ecosystem set of read-only source roots. The verifier
// (refuter + arbiter) can read files under these roots in addition to the repo
// root, so a panel can confirm the behavior of a cited stdlib symbol or a
// third-party module by reading its actual source instead of asserting it from
// memory. The first concrete ecosystem is Go (GOROOT/src + the module cache);
// later ecosystems (C/C++ system include dirs, python site-packages) are added
// the same way without changing the tool wiring.
//
// Each root is independent and traversal-protected (no `..`, no symlink escape),
// matching the in-repo FSRoot contract. Roots that don't exist on the current
// host (e.g. a C++ developer without a Go toolchain) are simply absent from
// the active set; the resolve method is a no-op miss for them.
//
// DepSourceRoots is a snapshot value — once constructed, its members are
// stable, so a verifier tool that captures a DepSourceRoots at funnel.New
// reads a consistent set of roots for the run.
type DepSourceRoots struct {
	// roots is the cleaned, symlink-resolved set of read-only absolute
	// directories the verifier may read under. Order is irrelevant; lookup
	// is O(roots) but the set is small (<= a handful per ecosystem).
	roots []string
}

// errDepPathEscape is returned when a requested dep-source path resolves
// outside every configured root. Distinct from the in-repo errPathEscape so
// callers (tests) can distinguish a misconfigured dep-source set from an
// in-repo escape attempt.
var errDepPathEscape = errors.New("path escapes all dep-source roots")

// NewDepSourceRoots discovers the read-only dep-source roots available on the
// current host. It probes per-ecosystem:
//
//   - Go: GOROOT/src (env GOROOT, then `go env GOROOT`) and the module cache
//     (env GOMODCACHE, then `go env GOMODCACHE`, then $GOPATH/pkg/mod).
//   - C/C++/python: reserved for a later bead; the discovery is data-driven
//     so adding an ecosystem is a one-row change.
//
// Roots that do not exist as directories on disk are silently dropped — a
// developer on a host without the Go toolchain gets an empty (but valid)
// DepSourceRoots and the verifier behaves exactly as before. Never returns
// an error: a missing ecosystem is a quiet "no", not a failure.
//
// Discovery is cheap (a few env reads + at most two exec calls) so the funnel
// may call this at New and capture the snapshot on Funnel.
func NewDepSourceRoots() *DepSourceRoots {
	depRootsOnce.Do(func() { depRootsCached = discoverDepSourceRoots() })
	return depRootsCached
}

var (
	depRootsOnce   sync.Once
	depRootsCached *DepSourceRoots
)

// discoverDepSourceRoots performs the actual host probe. NewDepSourceRoots
// caches its result for the process: the roots are host-stable, so the `go env`
// forks must not repeat on every Funnel.New (the daemon builds one per cycle).
func discoverDepSourceRoots() *DepSourceRoots {
	var roots []string
	for _, p := range discoverGoRoots() {
		if p == "" {
			continue
		}
		if resolved, ok := resolveExistingDir(p); ok {
			roots = append(roots, resolved)
		}
	}
	// de-dup (in case GOROOT and GOMODCACHE coincide on this host, etc.)
	seen := make(map[string]struct{}, len(roots))
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	return &DepSourceRoots{roots: out}
}

// discoverGoRoots returns the candidate Go source roots in priority order:
// GOROOT/src first (the standard library; the most commonly cited external
// source), then the module cache (third-party deps). The env probe runs
// before the `go env` probe so a CI env that pre-sets GOROOT never forks
// `go` unnecessarily.
func discoverGoRoots() []string {
	var out []string
	if g, ok := goRootFromEnv(); ok {
		out = append(out, filepath.Join(g, "src"))
	}
	if mc, ok := goModCacheFromEnv(); ok {
		out = append(out, mc)
	}
	return out
}

// goRootFromEnv returns GOROOT from env if set and non-empty, otherwise via
// `go env GOROOT`. The env probe wins to honor CI overrides.
func goRootFromEnv() (string, bool) {
	if v := strings.TrimSpace(os.Getenv("GOROOT")); v != "" {
		return v, true
	}
	if runtime.GOOS != "" {
		if v, err := runGoEnv("GOROOT"); err == nil && v != "" {
			return v, true
		}
	}
	return "", false
}

// goModCacheFromEnv returns the Go module cache location: GOMODCACHE env, then
// `go env GOMODCACHE`, then $GOPATH/pkg/mod (the legacy default).
func goModCacheFromEnv() (string, bool) {
	if v := strings.TrimSpace(os.Getenv("GOMODCACHE")); v != "" {
		return v, true
	}
	if v, err := runGoEnv("GOMODCACHE"); err == nil && v != "" {
		return v, true
	}
	if gopath := strings.TrimSpace(os.Getenv("GOPATH")); gopath != "" {
		// GOPATH may be a list on Windows; take the first.
		if i := strings.IndexAny(gopath, string(os.PathListSeparator)); i >= 0 {
			gopath = gopath[:i]
		}
		return filepath.Join(gopath, "pkg", "mod"), true
	}
	return "", false
}

// runGoEnv shells out to `go env <key>` and returns the trimmed stdout. A
// missing `go` binary or any exec error yields ("", err) so the caller can
// fall through to the next probe. We do NOT abort on error: dep-source
// discovery is best-effort, and a host without the Go toolchain simply
// produces no Go roots.
func runGoEnv(key string) (string, error) {
	cmd := exec.Command("go", "env", key)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveExistingDir canonicalizes p through any symlinks and returns it only
// if it is an existing directory. A non-existent path returns ("", false) so
// the discovery loop can drop it silently.
func resolveExistingDir(p string) (string, bool) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", false
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return "", false
	}
	return filepath.Clean(resolved), true
}

// Roots returns a copy of the configured roots. The returned slice is a fresh
// allocation so callers cannot mutate the DepSourceRoots' internal state.
func (d *DepSourceRoots) Roots() []string {
	if d == nil || len(d.roots) == 0 {
		return nil
	}
	out := make([]string, len(d.roots))
	copy(out, d.roots)
	return out
}

// Len reports how many roots the set has. Zero is a valid answer — a host
// with no ecosystems installed produces an empty set, not an error.
func (d *DepSourceRoots) Len() int {
	if d == nil {
		return 0
	}
	return len(d.roots)
}

// resolve maps a repo-relative path to an absolute on-disk path UNDER ONE OF
// THE CONFIGURED ROOTS, rejecting absolute inputs and any path that escapes
// every root. The semantics mirror FSRoot.Resolve: the same traversal
// protection (no `..`, symlink containment) and the same empty-string
// convention are honoured. Returns errDepPathEscape when the path is well
// formed but lands outside every root, so the tool can distinguish
// "this is not a dep-source path" from "the path itself is malformed".
//
// An empty dep-source set is a no-op miss: every call returns
// errDepPathEscape. The tool wiring treats that as "this path is not in any
// dep-source root" exactly like a real host with the configured ecosystem
// installed but the cited path not under it.
func (d *DepSourceRoots) resolve(rel string) (string, error) {
	if d == nil || len(d.roots) == 0 {
		return "", errDepPathEscape
	}
	rel = filepath.FromSlash(rel)
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: absolute paths are not allowed (%q)", errDepPathEscape, rel)
	}
	// Every relative path is lexically contained in EVERY root, so root order
	// alone cannot pick the right one: without an existence check the first root
	// (GOROOT/src) would always win and the module cache would be dead. Prefer
	// the root where the file actually EXISTS; remember the first lexically
	// valid candidate as a fallback so a genuine not-found still surfaces as a
	// read error at the call site rather than a misleading escape error.
	var fallback string
	for _, root := range d.roots {
		cleaned := filepath.Clean(filepath.Join(root, rel))
		if !strings.HasPrefix(cleaned, root+string(filepath.Separator)) && cleaned != root {
			// The lexical join escaped the root (e.g. `..` in rel). Try the next.
			continue
		}
		// Symlink containment: resolve the longest existing prefix of the path
		// and ensure it still lands inside the root. This defeats symlinks that
		// point outside the tree, matching the in-repo fsRoot contract.
		if resolved, err := evalExistingPrefix(cleaned); err == nil {
			if !strings.HasPrefix(resolved, root+string(filepath.Separator)) && resolved != root {
				continue
			}
		}
		if _, statErr := os.Stat(cleaned); statErr == nil {
			return cleaned, nil
		}
		if fallback == "" {
			fallback = cleaned
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("%w: %q", errDepPathEscape, rel)
}

// evalExistingPrefix is the dep-source traversal guard; it delegates to the
// package-level evalExistingPrefixPath so the in-repository and dep-source
// guards share one implementation (see pathutil.go).
func evalExistingPrefix(p string) (string, error) {
	return evalExistingPrefixPath(p)
}

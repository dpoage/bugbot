package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeFile is a tiny helper that creates parent dirs and writes a file.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func envHas(env []string, kv string) bool { return slices.Contains(env, kv) }

// assertGoBuildScratch verifies a Go resolution relocates the build cache and
// transient work dir onto the disk-backed workspace (off the size-capped /tmp
// tmpfs) via env + a mkdir setup command. See deps.go goCacheDir.
func assertGoBuildScratch(t *testing.T, res Resolution) {
	t.Helper()
	if !envHas(res.Env, "GOCACHE="+goCacheDir) {
		t.Errorf("missing GOCACHE=%s; env=%v", goCacheDir, res.Env)
	}
	if !envHas(res.Env, "GOTMPDIR="+goTmpDir) {
		t.Errorf("missing GOTMPDIR=%s; env=%v", goTmpDir, res.Env)
	}
	found := false
	for _, c := range res.SetupCmds {
		if len(c) >= 1 && c[0] == "mkdir" && slices.Contains(c, goCacheDir) && slices.Contains(c, goTmpDir) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("missing mkdir setup cmd creating %s and %s; setupCmds=%v", goCacheDir, goTmpDir, res.SetupCmds)
	}
}

func TestResolveDepsNoGoModule(t *testing.T) {
	dir := t.TempDir() // no go.mod
	for _, strat := range []DepStrategy{DepStrategyOff, DepStrategyHost, DepStrategyFetch} {
		res, err := ResolveDeps(dir, DepOptions{Strategy: strat, FetchSandbox: NewMock(MockResponse{})})
		if err != nil {
			t.Fatalf("strategy %q: unexpected error: %v", strat, err)
		}
		if len(res.ROMounts) != 0 || len(res.Env) != 0 || res.Prefetch != nil {
			t.Errorf("strategy %q on non-Go repo: want empty resolution, got %+v", strat, res)
		}
		if res.Strategy != DepStrategyOff {
			t.Errorf("strategy %q on non-Go repo: Strategy=%q want off", strat, res.Strategy)
		}
	}
}

func TestResolveDepsVendoredWinsInAllModes(t *testing.T) {
	for _, strat := range []DepStrategy{DepStrategyOff, DepStrategyHost, DepStrategyFetch} {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
		writeFile(t, filepath.Join(dir, "vendor", "modules.txt"), "# x\n")

		res, err := ResolveDeps(dir, DepOptions{Strategy: strat, FetchSandbox: NewMock(MockResponse{})})
		if err != nil {
			t.Fatalf("strategy %q: %v", strat, err)
		}
		if len(res.ROMounts) != 0 {
			t.Errorf("strategy %q vendored: want no mounts, got %+v", strat, res.ROMounts)
		}
		if !envHas(res.Env, "GOFLAGS=-mod=vendor") {
			t.Errorf("strategy %q vendored: want GOFLAGS=-mod=vendor, got %v", strat, res.Env)
		}
		if res.Prefetch != nil {
			t.Errorf("strategy %q vendored: want no prefetch", strat)
		}
		if res.Strategy != "vendored" {
			t.Errorf("strategy %q vendored: Strategy=%q want vendored", strat, res.Strategy)
		}
	}
}

func TestResolveDepsOff(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")

	res, err := ResolveDeps(dir, DepOptions{Strategy: DepStrategyOff})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	// OFF carries no module mounts or prefetch, but a Go repo still gets the
	// build-scratch relocation (GOCACHE/GOTMPDIR off the tmpfs) in every mode.
	if len(res.ROMounts) != 0 || res.Prefetch != nil {
		t.Errorf("off on non-vendored Go repo: want no mounts/prefetch, got %+v", res)
	}
	if envHas(res.Env, "GOFLAGS=-mod=vendor") {
		t.Errorf("off (non-vendored) must not set GOFLAGS, got %v", res.Env)
	}
	assertGoBuildScratch(t, res)
	if res.Strategy != DepStrategyOff {
		t.Errorf("Strategy=%q want off", res.Strategy)
	}
}

// TestResolveDepsGoBuildScratchAllStrategies locks in that EVERY Go strategy
// (off/host/fetch/vendored) relocates the build cache off the /tmp tmpfs, and
// that a non-Go repo gets none of it. This is the regression guard for the
// "no space left on device" environment_error a cold Go build hits when GOCACHE
// and $WORK share the small sandbox tmpfs.
func TestResolveDepsGoBuildScratchAllStrategies(t *testing.T) {
	cache := t.TempDir()
	cacheBase := t.TempDir()
	cases := []struct {
		name string
		opts DepOptions
	}{
		{"off", DepOptions{Strategy: DepStrategyOff}},
		{"host", DepOptions{Strategy: DepStrategyHost, hostModcache: cache}},
		{"fetch", DepOptions{Strategy: DepStrategyFetch, FetchSandbox: NewMock(MockResponse{Result: Result{ExitCode: 0}}), userCacheDir: cacheBase}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
			writeFile(t, filepath.Join(dir, "go.sum"), "example.com/x v1.0.0 h1:abc\n")
			res, err := ResolveDeps(dir, tc.opts)
			if err != nil {
				t.Fatalf("ResolveDeps: %v", err)
			}
			assertGoBuildScratch(t, res)
		})
	}

	t.Run("vendored", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
		writeFile(t, filepath.Join(dir, "vendor", "modules.txt"), "# x\n")
		res, err := ResolveDeps(dir, DepOptions{Strategy: DepStrategyOff})
		if err != nil {
			t.Fatalf("ResolveDeps: %v", err)
		}
		assertGoBuildScratch(t, res)
	})

	t.Run("non-go", func(t *testing.T) {
		dir := t.TempDir()
		res, err := ResolveDeps(dir, DepOptions{Strategy: DepStrategyOff})
		if err != nil {
			t.Fatalf("ResolveDeps: %v", err)
		}
		if envHas(res.Env, "GOCACHE="+goCacheDir) || len(res.SetupCmds) != 0 {
			t.Errorf("non-Go repo must not get Go build scratch; got env=%v setup=%v", res.Env, res.SetupCmds)
		}
	})
}

func TestResolveDepsHostMountsCache(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	cache := t.TempDir() // exists on disk

	res, err := ResolveDeps(dir, DepOptions{Strategy: DepStrategyHost, hostModcache: cache})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	if len(res.ROMounts) != 1 {
		t.Fatalf("want 1 ROMount, got %d", len(res.ROMounts))
	}
	m := res.ROMounts[0]
	if m.HostPath != cache || m.ContainerPath != modcacheMount {
		t.Errorf("mount = %+v, want host=%q ctr=%q", m, cache, modcacheMount)
	}
	// Host strategy: the mount must be Shared=true (no SELinux :Z relabel) to
	// avoid corrupting the user's shared Go module cache on SELinux-enforcing
	// hosts (Fedora/RHEL).
	if !m.Shared {
		t.Errorf("host strategy ROMount.Shared = false; want true to suppress :Z relabel on shared host cache")
	}
	for _, want := range []string{"GOMODCACHE=" + modcacheMount, "GOFLAGS=-mod=mod", "GOPROXY=off"} {
		if !envHas(res.Env, want) {
			t.Errorf("host env missing %q; got %v", want, res.Env)
		}
	}
	if res.Prefetch != nil {
		t.Error("host strategy must not set a prefetch hook")
	}
	if res.Strategy != DepStrategyHost {
		t.Errorf("Strategy=%q want host", res.Strategy)
	}
}

func TestResolveDepsHostMissingCacheErrors(t *testing.T) {
	// The host cache override points at a directory that does not exist.
	// ResolveDeps must return a clear error instead of letting podman fail
	// opaquely at run time.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	missing := filepath.Join(t.TempDir(), "does", "not", "exist")

	_, err := ResolveDeps(dir, DepOptions{Strategy: DepStrategyHost, hostModcache: missing})
	if err == nil {
		t.Fatal("expected error for missing host module cache, got nil")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error %q should mention 'does not exist'", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q should include the missing path %q", err, missing)
	}
}

func TestResolveDepsFetchRequiresSandbox(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")

	_, err := ResolveDeps(dir, DepOptions{Strategy: DepStrategyFetch}) // nil FetchSandbox
	if err == nil {
		t.Fatal("fetch with nil sandbox should error")
	}
	if !strings.Contains(err.Error(), "fetch sandbox") {
		t.Errorf("error = %v, want mention of fetch sandbox", err)
	}
}

func TestResolveDepsFetchMountsAndPrefetches(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "go.sum"), "example.com/x v1.0.0 h1:abc\n")
	cacheBase := t.TempDir()

	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	res, err := ResolveDeps(dir, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: mock,
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	if len(res.ROMounts) != 1 || res.ROMounts[0].ContainerPath != modcacheMount {
		t.Fatalf("want one modcache mount, got %+v", res.ROMounts)
	}
	if !strings.HasPrefix(res.ROMounts[0].HostPath, cacheBase) {
		t.Errorf("fetch cache %q should live under user cache base %q", res.ROMounts[0].HostPath, cacheBase)
	}
	// Fetch strategy: the mount must be Shared=false (gets :Z) because the
	// fetch cache is a bugbot-owned directory, not a shared host resource.
	if res.ROMounts[0].Shared {
		t.Errorf("fetch strategy ROMount.Shared = true; want false (bugbot-owned dir should get :Z isolation)")
	}
	for _, want := range []string{"GOMODCACHE=" + modcacheMount, "GOPROXY=off"} {
		if !envHas(res.Env, want) {
			t.Errorf("fetch env missing %q; got %v", want, res.Env)
		}
	}
	if res.Prefetch == nil {
		t.Fatal("fetch strategy must set a prefetch hook")
	}

	// Running the prefetch must invoke the sandbox with a network-enabled,
	// writable-cache `go mod download` spec.
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	calls := mock.Calls()
	if len(calls) != 1 {
		t.Fatalf("prefetch should run exactly one container, got %d", len(calls))
	}
	spec := calls[0].Spec
	if spec.Network == "" || spec.Network == "none" {
		t.Errorf("prefetch network = %q, want a real network (not none/empty)", spec.Network)
	}
	if !slices.Equal(spec.Cmd, []string{"go", "mod", "download", "all"}) {
		t.Errorf("prefetch cmd = %v", spec.Cmd)
	}
	if len(spec.RWMounts) != 1 || spec.RWMounts[0].ContainerPath != modcacheMount {
		t.Errorf("prefetch must bind the cache WRITABLE at the modcache mount; got %+v", spec.RWMounts)
	}
	if len(spec.ROMounts) != 0 {
		t.Errorf("prefetch should not use read-only mounts; got %+v", spec.ROMounts)
	}

	// Second call must be a no-op (sync.Once): no additional container.
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("prefetch second call: %v", err)
	}
	if mock.CallCount() != 1 {
		t.Errorf("prefetch ran %d times, want 1 (sync.Once)", mock.CallCount())
	}
}

func TestResolveDepsFetchSkipsWhenCacheWarm(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "go.sum"), "example.com/x v1.0.0 h1:abc\n")
	cacheBase := t.TempDir()

	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})
	res, err := ResolveDeps(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("first prefetch: %v", err)
	}
	if mock.CallCount() != 1 {
		t.Fatalf("first prefetch want 1 container, got %d", mock.CallCount())
	}

	// A fresh Resolution (new sync.Once) over the same warm cache + unchanged
	// go.sum must skip the download entirely (sentinel hit).
	res2, err := ResolveDeps(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("ResolveDeps 2: %v", err)
	}
	if err := res2.Prefetch(context.Background()); err != nil {
		t.Fatalf("second prefetch: %v", err)
	}
	if mock.CallCount() != 1 {
		t.Errorf("warm cache should skip download; container ran %d times, want 1", mock.CallCount())
	}
}

func TestResolveDepsFetchPrefetchSurfacesFailure(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "go.sum"), "example.com/x v1.0.0 h1:abc\n")
	cacheBase := t.TempDir()

	mock := NewMock(MockResponse{Result: Result{ExitCode: 1, Stderr: "no internet"}})
	res, err := ResolveDeps(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	if perr := res.Prefetch(context.Background()); perr == nil {
		t.Fatal("prefetch with non-zero download exit should error")
	}
}

func TestResolveDepsInvalidStrategy(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	if _, err := ResolveDeps(dir, DepOptions{Strategy: DepStrategy("bogus")}); err == nil {
		t.Fatal("invalid strategy should error")
	}
}

func TestValidDepStrategy(t *testing.T) {
	for _, s := range []DepStrategy{"", DepStrategyOff, DepStrategyHost, DepStrategyFetch} {
		if !ValidDepStrategy(s) {
			t.Errorf("ValidDepStrategy(%q) = false, want true", s)
		}
	}
	if ValidDepStrategy("nope") {
		t.Error("ValidDepStrategy(nope) = true, want false")
	}
}

// --- Ecosystem registry / resolveWith composition tests ---

// testEcosystem builds a test-only ecosystem entry that returns a fixed
// Resolution with the given mount, env, setup commands, and prefetch hook.
func testEcosystem(name, detectMarker string, res Resolution, prefetchFn func(context.Context) error) ecosystem {
	return ecosystem{
		name: name,
		detect: func(repoDir string) bool {
			_, err := os.Stat(filepath.Join(repoDir, detectMarker))
			return err == nil
		},
		resolve: func(_ string, _ DepOptions) (Resolution, error) {
			r := res
			r.Prefetch = prefetchFn
			return r, nil
		},
	}
}

// TestResolveWith_SingleEcosystem asserts that resolveWith with only the Go
// ecosystem produces the exact same result as the pre-refactor ResolveDeps for
// every strategy variant (it is the acceptance criterion for byte-identical
// behavior).
func TestResolveWith_SingleEcosystem(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	cache := t.TempDir()

	// OFF strategy with only Go ecosystem.
	res, err := resolveWith(ecosystems, dir, DepOptions{Strategy: DepStrategyOff})
	if err != nil {
		t.Fatalf("resolveWith off: %v", err)
	}
	if res.Strategy != DepStrategyOff || len(res.ROMounts) != 0 {
		t.Errorf("off: unexpected resolution %+v", res)
	}
	assertGoBuildScratch(t, res) // Go always relocates build scratch, even OFF

	// HOST strategy.
	res, err = resolveWith(ecosystems, dir, DepOptions{Strategy: DepStrategyHost, hostModcache: cache})
	if err != nil {
		t.Fatalf("resolveWith host: %v", err)
	}
	if res.Strategy != DepStrategyHost || len(res.ROMounts) != 1 {
		t.Errorf("host: unexpected resolution %+v", res)
	}
	// Go contributes exactly the build-scratch mkdir as its only setup cmd.
	if len(res.SetupCmds) != 1 {
		t.Errorf("Go ecosystem should contribute exactly the build-scratch mkdir; got %v", res.SetupCmds)
	}
	assertGoBuildScratch(t, res)
}

// TestResolveWith_NoMatch asserts that a repo matching no ecosystem resolves
// to Resolution{Strategy: DepStrategyOff}, consistent with the pre-refactor
// non-Go behavior.
func TestResolveWith_NoMatch(t *testing.T) {
	dir := t.TempDir() // no go.mod, no other markers
	res, err := resolveWith(ecosystems, dir, DepOptions{})
	if err != nil {
		t.Fatalf("no match: %v", err)
	}
	if res.Strategy != DepStrategyOff {
		t.Errorf("no-match Strategy = %q, want off", res.Strategy)
	}
	if len(res.ROMounts) != 0 || len(res.Env) != 0 || res.Prefetch != nil {
		t.Errorf("no-match: want empty resolution, got %+v", res)
	}
}

// TestResolveWith_MultiEcosystem asserts the merge semantics when two
// ecosystems both match:
//   - ROMounts are appended in table order.
//   - Env is appended in table order.
//   - SetupCmds is appended in table order.
//   - Strategy is taken from the first matching ecosystem.
//   - Prefetch funcs are chained sequentially (both called; first error wins).
func TestResolveWith_MultiEcosystem(t *testing.T) {
	dir := t.TempDir()
	// Both markers present so both ecosystems match.
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "package.json"), "{}")

	goMount := ROMount{HostPath: "/go-cache", ContainerPath: "/modcache"}
	jsMount := ROMount{HostPath: "/js-cache", ContainerPath: "/jscache"}

	var prefetchOrder []string

	goEco := testEcosystem("go", "go.mod", Resolution{
		ROMounts:  []ROMount{goMount},
		Env:       []string{"GOMODCACHE=/modcache"},
		SetupCmds: nil, // Go contributes none
		Strategy:  DepStrategyHost,
	}, func(_ context.Context) error {
		prefetchOrder = append(prefetchOrder, "go")
		return nil
	})

	jsEco := testEcosystem("js", "package.json", Resolution{
		ROMounts:  []ROMount{jsMount},
		Env:       []string{"NPM_CONFIG_CACHE=/jscache"},
		SetupCmds: [][]string{{"npm", "ci", "--offline"}},
		Strategy:  DepStrategy("npm"),
	}, func(_ context.Context) error {
		prefetchOrder = append(prefetchOrder, "js")
		return nil
	})

	table := []ecosystem{goEco, jsEco}
	res, err := resolveWith(table, dir, DepOptions{})
	if err != nil {
		t.Fatalf("resolveWith: %v", err)
	}

	// Strategy from first match.
	if res.Strategy != DepStrategyHost {
		t.Errorf("Strategy = %q, want host (first match)", res.Strategy)
	}

	// ROMounts: go first, js second.
	if len(res.ROMounts) != 2 {
		t.Fatalf("ROMounts len = %d, want 2", len(res.ROMounts))
	}
	if res.ROMounts[0].ContainerPath != "/modcache" || res.ROMounts[1].ContainerPath != "/jscache" {
		t.Errorf("ROMounts order wrong: %+v", res.ROMounts)
	}

	// Env: go first, js second.
	if len(res.Env) != 2 || res.Env[0] != "GOMODCACHE=/modcache" || res.Env[1] != "NPM_CONFIG_CACHE=/jscache" {
		t.Errorf("Env = %v, want [GOMODCACHE=/modcache NPM_CONFIG_CACHE=/jscache]", res.Env)
	}

	// SetupCmds: js contributes; go contributes none.
	if len(res.SetupCmds) != 1 || !slices.Equal(res.SetupCmds[0], []string{"npm", "ci", "--offline"}) {
		t.Errorf("SetupCmds = %v, want [[npm ci --offline]]", res.SetupCmds)
	}

	// Prefetch: chained; both called in order.
	if res.Prefetch == nil {
		t.Fatal("Prefetch must be non-nil when any ecosystem contributes one")
	}
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("chained Prefetch: %v", err)
	}
	if !slices.Equal(prefetchOrder, []string{"go", "js"}) {
		t.Errorf("prefetch call order = %v, want [go js]", prefetchOrder)
	}
}

// TestResolveWith_PrefetchChainFirstErrorWins asserts that when the first
// ecosystem's prefetch fails, the chain aborts and the second is not called.
func TestResolveWith_PrefetchChainFirstErrorWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "package.json"), "{}")

	secondCalled := false

	goEco := testEcosystem("go", "go.mod", Resolution{
		Strategy: DepStrategyHost,
	}, func(_ context.Context) error {
		return fmt.Errorf("prefetch go: boom")
	})
	jsEco := testEcosystem("js", "package.json", Resolution{
		Strategy: DepStrategy("npm"),
	}, func(_ context.Context) error {
		secondCalled = true
		return nil
	})

	table := []ecosystem{goEco, jsEco}
	res, err := resolveWith(table, dir, DepOptions{})
	if err != nil {
		t.Fatalf("resolveWith: %v", err)
	}
	if res.Prefetch == nil {
		t.Fatal("Prefetch must be non-nil")
	}
	if perr := res.Prefetch(context.Background()); perr == nil {
		t.Fatal("expected error from first prefetch, got nil")
	}
	if secondCalled {
		t.Error("second prefetch must not be called after first fails")
	}
}

// TestResolveWith_GoOnlyRepo asserts that a pure-Go repo (no other ecosystem
// markers) produces exactly the same Resolution as the pre-refactor ResolveDeps.
// This is the byte-identical acceptance criterion.
func TestResolveWith_GoOnlyRepo(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	cache := t.TempDir()

	// Using resolveWith directly with the production table.
	got, err := resolveWith(ecosystems, dir, DepOptions{Strategy: DepStrategyHost, hostModcache: cache})
	if err != nil {
		t.Fatalf("resolveWith: %v", err)
	}
	// Also call the public ResolveDeps (which uses the same table) for comparison.
	want, err := ResolveDeps(dir, DepOptions{Strategy: DepStrategyHost, hostModcache: cache})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}

	// ROMounts must match.
	if len(got.ROMounts) != len(want.ROMounts) {
		t.Fatalf("ROMounts len: resolveWith=%d ResolveDeps=%d", len(got.ROMounts), len(want.ROMounts))
	}
	for i, m := range got.ROMounts {
		if m != want.ROMounts[i] {
			t.Errorf("ROMounts[%d]: got %+v, want %+v", i, m, want.ROMounts[i])
		}
	}
	// Env must match.
	if !slices.Equal(got.Env, want.Env) {
		t.Errorf("Env: got %v, want %v", got.Env, want.Env)
	}
	// SetupCmds must match between resolveWith and ResolveDeps, and contain the
	// Go build-scratch mkdir.
	if len(got.SetupCmds) != len(want.SetupCmds) {
		t.Errorf("SetupCmds len: resolveWith=%d ResolveDeps=%d", len(got.SetupCmds), len(want.SetupCmds))
	}
	assertGoBuildScratch(t, got)
	if got.Strategy != want.Strategy {
		t.Errorf("Strategy: got %q, want %q", got.Strategy, want.Strategy)
	}
}

// ---- Python ecosystem unit tests -------------------------------------------

// TestPythonDetectOnOff: detect fires on requirements.txt, not without.
func TestPythonDetectOnOff(t *testing.T) {
	// Without requirements.txt — Python ecosystem must resolve to OFF.
	dirNo := t.TempDir()
	if hasRequirementsTxt(dirNo) {
		t.Error("hasRequirementsTxt: want false for dir without requirements.txt")
	}
	// With requirements.txt — must detect.
	dirYes := t.TempDir()
	writeFile(t, filepath.Join(dirYes, "requirements.txt"), "six==1.16.0\n")
	if !hasRequirementsTxt(dirYes) {
		t.Error("hasRequirementsTxt: want true for dir with requirements.txt")
	}
}

// TestPythonResolveOff: OFF strategy on a Python repo returns an empty Resolution.
func TestPythonResolveOff(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "requirements.txt"), "six==1.16.0\n")

	res, err := resolvePython(dir, DepOptions{Strategy: DepStrategyOff})
	if err != nil {
		t.Fatalf("resolvePython OFF: %v", err)
	}
	if len(res.ROMounts) != 0 || len(res.Env) != 0 || len(res.SetupCmds) != 0 || res.Prefetch != nil {
		t.Errorf("Python OFF: want empty resolution, got %+v", res)
	}
	if res.Strategy != DepStrategyOff {
		t.Errorf("Python OFF: Strategy=%q want off", res.Strategy)
	}
}

// TestPythonResolveHostIsOff: HOST strategy for Python also returns OFF
// (pip HTTP cache is not a wheelhouse; --no-index installs need wheel files).
func TestPythonResolveHostIsOff(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "requirements.txt"), "six==1.16.0\n")

	res, err := resolvePython(dir, DepOptions{Strategy: DepStrategyHost})
	if err != nil {
		t.Fatalf("resolvePython HOST: %v", err)
	}
	if len(res.ROMounts) != 0 || len(res.Env) != 0 || len(res.SetupCmds) != 0 || res.Prefetch != nil {
		t.Errorf("Python HOST must resolve to OFF (pip HTTP cache not a wheelhouse), got %+v", res)
	}
	if res.Strategy != DepStrategyOff {
		t.Errorf("Python HOST: Strategy=%q want off", res.Strategy)
	}
}

// TestPythonResolveFetchShape: FETCH strategy returns the correct mount,
// env, SetupCmds, and a non-nil Prefetch hook.
func TestPythonResolveFetchShape(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "requirements.txt"), "six==1.16.0\npytest==8.0.0\n")
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	res, err := resolvePython(dir, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: mock,
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("resolvePython FETCH: %v", err)
	}

	// Must have exactly one RO mount at /depcache, bugbot-owned (Shared=false).
	if len(res.ROMounts) != 1 {
		t.Fatalf("want 1 RO mount, got %d: %+v", len(res.ROMounts), res.ROMounts)
	}
	m := res.ROMounts[0]
	if m.ContainerPath != pipCacheMount {
		t.Errorf("RO mount ContainerPath = %q, want %q", m.ContainerPath, pipCacheMount)
	}
	if !strings.HasPrefix(m.HostPath, cacheBase) {
		t.Errorf("pip cache %q should live under user cache base %q", m.HostPath, cacheBase)
	}
	if m.Shared {
		t.Error("Python FETCH ROMount.Shared = true; want false (bugbot-owned dir should get :Z isolation)")
	}

	// PIP_NO_INDEX=1 must be in env (offline enforcement).
	if !envHas(res.Env, "PIP_NO_INDEX=1") {
		t.Errorf("Python FETCH env missing PIP_NO_INDEX=1; got %v", res.Env)
	}

	// SetupCmds must include pip install --user --no-index --find-links=/depcache -r requirements.txt.
	if len(res.SetupCmds) != 1 {
		t.Fatalf("want 1 SetupCmd, got %d: %v", len(res.SetupCmds), res.SetupCmds)
	}
	cmd := res.SetupCmds[0]
	wantSetup := []string{"pip", "install", "--user", "--no-index", "--find-links=" + pipCacheMount, "-r", "requirements.txt"}
	if !slices.Equal(cmd, wantSetup) {
		t.Errorf("SetupCmds[0] = %v, want %v", cmd, wantSetup)
	}

	// Prefetch must be non-nil.
	if res.Prefetch == nil {
		t.Fatal("Python FETCH must set a prefetch hook")
	}
	if res.Strategy != DepStrategyFetch {
		t.Errorf("Strategy = %q, want fetch", res.Strategy)
	}
}

// TestPythonResolveFetchPrefetchSpec: Running the prefetch hook invokes the
// sandbox with a network-enabled, writable-cache `pip download` spec.
func TestPythonResolveFetchPrefetchSpec(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "requirements.txt"), "six==1.16.0\n")
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	res, err := resolvePython(dir, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: mock,
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("resolvePython: %v", err)
	}
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	calls := mock.Calls()
	if len(calls) != 1 {
		t.Fatalf("prefetch should run exactly one container, got %d", len(calls))
	}
	spec := calls[0].Spec
	if spec.Network == "" || spec.Network == "none" {
		t.Errorf("prefetch network = %q, want a real network (not none/empty)", spec.Network)
	}
	wantCmd := []string{"pip", "download", "-r", "requirements.txt", "-d", pipCacheMount}
	if !slices.Equal(spec.Cmd, wantCmd) {
		t.Errorf("prefetch cmd = %v, want %v", spec.Cmd, wantCmd)
	}
	if len(spec.RWMounts) != 1 || spec.RWMounts[0].ContainerPath != pipCacheMount {
		t.Errorf("prefetch must bind the cache WRITABLE at %s; got %+v", pipCacheMount, spec.RWMounts)
	}
	if len(spec.ROMounts) != 0 {
		t.Errorf("prefetch should not use read-only mounts; got %+v", spec.ROMounts)
	}
	// sync.Once: second call must not re-run the container.
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("prefetch second call: %v", err)
	}
	if mock.CallCount() != 1 {
		t.Errorf("prefetch ran %d times, want 1 (sync.Once)", mock.CallCount())
	}
}

// TestPythonPrefetchSentinelSkip: a fresh Resolution over a warm cache (same
// requirements.txt hash) must skip the download entirely (sentinel hit).
func TestPythonPrefetchSentinelSkip(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "requirements.txt"), "six==1.16.0\n")
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	res, err := resolvePython(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("first ResolveDeps: %v", err)
	}
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("first prefetch: %v", err)
	}
	if mock.CallCount() != 1 {
		t.Fatalf("first prefetch: want 1 container, got %d", mock.CallCount())
	}

	// Fresh Resolution (new sync.Once) but warm sentinel → must skip.
	res2, err := resolvePython(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("second ResolveDeps: %v", err)
	}
	if err := res2.Prefetch(context.Background()); err != nil {
		t.Fatalf("second prefetch: %v", err)
	}
	if mock.CallCount() != 1 {
		t.Errorf("warm sentinel should skip download; container ran %d times, want 1", mock.CallCount())
	}
}

// TestPythonFetchRequiresSandbox: FETCH with nil FetchSandbox must error.
func TestPythonFetchRequiresSandbox(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "requirements.txt"), "six==1.16.0\n")
	_, err := resolvePython(dir, DepOptions{Strategy: DepStrategyFetch}) // nil sandbox
	if err == nil {
		t.Fatal("Python FETCH with nil sandbox should error")
	}
	if !strings.Contains(err.Error(), "fetch sandbox") {
		t.Errorf("error = %v, want mention of fetch sandbox", err)
	}
}

// TestMultiEcosystemComposition: a repo with both go.mod and requirements.txt
// gets both the Go modcache mount and the Python wheelhouse mount, and the
// merged Resolution carries Python's SetupCmds.
//
// Strategy semantics: main's resolveWith uses FIRST-MATCH strategy, not
// "multi". Since Go is the first ecosystem in the table and it matches (FETCH),
// Strategy == DepStrategyFetch. This differs from the old branch's "multi"
// strategy — the first-match rule is the canonical design; see the resolveWith
// doc comment for the rationale.
func TestMultiEcosystemComposition(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	writeFile(t, filepath.Join(dir, "go.sum"), "example.com/x v1.0.0 h1:abc\n")
	writeFile(t, filepath.Join(dir, "requirements.txt"), "six==1.16.0\npytest==8.0.0\n")
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	res, err := ResolveDeps(dir, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: mock,
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("ResolveDeps multi-ecosystem: %v", err)
	}

	// Must have exactly two mounts: one at /modcache (Go), one at /depcache (Python).
	if len(res.ROMounts) != 2 {
		t.Fatalf("want 2 RO mounts (Go + Python), got %d: %+v", len(res.ROMounts), res.ROMounts)
	}
	containerPaths := make(map[string]bool)
	for _, m := range res.ROMounts {
		containerPaths[m.ContainerPath] = true
	}
	if !containerPaths[modcacheMount] {
		t.Errorf("missing Go modcache mount at %s; mounts: %+v", modcacheMount, res.ROMounts)
	}
	if !containerPaths[pipCacheMount] {
		t.Errorf("missing Python wheelhouse mount at %s; mounts: %+v", pipCacheMount, res.ROMounts)
	}

	// Go env must be present.
	if !envHas(res.Env, "GOPROXY=off") {
		t.Errorf("missing GOPROXY=off in multi-ecosystem env: %v", res.Env)
	}
	// Python env must be present.
	if !envHas(res.Env, "PIP_NO_INDEX=1") {
		t.Errorf("missing PIP_NO_INDEX=1 in multi-ecosystem env: %v", res.Env)
	}

	// SetupCmds = Go build-scratch mkdir (table order: go before python) then
	// the Python pip install.
	if len(res.SetupCmds) != 2 {
		t.Fatalf("want 2 SetupCmds (Go mkdir + Python pip install), got %d: %v", len(res.SetupCmds), res.SetupCmds)
	}
	assertGoBuildScratch(t, res)
	foundPip := false
	for _, c := range res.SetupCmds {
		if len(c) > 0 && c[0] == "pip" {
			foundPip = true
		}
	}
	if !foundPip {
		t.Errorf("missing Python pip install SetupCmd; got %v", res.SetupCmds)
	}

	// Strategy is from the FIRST matching ecosystem (Go = "fetch"), per
	// resolveWith's first-match rule. The old branch used "multi" here; that
	// was specific to its divergent resolveWith — the canonical design is
	// first-match.
	if res.Strategy != DepStrategyFetch {
		t.Errorf("Strategy = %q, want fetch (first-match: Go fires first)", res.Strategy)
	}

	// A non-nil Prefetch must have been set (chained: Go + Python prefetches).
	if res.Prefetch == nil {
		t.Fatal("multi-ecosystem FETCH: Prefetch must be non-nil")
	}
}

// ---------------------------------------------------------------------------
// Tests for DepOptions.LocalMounts (composable local-mount dep layer, bugbot-ixu)
// and operator SetupCmds ordering (bugbot-ftd.2).
// ---------------------------------------------------------------------------

// TestResolveDepsLocalMounts verifies that LocalMounts in DepOptions are
// appended to the Resolution.ROMounts regardless of ecosystem or dep strategy.
func TestResolveDepsLocalMounts(t *testing.T) {
	hostDir := t.TempDir()
	mount := ROMount{HostPath: hostDir, ContainerPath: "/sibling", Shared: true}

	t.Run("off strategy no go.mod still gets local mounts", func(t *testing.T) {
		dir := t.TempDir() // no go.mod
		res, err := ResolveDeps(dir, DepOptions{
			Strategy:    DepStrategyOff,
			LocalMounts: []ROMount{mount},
		})
		if err != nil {
			t.Fatalf("ResolveDeps: %v", err)
		}
		if len(res.ROMounts) != 1 || res.ROMounts[0].ContainerPath != "/sibling" {
			t.Errorf("want 1 local mount at /sibling, got %+v", res.ROMounts)
		}
		if !res.ROMounts[0].Shared {
			t.Error("local mount Shared should be true")
		}
	})

	t.Run("host strategy Go repo: local mounts compose with modcache mount", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
		cache := t.TempDir()
		res, err := ResolveDeps(dir, DepOptions{
			Strategy:     DepStrategyHost,
			hostModcache: cache,
			LocalMounts:  []ROMount{mount},
		})
		if err != nil {
			t.Fatalf("ResolveDeps: %v", err)
		}
		// Must have at least the modcache mount + the local mount.
		hasModcache := false
		hasSibling := false
		for _, m := range res.ROMounts {
			if m.ContainerPath == modcacheMount {
				hasModcache = true
			}
			if m.ContainerPath == "/sibling" {
				hasSibling = true
			}
		}
		if !hasModcache {
			t.Errorf("host strategy: missing modcache mount; mounts=%+v", res.ROMounts)
		}
		if !hasSibling {
			t.Errorf("host strategy: missing local /sibling mount; mounts=%+v", res.ROMounts)
		}
	})

	t.Run("fetch strategy Go repo: local mounts compose with fetch cache mount", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
		writeFile(t, filepath.Join(dir, "go.sum"), "")
		cacheBase := t.TempDir()
		res, err := ResolveDeps(dir, DepOptions{
			Strategy:     DepStrategyFetch,
			FetchSandbox: NewMock(MockResponse{Result: Result{ExitCode: 0}}),
			userCacheDir: cacheBase,
			LocalMounts:  []ROMount{mount},
		})
		if err != nil {
			t.Fatalf("ResolveDeps: %v", err)
		}
		hasFetchCache := false
		hasSibling := false
		for _, m := range res.ROMounts {
			if m.ContainerPath == modcacheMount {
				hasFetchCache = true
			}
			if m.ContainerPath == "/sibling" {
				hasSibling = true
			}
		}
		if !hasFetchCache {
			t.Errorf("fetch strategy: missing modcache mount; mounts=%+v", res.ROMounts)
		}
		if !hasSibling {
			t.Errorf("fetch strategy: missing local /sibling mount; mounts=%+v", res.ROMounts)
		}
	})

	t.Run("multiple local mounts all appear", func(t *testing.T) {
		dir := t.TempDir()
		hostDir2 := t.TempDir()
		res, err := ResolveDeps(dir, DepOptions{
			Strategy: DepStrategyOff,
			LocalMounts: []ROMount{
				{HostPath: hostDir, ContainerPath: "/sibling1", Shared: true},
				{HostPath: hostDir2, ContainerPath: "/sibling2", Shared: true},
			},
		})
		if err != nil {
			t.Fatalf("ResolveDeps: %v", err)
		}
		if len(res.ROMounts) != 2 {
			t.Fatalf("want 2 mounts, got %d: %+v", len(res.ROMounts), res.ROMounts)
		}
	})
}

// TestResolveDepsLocalMountsOrderAfterEcosystem verifies that local mounts
// appear AFTER ecosystem-derived mounts in the Resolution (ecosystem mounts
// first, then operator local mounts).
func TestResolveDepsLocalMountsOrderAfterEcosystem(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	hostDir := t.TempDir()
	cache := t.TempDir()

	res, err := ResolveDeps(dir, DepOptions{
		Strategy:     DepStrategyHost,
		hostModcache: cache,
		LocalMounts:  []ROMount{{HostPath: hostDir, ContainerPath: "/sibling", Shared: true}},
	})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	// Ecosystem mount (modcache at /modcache) must appear before local mount.
	ecosystemIdx := -1
	localIdx := -1
	for i, m := range res.ROMounts {
		if m.ContainerPath == modcacheMount {
			ecosystemIdx = i
		}
		if m.ContainerPath == "/sibling" {
			localIdx = i
		}
	}
	if ecosystemIdx < 0 {
		t.Fatal("ecosystem modcache mount not found")
	}
	if localIdx < 0 {
		t.Fatal("local /sibling mount not found")
	}
	if ecosystemIdx >= localIdx {
		t.Errorf("ecosystem mount (idx %d) must appear before local mount (idx %d)", ecosystemIdx, localIdx)
	}
}

// TestResolveWithOperatorSetupCmdsOrder verifies that when operator SetupCmds
// are prepended at the call site, they appear BEFORE ecosystem-derived setup
// commands. This tests the contract documented in config.Sandbox.SetupCmds and
// repro/funnel call sites: operator cmds (system libs) run before ecosystem
// offline-install cmds so system-level deps are present for the ecosystem tool.
//
// Note: the prepend itself happens at the consumer call sites (repro.go,
// funnel.go), not inside ResolveDeps. This test validates the merge result
// by simulating that caller logic.
func TestOperatorSetupCmdsOrderBeforeEcosystem(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "requirements.txt"), "requests==2.31.0\n")
	cacheBase := t.TempDir()

	// Simulate: Python FETCH resolution produces SetupCmds (pip install).
	res, err := ResolveDeps(dir, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: NewMock(MockResponse{Result: Result{ExitCode: 0}}),
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}

	ecosystemCmds := append([][]string(nil), res.SetupCmds...)
	if len(ecosystemCmds) == 0 {
		t.Skip("Python FETCH produced no ecosystem setup cmds; test not applicable")
	}

	// Simulate the operator-prepend logic in repro.New / funnel.New.
	operatorCmds := [][]string{{"apt-get", "install", "-y", "libssl-dev"}}
	merged := append(operatorCmds, res.SetupCmds...)

	// Operator cmd must appear first.
	if len(merged) == 0 || merged[0][0] != "apt-get" {
		t.Errorf("operator cmd must be first; got %v", merged)
	}
	// Ecosystem cmds must still be present.
	for i, eco := range ecosystemCmds {
		found := false
		for _, m := range merged {
			if len(m) > 0 && len(eco) > 0 && m[0] == eco[0] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ecosystem setup cmd %d (%v) missing from merged; merged=%v", i, eco, merged)
		}
	}
}

// ---- Rust/Cargo ecosystem unit tests ----------------------------------------

// TestCargoDetectOnOff: detect fires on Cargo.toml, not without.
func TestCargoDetectOnOff(t *testing.T) {
	dirNo := t.TempDir()
	if hasCargoToml(dirNo) {
		t.Error("hasCargoToml: want false for dir without Cargo.toml")
	}
	dirYes := t.TempDir()
	writeFile(t, filepath.Join(dirYes, "Cargo.toml"), "[package]\nname = \"hello\"\n")
	if !hasCargoToml(dirYes) {
		t.Error("hasCargoToml: want true for dir with Cargo.toml")
	}
}

// TestCargoVendoredDetection: isCargoVendored is true only when BOTH vendor/
// exists AND .cargo/config.toml contains replace-with.
func TestCargoVendoredDetection(t *testing.T) {
	// vendor/ + config with replace-with → vendored.
	dirYes := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dirYes, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dirYes, ".cargo", "config.toml"),
		"[source.crates-io]\nreplace-with = \"vendored-sources\"\n[source.vendored-sources]\ndirectory = \"vendor\"\n")
	if !isCargoVendored(dirYes) {
		t.Error("isCargoVendored: want true when vendor/ + config with replace-with")
	}

	// vendor/ without config → NOT vendored (cargo ignores vendor/).
	dirVendorOnly := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dirVendorOnly, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isCargoVendored(dirVendorOnly) {
		t.Error("isCargoVendored: want false when vendor/ exists but no .cargo/config stanza — cargo ignores vendor/ without source replacement")
	}

	// config with replace-with but no vendor/ → NOT vendored.
	dirConfigOnly := t.TempDir()
	writeFile(t, filepath.Join(dirConfigOnly, ".cargo", "config.toml"),
		"[source.crates-io]\nreplace-with = \"vendored-sources\"\n")
	if isCargoVendored(dirConfigOnly) {
		t.Error("isCargoVendored: want false when .cargo/config exists but no vendor/")
	}

	// Legacy .cargo/config (no .toml extension) is also recognized.
	dirLegacy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dirLegacy, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dirLegacy, ".cargo", "config"),
		"[source.crates-io]\nreplace-with = \"vendored-sources\"\n")
	if !isCargoVendored(dirLegacy) {
		t.Error("isCargoVendored: want true for legacy .cargo/config (no .toml extension)")
	}
}

// TestCargoResolveVendoredWinsInAllModes: vendored detection overrides every
// requested strategy, sets CARGO_NET_OFFLINE=true, no mounts, Strategy=vendored.
func TestCargoResolveVendoredWinsInAllModes(t *testing.T) {
	for _, strategy := range []DepStrategy{DepStrategyOff, DepStrategyHost, DepStrategyFetch, ""} {
		t.Run(string(strategy), func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname=\"x\"\n")
			if err := os.MkdirAll(filepath.Join(dir, "vendor"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, ".cargo", "config.toml"),
				"[source.crates-io]\nreplace-with = \"vendored-sources\"\n")

			res, err := resolveCargo(dir, DepOptions{Strategy: strategy})
			if err != nil {
				t.Fatalf("resolveCargo(%q): %v", strategy, err)
			}
			if res.Strategy != "vendored" {
				t.Errorf("Strategy = %q, want vendored", res.Strategy)
			}
			if !envHas(res.Env, "CARGO_NET_OFFLINE=true") {
				t.Errorf("vendored env missing CARGO_NET_OFFLINE=true; got %v", res.Env)
			}
			if len(res.ROMounts) != 0 {
				t.Errorf("vendored must have no mounts, got %+v", res.ROMounts)
			}
			if len(res.SetupCmds) != 0 {
				t.Errorf("vendored must have no SetupCmds, got %v", res.SetupCmds)
			}
			if res.Prefetch != nil {
				t.Error("vendored must have no prefetch hook")
			}
		})
	}
}

// TestCargoResolveOff: OFF strategy returns empty Resolution.
func TestCargoResolveOff(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname=\"x\"\n")

	res, err := resolveCargo(dir, DepOptions{Strategy: DepStrategyOff})
	if err != nil {
		t.Fatalf("resolveCargo OFF: %v", err)
	}
	if len(res.ROMounts) != 0 || len(res.Env) != 0 || len(res.SetupCmds) != 0 || res.Prefetch != nil {
		t.Errorf("Cargo OFF: want empty resolution, got %+v", res)
	}
	if res.Strategy != DepStrategyOff {
		t.Errorf("Cargo OFF: Strategy=%q want off", res.Strategy)
	}
}

// TestCargoResolveHostMount: HOST strategy mounts ONLY the registry subdir,
// never all of ~/.cargo; mount path ends with "registry", Shared=true,
// CARGO_NET_OFFLINE=true, CARGO_HOME=/cargo.
func TestCargoResolveHostMount(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname=\"x\"\n")

	// Provide a fake registry directory (must exist for checkCargoRegistryExists).
	fakeRegistry := t.TempDir()

	res, err := resolveCargo(dir, DepOptions{
		Strategy:          DepStrategyHost,
		hostCargoRegistry: fakeRegistry,
	})
	if err != nil {
		t.Fatalf("resolveCargo HOST: %v", err)
	}

	// Must have exactly one RO mount.
	if len(res.ROMounts) != 1 {
		t.Fatalf("want 1 ROMount, got %d: %+v", len(res.ROMounts), res.ROMounts)
	}
	m := res.ROMounts[0]

	// Container path must be /cargo/registry (not all of /cargo).
	if m.ContainerPath != cargoRegistryMount {
		t.Errorf("ContainerPath = %q, want %q", m.ContainerPath, cargoRegistryMount)
	}

	// Host path must end with "registry" — we never mount all of ~/.cargo.
	if !strings.HasSuffix(m.HostPath, "registry") && m.HostPath != fakeRegistry {
		t.Errorf("HostPath = %q: must be the registry subdirectory, not all of ~/.cargo", m.HostPath)
	}

	// SECURITY: must not contain "credentials" or "/bin" in any mount path.
	for _, mount := range res.ROMounts {
		if strings.Contains(mount.HostPath, "credentials") {
			t.Errorf("SECURITY: mount HostPath %q contains 'credentials' — must not mount ~/.cargo/credentials.toml", mount.HostPath)
		}
		if strings.Contains(mount.ContainerPath, "credentials") {
			t.Errorf("SECURITY: mount ContainerPath %q contains 'credentials'", mount.ContainerPath)
		}
		// Ensure we are not mounting ~/.cargo/bin
		if strings.HasSuffix(mount.HostPath, "/.cargo/bin") || strings.HasSuffix(mount.HostPath, "/.cargo") {
			t.Errorf("SECURITY: mount HostPath %q looks like all of ~/.cargo or ~/.cargo/bin; must be registry only", mount.HostPath)
		}
	}

	// Shared=true: host-owned dir must NOT receive :Z relabel.
	if !m.Shared {
		t.Errorf("host strategy ROMount.Shared = false; want true to suppress :Z relabel on shared host Cargo registry")
	}

	// Required env vars.
	for _, want := range []string{"CARGO_NET_OFFLINE=true", "CARGO_HOME=" + cargoCacheMount} {
		if !envHas(res.Env, want) {
			t.Errorf("host env missing %q; got %v", want, res.Env)
		}
	}

	if res.Prefetch != nil {
		t.Error("host strategy must not set a prefetch hook")
	}
	if res.Strategy != DepStrategyHost {
		t.Errorf("Strategy=%q want host", res.Strategy)
	}
}

// TestCargoResolveHostMissingRegistryErrors: HOST with a nonexistent registry
// path must return an error before podman gets to try the bind mount.
func TestCargoResolveHostMissingRegistryErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname=\"x\"\n")
	missing := filepath.Join(t.TempDir(), "does", "not", "exist")

	_, err := resolveCargo(dir, DepOptions{Strategy: DepStrategyHost, hostCargoRegistry: missing})
	if err == nil {
		t.Fatal("expected error for missing host Cargo registry, got nil")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error %q should mention 'does not exist'", err)
	}
}

// TestCargoResolveFetchShape: FETCH strategy returns correct mount, env,
// no SetupCmds, non-nil Prefetch hook, Shared=false, CARGO_NET_OFFLINE=true.
func TestCargoResolveFetchShape(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname=\"x\"\n")
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	res, err := resolveCargo(dir, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: mock,
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("resolveCargo FETCH: %v", err)
	}

	// Must have exactly one RO mount at /cargo/registry, bugbot-owned.
	if len(res.ROMounts) != 1 {
		t.Fatalf("want 1 ROMount, got %d: %+v", len(res.ROMounts), res.ROMounts)
	}
	m := res.ROMounts[0]
	if m.ContainerPath != cargoRegistryMount {
		t.Errorf("ContainerPath = %q, want %q", m.ContainerPath, cargoRegistryMount)
	}
	if !strings.HasPrefix(m.HostPath, cacheBase) {
		t.Errorf("cargo cache %q should live under user cache base %q", m.HostPath, cacheBase)
	}
	// Fetch cache is bugbot-owned: Shared=false (:Z isolation correct).
	if m.Shared {
		t.Error("Cargo FETCH ROMount.Shared = true; want false (bugbot-owned dir should get :Z isolation)")
	}

	for _, want := range []string{"CARGO_NET_OFFLINE=true", "CARGO_HOME=" + cargoCacheMount} {
		if !envHas(res.Env, want) {
			t.Errorf("fetch env missing %q; got %v", want, res.Env)
		}
	}
	if res.Prefetch == nil {
		t.Fatal("Cargo FETCH must set a prefetch hook")
	}
	if res.Strategy != DepStrategyFetch {
		t.Errorf("Strategy = %q, want fetch", res.Strategy)
	}
}

// TestCargoResolveFetchPrefetchSpec: Running the prefetch hook invokes the
// sandbox with a network-enabled, writable-cache `cargo fetch` spec.
func TestCargoResolveFetchPrefetchSpec(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname=\"x\"\n")
	// No Cargo.lock → fetch without --locked.
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	res, err := resolveCargo(dir, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: mock,
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("resolveCargo: %v", err)
	}
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	calls := mock.Calls()
	if len(calls) != 1 {
		t.Fatalf("prefetch should run exactly one container, got %d", len(calls))
	}
	spec := calls[0].Spec
	if spec.Network == "" || spec.Network == "none" {
		t.Errorf("prefetch network = %q, want a real network (not none/empty)", spec.Network)
	}
	// No Cargo.lock → no --locked flag.
	wantCmd := []string{"cargo", "fetch"}
	if !slices.Equal(spec.Cmd, wantCmd) {
		t.Errorf("prefetch cmd = %v, want %v", spec.Cmd, wantCmd)
	}
	// Cache mounted WRITABLE at /cargo (CARGO_HOME).
	if len(spec.RWMounts) != 1 || spec.RWMounts[0].ContainerPath != cargoCacheMount {
		t.Errorf("prefetch must bind the cache WRITABLE at %s; got %+v", cargoCacheMount, spec.RWMounts)
	}
	if len(spec.ROMounts) != 0 {
		t.Errorf("prefetch should not use read-only mounts; got %+v", spec.ROMounts)
	}
}

// TestCargoResolveFetchWithLockfile: when Cargo.lock exists, prefetch adds --locked.
func TestCargoResolveFetchWithLockfile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname=\"x\"\n")
	writeFile(t, filepath.Join(dir, "Cargo.lock"), "# lockfile\n")
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	res, err := resolveCargo(dir, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: mock,
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("resolveCargo: %v", err)
	}
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	calls := mock.Calls()
	if len(calls) != 1 {
		t.Fatalf("want 1 container call, got %d", len(calls))
	}
	wantCmd := []string{"cargo", "fetch", "--locked"}
	if !slices.Equal(calls[0].Spec.Cmd, wantCmd) {
		t.Errorf("prefetch cmd = %v, want %v", calls[0].Spec.Cmd, wantCmd)
	}
}

// TestCargoFetchSentinelKeyedOnCargoLock: sentinel is keyed on Cargo.lock hash;
// changing Cargo.lock invalidates the cache (different sentinel → re-fetch).
func TestCargoFetchSentinelKeyedOnCargoLock(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname=\"x\"\n")
	writeFile(t, filepath.Join(dir, "Cargo.lock"), "v1\n")
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	// First fetch — populates sentinel keyed on "v1" lockfile.
	res, err := resolveCargo(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("first resolveCargo: %v", err)
	}
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("first prefetch: %v", err)
	}
	if mock.CallCount() != 1 {
		t.Fatalf("first prefetch: want 1 container, got %d", mock.CallCount())
	}

	// Warm cache: same Cargo.lock → second Resolution skips the download.
	res2, err := resolveCargo(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("second resolveCargo: %v", err)
	}
	if err := res2.Prefetch(context.Background()); err != nil {
		t.Fatalf("second prefetch: %v", err)
	}
	if mock.CallCount() != 1 {
		t.Errorf("warm sentinel should skip download; container ran %d times, want 1", mock.CallCount())
	}

	// Changed Cargo.lock → sentinel miss → re-fetch.
	writeFile(t, filepath.Join(dir, "Cargo.lock"), "v2-changed\n")
	res3, err := resolveCargo(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("third resolveCargo: %v", err)
	}
	if err := res3.Prefetch(context.Background()); err != nil {
		t.Fatalf("third prefetch: %v", err)
	}
	if mock.CallCount() != 2 {
		t.Errorf("changed Cargo.lock should trigger re-fetch; container ran %d times, want 2", mock.CallCount())
	}
}

// TestCargoFetchRequiresSandbox: FETCH with nil FetchSandbox must error.
func TestCargoFetchRequiresSandbox(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname=\"x\"\n")
	_, err := resolveCargo(dir, DepOptions{Strategy: DepStrategyFetch}) // nil sandbox
	if err == nil {
		t.Fatal("Cargo FETCH with nil sandbox should error")
	}
	if !strings.Contains(err.Error(), "fetch sandbox") {
		t.Errorf("error = %v, want mention of fetch sandbox", err)
	}
}

// ---- JS/npm ecosystem unit tests --------------------------------------------

// TestJSDetectOnOff: detect fires on package.json, not without.
func TestJSDetectOnOff(t *testing.T) {
	dirNo := t.TempDir()
	if hasPackageJSON(dirNo) {
		t.Error("hasPackageJSON: want false for dir without package.json")
	}
	dirYes := t.TempDir()
	writeFile(t, filepath.Join(dirYes, "package.json"), `{"name":"x"}`+"\n")
	if !hasPackageJSON(dirYes) {
		t.Error("hasPackageJSON: want true for dir with package.json")
	}
}

// TestJSCommittedNodeModulesIsVendored: node_modules/ present → Strategy=vendored,
// no mounts, no SetupCmds, no Prefetch, in all requested strategy modes.
func TestJSCommittedNodeModulesIsVendored(t *testing.T) {
	for _, strategy := range []DepStrategy{DepStrategyOff, DepStrategyHost, DepStrategyFetch, ""} {
		t.Run(string(strategy), func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "package.json"), `{"name":"x"}`+"\n")
			if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
				t.Fatal(err)
			}

			res, err := resolveJS(dir, DepOptions{Strategy: strategy})
			if err != nil {
				t.Fatalf("resolveJS(%q): %v", strategy, err)
			}
			if res.Strategy != "vendored" {
				t.Errorf("Strategy = %q, want vendored", res.Strategy)
			}
			if len(res.ROMounts) != 0 {
				t.Errorf("committed node_modules: want no mounts, got %+v", res.ROMounts)
			}
			if len(res.SetupCmds) != 0 {
				t.Errorf("committed node_modules: want no SetupCmds, got %v", res.SetupCmds)
			}
			if res.Prefetch != nil {
				t.Error("committed node_modules: want no prefetch hook")
			}
		})
	}
}

// TestJSResolveOff: OFF strategy returns empty Resolution.
func TestJSResolveOff(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"x"}`+"\n")

	res, err := resolveJS(dir, DepOptions{Strategy: DepStrategyOff})
	if err != nil {
		t.Fatalf("resolveJS OFF: %v", err)
	}
	if len(res.ROMounts) != 0 || len(res.Env) != 0 || len(res.SetupCmds) != 0 || res.Prefetch != nil {
		t.Errorf("JS OFF: want empty resolution, got %+v", res)
	}
	if res.Strategy != DepStrategyOff {
		t.Errorf("JS OFF: Strategy=%q want off", res.Strategy)
	}
}

// TestJSResolveHostIsOff: HOST strategy for JS returns OFF (npm HTTP cache
// does not materialize node_modules; see file-level comment).
func TestJSResolveHostIsOff(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"x"}`+"\n")

	res, err := resolveJS(dir, DepOptions{Strategy: DepStrategyHost})
	if err != nil {
		t.Fatalf("resolveJS HOST: %v", err)
	}
	if len(res.ROMounts) != 0 || len(res.Env) != 0 || len(res.SetupCmds) != 0 || res.Prefetch != nil {
		t.Errorf("JS HOST must resolve to OFF (npm cache not a materializer), got %+v", res)
	}
	if res.Strategy != DepStrategyOff {
		t.Errorf("JS HOST: Strategy=%q want off", res.Strategy)
	}
}

// TestJSResolveNoLockfileIsOff: FETCH without package-lock.json resolves to OFF
// (pnpm/yarn repos fall back to off; deferred to a future bead).
func TestJSResolveNoLockfileIsOff(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"x"}`+"\n")
	// Deliberately no package-lock.json.

	res, err := resolveJS(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: NewMock(MockResponse{Result: Result{ExitCode: 0}})})
	if err != nil {
		t.Fatalf("resolveJS FETCH (no lockfile): %v", err)
	}
	if res.Strategy != DepStrategyOff {
		t.Errorf("JS FETCH without package-lock.json: Strategy=%q want off (deferred)", res.Strategy)
	}
}

// TestJSResolveFetchShape: FETCH strategy returns correct mount, env, SetupCmds,
// non-nil Prefetch hook.
func TestJSResolveFetchShape(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"x","dependencies":{"lodash":"4.17.21"}}`+"\n")
	writeFile(t, filepath.Join(dir, "package-lock.json"), `{"lockfileVersion":2}`+"\n")
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	res, err := resolveJS(dir, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: mock,
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("resolveJS FETCH: %v", err)
	}

	// Must have exactly one RO mount at /npmcache, bugbot-owned (Shared=false).
	if len(res.ROMounts) != 1 {
		t.Fatalf("want 1 RO mount, got %d: %+v", len(res.ROMounts), res.ROMounts)
	}
	m := res.ROMounts[0]
	if m.ContainerPath != npmCacheMount {
		t.Errorf("RO mount ContainerPath = %q, want %q", m.ContainerPath, npmCacheMount)
	}
	if !strings.HasPrefix(m.HostPath, cacheBase) {
		t.Errorf("npm cache %q should live under user cache base %q", m.HostPath, cacheBase)
	}
	if m.Shared {
		t.Error("JS FETCH ROMount.Shared = true; want false (bugbot-owned dir should get :Z isolation)")
	}

	// npm_config_offline=true must be in env.
	if !envHas(res.Env, "npm_config_offline=true") {
		t.Errorf("JS FETCH env missing npm_config_offline=true; got %v", res.Env)
	}

	// SetupCmds must have exactly one command that performs the offline install.
	if len(res.SetupCmds) != 1 {
		t.Fatalf("want 1 SetupCmd, got %d: %v", len(res.SetupCmds), res.SetupCmds)
	}
	setupCmd := res.SetupCmds[0]
	if len(setupCmd) == 0 {
		t.Fatal("SetupCmds[0] must not be empty")
	}
	// The setup command must perform an offline npm ci from the copied cache.
	setupStr := strings.Join(setupCmd, " ")
	if !strings.Contains(setupStr, "npm ci") {
		t.Errorf("SetupCmds[0] = %v, want an npm ci offline install", setupCmd)
	}

	if res.Prefetch == nil {
		t.Fatal("JS FETCH must set a prefetch hook")
	}
	if res.Strategy != DepStrategyFetch {
		t.Errorf("Strategy = %q, want fetch", res.Strategy)
	}
}

// TestJSResolveFetchPrefetchSpec: Running the prefetch hook invokes the sandbox
// with --ignore-scripts mandatory (SECURITY requirement).
func TestJSResolveFetchPrefetchSpec(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"x"}`+"\n")
	writeFile(t, filepath.Join(dir, "package-lock.json"), `{"lockfileVersion":2}`+"\n")
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	res, err := resolveJS(dir, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: mock,
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("resolveJS: %v", err)
	}
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	calls := mock.Calls()
	if len(calls) != 1 {
		t.Fatalf("prefetch should run exactly one container, got %d", len(calls))
	}
	spec := calls[0].Spec
	if spec.Network == "" || spec.Network == "none" {
		t.Errorf("prefetch network = %q, want a real network (not none/empty)", spec.Network)
	}
	// SECURITY: --ignore-scripts is MANDATORY in the prefetch Spec.
	if !slices.Contains(spec.Cmd, "--ignore-scripts") {
		t.Errorf("SECURITY: prefetch Cmd %v missing --ignore-scripts; npm lifecycle scripts must not run in the online (network-enabled) prefetch container", spec.Cmd)
	}
	// Cache mounted WRITABLE at /npmcache.
	if len(spec.RWMounts) != 1 || spec.RWMounts[0].ContainerPath != npmCacheMount {
		t.Errorf("prefetch must bind the cache WRITABLE at %s; got %+v", npmCacheMount, spec.RWMounts)
	}
	if len(spec.ROMounts) != 0 {
		t.Errorf("prefetch should not use read-only mounts; got %+v", spec.ROMounts)
	}

	// sync.Once: second call must not re-run the container.
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("prefetch second call: %v", err)
	}
	if mock.CallCount() != 1 {
		t.Errorf("prefetch ran %d times, want 1 (sync.Once)", mock.CallCount())
	}
}

// TestJSPrefetchSentinelKeyedOnPackageLock: sentinel is keyed on package-lock.json
// hash; warm cache is skipped, changed lockfile triggers re-fetch.
func TestJSPrefetchSentinelKeyedOnPackageLock(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"x"}`+"\n")
	writeFile(t, filepath.Join(dir, "package-lock.json"), `{"lockfileVersion":2,"v":"1"}`+"\n")
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	// First fetch.
	res, err := resolveJS(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("first resolveJS: %v", err)
	}
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("first prefetch: %v", err)
	}
	if mock.CallCount() != 1 {
		t.Fatalf("first prefetch: want 1 container, got %d", mock.CallCount())
	}

	// Warm sentinel: same package-lock.json → skip.
	res2, err := resolveJS(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("second resolveJS: %v", err)
	}
	if err := res2.Prefetch(context.Background()); err != nil {
		t.Fatalf("second prefetch: %v", err)
	}
	if mock.CallCount() != 1 {
		t.Errorf("warm sentinel should skip download; container ran %d times, want 1", mock.CallCount())
	}

	// Changed package-lock.json → re-fetch.
	writeFile(t, filepath.Join(dir, "package-lock.json"), `{"lockfileVersion":2,"v":"2-changed"}`+"\n")
	res3, err := resolveJS(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("third resolveJS: %v", err)
	}
	if err := res3.Prefetch(context.Background()); err != nil {
		t.Fatalf("third prefetch: %v", err)
	}
	if mock.CallCount() != 2 {
		t.Errorf("changed lockfile should trigger re-fetch; container ran %d times, want 2", mock.CallCount())
	}
}

// TestJSFetchRequiresSandbox: FETCH with nil FetchSandbox must error.
func TestJSFetchRequiresSandbox(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"x"}`+"\n")
	writeFile(t, filepath.Join(dir, "package-lock.json"), `{"lockfileVersion":2}`+"\n")
	_, err := resolveJS(dir, DepOptions{Strategy: DepStrategyFetch}) // nil sandbox
	if err == nil {
		t.Fatal("JS FETCH with nil sandbox should error")
	}
	if !strings.Contains(err.Error(), "fetch sandbox") {
		t.Errorf("error = %v, want mention of fetch sandbox", err)
	}
}

// ---- C#/NuGet ecosystem unit tests -----------------------------------------

// TestNuGetDetectOnOff: detect fires on any root-level C#/F# project file
// (*.csproj, *.sln, or *.fsproj), not without.
func TestNuGetDetectOnOff(t *testing.T) {
	dirNo := t.TempDir()
	if hasCSharpProject(dirNo) {
		t.Error("hasCSharpProject: want false for bare dir")
	}
	dirYes := t.TempDir()
	writeFile(t, filepath.Join(dirYes, "hello.csproj"), "<Project Sdk=\"Microsoft.NET.Sdk\"/>\n")
	if !hasCSharpProject(dirYes) {
		t.Error("hasCSharpProject: want true for dir with .csproj")
	}
	dirSln := t.TempDir()
	writeFile(t, filepath.Join(dirSln, "hello.sln"), "\n")
	if !hasCSharpProject(dirSln) {
		t.Error("hasCSharpProject: want true for dir with .sln")
	}
	dirFs := t.TempDir()
	writeFile(t, filepath.Join(dirFs, "hello.fsproj"), "<Project Sdk=\"Microsoft.NET.Sdk\"/>\n")
	if !hasCSharpProject(dirFs) {
		t.Error("hasCSharpProject: want true for dir with .fsproj")
	}
}

// TestNuGetResolveOff: OFF strategy returns empty Resolution.
func TestNuGetResolveOff(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hello.csproj"), "<Project Sdk=\"Microsoft.NET.Sdk\"/>\n")

	res, err := resolveNuGet(dir, DepOptions{Strategy: DepStrategyOff})
	if err != nil {
		t.Fatalf("resolveNuGet OFF: %v", err)
	}
	if len(res.ROMounts) != 0 || len(res.Env) != 0 || len(res.SetupCmds) != 0 || res.Prefetch != nil {
		t.Errorf("NuGet OFF: want empty resolution, got %+v", res)
	}
	if res.Strategy != DepStrategyOff {
		t.Errorf("NuGet OFF: Strategy=%q want off", res.Strategy)
	}
}

// TestNuGetResolveHostMount: HOST strategy mounts the host NuGet packages
// directory at /nugetcache, Shared=true (host-owned), NUGET_PACKAGES=/nugetcache.
func TestNuGetResolveHostMount(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hello.csproj"), "<Project Sdk=\"Microsoft.NET.Sdk\"/>\n")

	// Provide a fake packages directory (must exist for checkNuGetPackagesExists).
	fakePackages := t.TempDir()

	res, err := resolveNuGet(dir, DepOptions{
		Strategy:          DepStrategyHost,
		hostNuGetPackages: fakePackages,
	})
	if err != nil {
		t.Fatalf("resolveNuGet HOST: %v", err)
	}

	// Must have exactly one RO mount.
	if len(res.ROMounts) != 1 {
		t.Fatalf("want 1 ROMount, got %d: %+v", len(res.ROMounts), res.ROMounts)
	}
	m := res.ROMounts[0]

	if m.ContainerPath != nugetCacheMount {
		t.Errorf("ContainerPath = %q, want %q", m.ContainerPath, nugetCacheMount)
	}
	if m.HostPath != fakePackages {
		t.Errorf("HostPath = %q, want %q", m.HostPath, fakePackages)
	}

	// Shared=true: host-owned dir must NOT receive :Z relabel.
	if !m.Shared {
		t.Errorf("host strategy ROMount.Shared = false; want true to suppress :Z relabel on shared host NuGet packages dir")
	}

	// Required env var.
	if !envHas(res.Env, "NUGET_PACKAGES="+nugetCacheMount) {
		t.Errorf("host env missing %q; got %v", "NUGET_PACKAGES="+nugetCacheMount, res.Env)
	}

	if res.Prefetch != nil {
		t.Error("host strategy must not set a prefetch hook")
	}
	if res.Strategy != DepStrategyHost {
		t.Errorf("Strategy=%q want host", res.Strategy)
	}
}

// TestNuGetResolveHostMissingPackagesErrors: HOST with a nonexistent packages
// path must return an error before podman gets to try the bind mount.
func TestNuGetResolveHostMissingPackagesErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hello.csproj"), "<Project Sdk=\"Microsoft.NET.Sdk\"/>\n")
	missing := filepath.Join(t.TempDir(), "does", "not", "exist")

	_, err := resolveNuGet(dir, DepOptions{Strategy: DepStrategyHost, hostNuGetPackages: missing})
	if err == nil {
		t.Fatal("expected error for missing host NuGet packages dir, got nil")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error %q should mention 'does not exist'", err)
	}
}

// TestNuGetResolveFetchShape: FETCH strategy returns correct mount, env,
// no SetupCmds, non-nil Prefetch hook, Shared=false.
func TestNuGetResolveFetchShape(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hello.csproj"), "<Project Sdk=\"Microsoft.NET.Sdk\"/>\n")
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	res, err := resolveNuGet(dir, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: mock,
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("resolveNuGet FETCH: %v", err)
	}

	// Must have exactly one RO mount at /nugetcache, bugbot-owned.
	if len(res.ROMounts) != 1 {
		t.Fatalf("want 1 ROMount, got %d: %+v", len(res.ROMounts), res.ROMounts)
	}
	m := res.ROMounts[0]
	if m.ContainerPath != nugetCacheMount {
		t.Errorf("ContainerPath = %q, want %q", m.ContainerPath, nugetCacheMount)
	}
	if !strings.HasPrefix(m.HostPath, cacheBase) {
		t.Errorf("nuget cache %q should live under user cache base %q", m.HostPath, cacheBase)
	}
	// Fetch cache is bugbot-owned: Shared=false (:Z isolation correct).
	if m.Shared {
		t.Error("NuGet FETCH ROMount.Shared = true; want false (bugbot-owned dir should get :Z isolation)")
	}

	if !envHas(res.Env, "NUGET_PACKAGES="+nugetCacheMount) {
		t.Errorf("fetch env missing %q; got %v", "NUGET_PACKAGES="+nugetCacheMount, res.Env)
	}
	if len(res.SetupCmds) != 0 {
		t.Errorf("NuGet FETCH must have no SetupCmds, got %v", res.SetupCmds)
	}
	if res.Prefetch == nil {
		t.Fatal("NuGet FETCH must set a prefetch hook")
	}
	if res.Strategy != DepStrategyFetch {
		t.Errorf("Strategy = %q, want fetch", res.Strategy)
	}
}

// TestNuGetResolveFetchPrefetchSpec: Running the prefetch hook invokes the
// sandbox with a network-enabled, writable-cache `dotnet restore` spec.
func TestNuGetResolveFetchPrefetchSpec(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hello.csproj"), "<Project Sdk=\"Microsoft.NET.Sdk\"/>\n")
	// No packages.lock.json → restore without --locked-mode.
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	res, err := resolveNuGet(dir, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: mock,
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("resolveNuGet: %v", err)
	}
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	calls := mock.Calls()
	if len(calls) != 1 {
		t.Fatalf("prefetch should run exactly one container, got %d", len(calls))
	}
	spec := calls[0].Spec
	if spec.Network == "" || spec.Network == "none" {
		t.Errorf("prefetch network = %q, want a real network (not none/empty)", spec.Network)
	}
	// No packages.lock.json → no --locked-mode flag.
	wantCmd := []string{"dotnet", "restore"}
	if !slices.Equal(spec.Cmd, wantCmd) {
		t.Errorf("prefetch cmd = %v, want %v", spec.Cmd, wantCmd)
	}
	// Cache mounted WRITABLE at /nugetcache.
	if len(spec.RWMounts) != 1 || spec.RWMounts[0].ContainerPath != nugetCacheMount {
		t.Errorf("prefetch must bind the cache WRITABLE at %s; got %+v", nugetCacheMount, spec.RWMounts)
	}
	if len(spec.ROMounts) != 0 {
		t.Errorf("prefetch should not use read-only mounts; got %+v", spec.ROMounts)
	}
	// Env must point NUGET_PACKAGES at the cache mount.
	if !envHas(spec.Env, "NUGET_PACKAGES="+nugetCacheMount) {
		t.Errorf("prefetch env missing NUGET_PACKAGES=%s; got %v", nugetCacheMount, spec.Env)
	}
}

// TestNuGetResolveFetchWithLockfile: when packages.lock.json exists, prefetch
// adds --locked-mode (mirrors Cargo's --locked behavior).
func TestNuGetResolveFetchWithLockfile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hello.csproj"), "<Project Sdk=\"Microsoft.NET.Sdk\"/>\n")
	writeFile(t, filepath.Join(dir, "packages.lock.json"), "{}\n")
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	res, err := resolveNuGet(dir, DepOptions{
		Strategy:     DepStrategyFetch,
		FetchSandbox: mock,
		userCacheDir: cacheBase,
	})
	if err != nil {
		t.Fatalf("resolveNuGet: %v", err)
	}
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	calls := mock.Calls()
	if len(calls) != 1 {
		t.Fatalf("want 1 container call, got %d", len(calls))
	}
	wantCmd := []string{"dotnet", "restore", "--locked-mode"}
	if !slices.Equal(calls[0].Spec.Cmd, wantCmd) {
		t.Errorf("prefetch cmd = %v, want %v", calls[0].Spec.Cmd, wantCmd)
	}
}

// TestNuGetFetchSentinelKeyedOnLock: sentinel is keyed on packages.lock.json
// hash; changing packages.lock.json invalidates the cache (different sentinel
// → re-fetch). The no-lockfile *.csproj/*.fsproj fallback key is covered
// separately by TestNuGetFetchSentinelKeyedOnProjectFiles.
func TestNuGetFetchSentinelKeyedOnLock(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hello.csproj"), "<Project Sdk=\"Microsoft.NET.Sdk\"/>\n")
	writeFile(t, filepath.Join(dir, "packages.lock.json"), "v1\n")
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	// First fetch — populates sentinel keyed on "v1" lockfile.
	res, err := resolveNuGet(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("first resolveNuGet: %v", err)
	}
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("first prefetch: %v", err)
	}
	if mock.CallCount() != 1 {
		t.Fatalf("first prefetch: want 1 container, got %d", mock.CallCount())
	}

	// Warm cache: same packages.lock.json → second Resolution skips the download.
	res2, err := resolveNuGet(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("second resolveNuGet: %v", err)
	}
	if err := res2.Prefetch(context.Background()); err != nil {
		t.Fatalf("second prefetch: %v", err)
	}
	if mock.CallCount() != 1 {
		t.Errorf("warm sentinel should skip download; container ran %d times, want 1", mock.CallCount())
	}

	// Changed packages.lock.json → sentinel miss → re-fetch.
	writeFile(t, filepath.Join(dir, "packages.lock.json"), "v2-changed\n")
	res3, err := resolveNuGet(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("third resolveNuGet: %v", err)
	}
	if err := res3.Prefetch(context.Background()); err != nil {
		t.Fatalf("third prefetch: %v", err)
	}
	if mock.CallCount() != 2 {
		t.Errorf("changed packages.lock.json should trigger re-fetch; container ran %d times, want 2", mock.CallCount())
	}
}

// TestNuGetFetchSentinelKeyedOnProjectFiles pins the FALLBACK cache key used
// when there is NO packages.lock.json: the sentinel is keyed on the sha256 of
// the root *.csproj/*.fsproj contents (sort+concat+sha256), so an unchanged
// project set warm-skips the download and an edited project file re-fetches.
// Uses a root *.fsproj (no *.csproj, no lockfile) to also pin that *.fsproj —
// which the detector accepts — contributes to the key; before that fix an
// .fsproj-only repo produced no key and re-fetched every cycle.
func TestNuGetFetchSentinelKeyedOnProjectFiles(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "app.fsproj")
	writeFile(t, proj, "<Project Sdk=\"Microsoft.NET.Sdk\"><ItemGroup><PackageReference Include=\"Foo\" Version=\"1.0.0\"/></ItemGroup></Project>\n")
	cacheBase := t.TempDir()
	mock := NewMock(MockResponse{Result: Result{ExitCode: 0}})

	// First fetch — populates the sentinel keyed on the .fsproj hash.
	res, err := resolveNuGet(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("first resolveNuGet: %v", err)
	}
	if err := res.Prefetch(context.Background()); err != nil {
		t.Fatalf("first prefetch: %v", err)
	}
	if mock.CallCount() != 1 {
		t.Fatalf("first prefetch: want 1 container, got %d", mock.CallCount())
	}

	// Warm cache: unchanged .fsproj → second Resolution skips the download.
	res2, err := resolveNuGet(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("second resolveNuGet: %v", err)
	}
	if err := res2.Prefetch(context.Background()); err != nil {
		t.Fatalf("second prefetch: %v", err)
	}
	if mock.CallCount() != 1 {
		t.Errorf("warm sentinel (fsproj fallback) should skip download; container ran %d times, want 1", mock.CallCount())
	}

	// Edited .fsproj → fallback hash changes → sentinel miss → re-fetch.
	writeFile(t, proj, "<Project Sdk=\"Microsoft.NET.Sdk\"><ItemGroup><PackageReference Include=\"Foo\" Version=\"2.0.0\"/></ItemGroup></Project>\n")
	res3, err := resolveNuGet(dir, DepOptions{Strategy: DepStrategyFetch, FetchSandbox: mock, userCacheDir: cacheBase})
	if err != nil {
		t.Fatalf("third resolveNuGet: %v", err)
	}
	if err := res3.Prefetch(context.Background()); err != nil {
		t.Fatalf("third prefetch: %v", err)
	}
	if mock.CallCount() != 2 {
		t.Errorf("edited .fsproj should trigger re-fetch; container ran %d times, want 2", mock.CallCount())
	}
}

// TestNuGetFetchRequiresSandbox: FETCH with nil FetchSandbox must error.
func TestNuGetFetchRequiresSandbox(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hello.csproj"), "<Project Sdk=\"Microsoft.NET.Sdk\"/>\n")
	_, err := resolveNuGet(dir, DepOptions{Strategy: DepStrategyFetch}) // nil sandbox
	if err == nil {
		t.Fatal("NuGet FETCH with nil sandbox should error")
	}
	if !strings.Contains(err.Error(), "fetch sandbox") {
		t.Errorf("error = %v, want mention of fetch sandbox", err)
	}
}

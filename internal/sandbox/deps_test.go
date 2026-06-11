package sandbox

import (
	"context"
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
	if len(res.ROMounts) != 0 || len(res.Env) != 0 || res.Prefetch != nil {
		t.Errorf("off on non-vendored Go repo: want empty resolution, got %+v", res)
	}
	if res.Strategy != DepStrategyOff {
		t.Errorf("Strategy=%q want off", res.Strategy)
	}
}

func TestResolveDepsHostMountsCache(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	cache := t.TempDir()

	res, err := ResolveDeps(dir, DepOptions{Strategy: DepStrategyHost, hostModcache: cache})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	if len(res.ROMounts) != 1 {
		t.Fatalf("want 1 ROMount, got %d", len(res.ROMounts))
	}
	if res.ROMounts[0].HostPath != cache || res.ROMounts[0].ContainerPath != modcacheMount {
		t.Errorf("mount = %+v, want host=%q ctr=%q", res.ROMounts[0], cache, modcacheMount)
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

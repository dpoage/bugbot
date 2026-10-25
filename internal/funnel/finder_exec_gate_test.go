package funnel

// finder_exec_gate_test.go covers the P1 decision table (funnel half: every
// status string), the P3 deps-resolution legs, and the A2 prefetch leg.
import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/llmkit/sandbox"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/sandboxenv"
	"github.com/dpoage/bugbot/internal/store"
)

const gateTestTimeout = 120 * time.Second

func newGateFunnel(t *testing.T, opts Options) *Funnel {
	t.Helper()
	st, repo := openFixture(t)
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, opts)
	if err != nil {
		t.Fatalf("funnel.New: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func gateProber(caps sandbox.CapabilitySet, err error, seen *[]sandbox.Spec) func(context.Context, sandbox.Spec) (sandbox.CapabilitySet, error) {
	return func(_ context.Context, base sandbox.Spec) (sandbox.CapabilitySet, error) {
		if seen != nil {
			*seen = append(*seen, base)
		}
		return caps, err
	}
}

// TestFinderExecGateDecisionTable asserts the full status line and the
// enabled flag for every row of the P1 decision table the funnel resolves.
// The OffReason values are exactly what engine provisionFinderExecForScan
// produces (pinned by TestProvisionFinderExecForScan in internal/engine).
func TestFinderExecGateDecisionTable(t *testing.T) {
	goCaps := sandbox.CapabilitySet{"go": {"present": true}}
	emptyCaps := sandbox.CapabilitySet{}
	probeErr := errors.New("probe unreachable")
	mock := func() sandbox.Sandbox { return sandbox.NewMock(sandbox.MockResponse{}) }

	cases := []struct {
		name    string
		opts    Options
		langs   []ingest.Language
		want    string
		wantOn  bool
		wantNet sandbox.NetworkMode
	}{
		{
			name: "mode off",
			opts: Options{FinderExec: FinderExecOpts{OffReason: "scan.finder_exec=off"}},
			want: "Finder exec: off (scan.finder_exec=off)",
		},
		{
			name: "auto no backend available",
			opts: Options{FinderExec: FinderExecOpts{OffReason: "no sandbox backend available: podman/docker not on PATH"}},
			want: "Finder exec: off (no sandbox backend available: podman/docker not on PATH)",
		},
		{
			name: "auto unsandboxed never",
			opts: Options{FinderExec: FinderExecOpts{OffReason: "auto never runs unsandboxed; set scan.finder_exec: on"}},
			want: "Finder exec: off (auto never runs unsandboxed; set scan.finder_exec: on)",
		},
		{
			name:  "auto no target language",
			opts:  Options{FinderExec: FinderExecOpts{Sandbox: mock(), Probe: gateProber(goCaps, nil, nil)}},
			langs: nil,
			want:  "Finder exec: off (no target language in scan scope)",
		},
		{
			name:  "auto no toolchain probe for language",
			opts:  Options{FinderExec: FinderExecOpts{Sandbox: mock(), Probe: gateProber(goCaps, nil, nil)}},
			langs: []ingest.Language{"java"},
			want:  "Finder exec: off (no toolchain probe for java)",
		},
		{
			name:  "auto probe error",
			opts:  Options{FinderExec: FinderExecOpts{Sandbox: mock(), Probe: gateProber(nil, probeErr, nil)}},
			langs: []ingest.Language{ingest.LangGo},
			want:  "Finder exec: off (capability probe failed: probe unreachable)",
		},
		{
			name:  "auto probe says image lacks go",
			opts:  Options{FinderExec: FinderExecOpts{Sandbox: mock(), BackendLabel: "podman", Image: "docker.io/library/debian:stable-slim", Probe: gateProber(emptyCaps, nil, nil)}},
			langs: []ingest.Language{ingest.LangGo},
			want:  "Finder exec: off (sandbox image docker.io/library/debian:stable-slim lacks go)",
		},
		{
			name:  "auto probe says host lacks python under bwrap",
			opts:  Options{FinderExec: FinderExecOpts{Sandbox: mock(), BackendLabel: "bwrap", Probe: gateProber(emptyCaps, nil, nil)}},
			langs: []ingest.Language{ingest.LangPython},
			want:  "Finder exec: off (host lacks python under bwrap)",
		},
		{
			name:   "auto probe pass",
			opts:   Options{FinderExec: FinderExecOpts{Sandbox: mock(), BackendLabel: "podman", Network: sandbox.NetworkNone, MaxExecs: 8, Timeout: gateTestTimeout, OutputCap: 16384, Probe: gateProber(goCaps, nil, nil)}},
			langs:  []ingest.Language{ingest.LangGo},
			want:   "Finder exec: on (sandbox=podman, language=go, network=none, dep_strategy=off, max_execs=8 per finder)",
			wantOn: true, wantNet: sandbox.NetworkNone,
		},
		{
			name:   "mode on skips probe and omits language",
			opts:   Options{FinderExec: FinderExecOpts{Sandbox: mock(), BackendLabel: "podman", Network: sandbox.NetworkNone, SkipProbe: true, MaxExecs: 8, Timeout: gateTestTimeout, OutputCap: 16384}},
			langs:  []ingest.Language{ingest.LangGo},
			want:   "Finder exec: on (sandbox=podman, network=none, dep_strategy=off, max_execs=8 per finder)",
			wantOn: true, wantNet: sandbox.NetworkNone,
		},
		{
			name:   "unsandboxed on",
			opts:   Options{FinderExec: FinderExecOpts{Sandbox: sandbox.NewHostExec(), Unsandboxed: true, Network: sandbox.NetworkHost, SkipProbe: true, MaxExecs: 8, Timeout: gateTestTimeout, OutputCap: 16384}},
			langs:  []ingest.Language{ingest.LangGo},
			want:   "Finder exec: on (sandbox=UNSANDBOXED host, network=host, max_execs=8 per finder)",
			wantOn: true, wantNet: sandbox.NetworkHost,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateFunnel(t, tc.opts)
			result := &Result{}
			f.resolveFinderExec(context.Background(), result, tc.langs)
			if result.FinderExecStatus != tc.want {
				t.Errorf("status = %q, want %q", result.FinderExecStatus, tc.want)
			}
			if f.finderExec.enabled() != tc.wantOn {
				t.Errorf("enabled = %v, want %v", f.finderExec.enabled(), tc.wantOn)
			}
			if tc.wantOn && f.finderExec.Network != tc.wantNet {
				t.Errorf("state.Network = %q, want %q", f.finderExec.Network, tc.wantNet)
			}
		})
	}
}

// TestFinderExecGateNotProvisioned: a zero FinderExecOpts (review/sweep/
// daemon-cycle funnels — ruling E5) emits no status and leaves the state off.
func TestFinderExecGateNotProvisioned(t *testing.T) {
	f := newGateFunnel(t, Options{})
	result := &Result{}
	f.resolveFinderExec(context.Background(), result, []ingest.Language{ingest.LangGo})
	if result.FinderExecStatus != "" {
		t.Errorf("status = %q, want empty", result.FinderExecStatus)
	}
	if f.finderExec.enabled() {
		t.Error("finderExec enabled on an unprovisioned funnel")
	}
}

// TestFinderExecProbeBaseSpecCarriesDeps (A1): the auto probe's base Spec is
// the same RepoDir/ROMounts the finder execs use (f.deps).
func TestFinderExecProbeBaseSpecCarriesDeps(t *testing.T) {
	mount := sandbox.ROMount{HostPath: t.TempDir(), ContainerPath: "/sibling", Shared: true}
	var seen []sandbox.Spec
	probe := gateProber(sandbox.CapabilitySet{"go": {"present": true}}, nil, &seen)
	opts := Options{FinderExec: FinderExecOpts{
		Sandbox: sandbox.NewMock(sandbox.MockResponse{}), BackendLabel: "podman",
		Network: sandbox.NetworkNone, MaxExecs: 8, Timeout: gateTestTimeout, OutputCap: 16384,
		LocalMounts: []sandbox.ROMount{mount}, Probe: probe,
	}}
	f := newGateFunnel(t, opts)
	result := &Result{}
	f.resolveFinderExec(context.Background(), result, []ingest.Language{ingest.LangGo})
	if len(seen) != 1 {
		t.Fatalf("probe calls = %d, want 1", len(seen))
	}
	base := seen[0]
	if base.RepoDir != f.repo.Root() {
		t.Errorf("base.RepoDir = %q, want repo root %q", base.RepoDir, f.repo.Root())
	}
	if len(base.ROMounts) != 1 || base.ROMounts[0] != mount {
		t.Errorf("base.ROMounts = %+v, want the f.deps mount %+v", base.ROMounts, mount)
	}
}

// TestFinderExecDepsResolvedWithoutVerify (P3): with verify.sandbox_exec off,
// the finder-exec Resolution equals ResolveDeps(repo, DepOptions{Strategy,
// FetchSandbox: sb, LocalMounts}) with setup_cmds prepended.
func TestFinderExecDepsResolvedWithoutVerify(t *testing.T) {
	sb := sandbox.NewMock(sandbox.MockResponse{})
	mount := sandbox.ROMount{HostPath: t.TempDir(), ContainerPath: "/sibling", Shared: true}
	setup := [][]string{{"apt-get", "install", "-y", "libpq-dev"}}
	opts := Options{FinderExec: FinderExecOpts{
		Sandbox: sb, DepStrategy: sandboxenv.DepStrategyHost,
		SetupCmds: setup, LocalMounts: []sandbox.ROMount{mount},
	}}
	f := newGateFunnel(t, opts)

	_, repo2 := openFixture(t)
	want, err := sandboxenv.ResolveDeps(repo2.Root(), sandboxenv.DepOptions{
		Strategy: sandboxenv.DepStrategyHost, FetchSandbox: sb, LocalMounts: []sandbox.ROMount{mount},
	})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	want.SetupCmds = append(append([][]string{}, setup...), want.SetupCmds...)
	if !resolutionsEqual(f.deps, want) {
		t.Errorf("f.deps = %+v, want %+v", f.deps, want)
	}
}

// TestFinderExecDepsUnsandboxedEmpty (P3/A3): under unsandboxed the
// Resolution carries no ROMounts/SetupCmds/Env.
func TestFinderExecDepsUnsandboxedEmpty(t *testing.T) {
	opts := Options{FinderExec: FinderExecOpts{
		Sandbox: sandbox.NewHostExec(), Unsandboxed: true, Network: sandbox.NetworkHost, SkipProbe: true,
		MaxExecs: 8, Timeout: gateTestTimeout, OutputCap: 16384,
	}}
	f := newGateFunnel(t, opts)
	if len(f.deps.ROMounts) != 0 || len(f.deps.SetupCmds) != 0 || len(f.deps.Env) != 0 {
		t.Errorf("unsandboxed deps = %+v, want no mounts/setup/env", f.deps)
	}
	if f.deps.Strategy != sandboxenv.DepStrategyOff {
		t.Errorf("unsandboxed deps.Strategy = %q, want off", f.deps.Strategy)
	}
}

// TestFinderExecPrefetchFailureDisables (A2): a fetch-strategy prefetch
// failure turns the decision off with the reason, after exactly one Exec.
func TestFinderExecPrefetchFailureDisables(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir := t.TempDir()
	writeGateFile(t, filepath.Join(dir, "go.mod"), "module example.com/x\n\ngo 1.22\n")
	runGateGit(t, dir, "init", "-q")
	runGateGit(t, dir, "add", ".")
	runGateGit(t, dir, "commit", "-q", "-m", "seed")

	sb := sandbox.NewMock(sandbox.MockResponse{Err: errors.New("network down")})
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo, err := ingest.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{
		FinderExec: FinderExecOpts{
			Sandbox: sb, BackendLabel: "podman", Network: sandbox.NetworkNone,
			SkipProbe: true, MaxExecs: 8, Timeout: gateTestTimeout, OutputCap: 16384,
			DepStrategy: sandboxenv.DepStrategyFetch,
		},
	})
	if err != nil {
		t.Fatalf("funnel.New: %v", err)
	}
	if f.deps.Prefetch == nil {
		t.Fatal("fetch strategy did not build a Prefetch closure")
	}
	result := &Result{}
	f.resolveFinderExec(ctx, result, []ingest.Language{ingest.LangGo})
	want := "Finder exec: off (dependency prefetch failed: "
	if !strings.HasPrefix(result.FinderExecStatus, want) {
		t.Errorf("status = %q, want prefix %q", result.FinderExecStatus, want)
	}
	if f.finderExec.enabled() {
		t.Error("finderExec enabled after prefetch failure")
	}
	if got := sb.CallCount(); got != 1 {
		t.Errorf("prefetch Exec count = %d, want 1", got)
	}
}

// TestSweepSetsFinderExecStatus proves the run_pipeline wiring: a provisioned
// full sweep records the status line on its Result.
func TestSweepSetsFinderExecStatus(t *testing.T) {
	f := newGateFunnel(t, Options{FinderExec: FinderExecOpts{OffReason: "scan.finder_exec=off"}})
	res, err := f.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.FinderExecStatus != "Finder exec: off (scan.finder_exec=off)" {
		t.Errorf("FinderExecStatus = %q, want the off line", res.FinderExecStatus)
	}
}

func resolutionsEqual(a, b sandboxenv.Resolution) bool {
	if a.Strategy != b.Strategy || len(a.Warnings) != len(b.Warnings) {
		return false
	}
	if len(a.ROMounts) != len(b.ROMounts) || len(a.Env) != len(b.Env) || len(a.SetupCmds) != len(b.SetupCmds) {
		return false
	}
	for i := range a.ROMounts {
		if a.ROMounts[i] != b.ROMounts[i] {
			return false
		}
	}
	for i := range a.Env {
		if a.Env[i] != b.Env[i] {
			return false
		}
	}
	for i := range a.SetupCmds {
		if strings.Join(a.SetupCmds[i], "\x00") != strings.Join(b.SetupCmds[i], "\x00") {
			return false
		}
	}
	return (a.Prefetch == nil) == (b.Prefetch == nil)
}

func writeGateFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGateGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

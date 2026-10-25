package engine

// finder_exec_test.go covers the P1 engine rows (mode x backend), the P2
// unsandboxed legs, the P3 instance-sharing leg, and the P4 banner leg.
import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/llmkit/sandbox"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/funnel"
)

func gateCfg(mode, backend string) config.Config {
	cfg := config.Default()
	cfg.Scan.FinderExec = mode
	cfg.Sandbox.Backend = backend
	return cfg
}

// TestDecideFinderExecBackend pins the snapshot-independent decision rows,
// including auto+unsandboxed (the P1 mutant: treating unsandboxed as a real
// backend in auto mode).
func TestDecideFinderExecBackend(t *testing.T) {
	cases := []struct {
		name        string
		mode        string
		backend     string
		available   bool
		verifyBuilt bool
		want        finderExecClass
	}{
		{"off any backend", "off", "", true, false, finderExecOffMode},
		{"off unsandboxed", "off", "unsandboxed", false, false, finderExecOffMode},
		{"auto empty backend available", "auto", "", true, false, finderExecReal},
		{"auto empty backend unavailable", "auto", "", false, false, finderExecOffNoBackend},
		{"auto podman unavailable", "auto", "podman", false, false, finderExecOffNoBackend},
		{"auto bwrap unavailable", "auto", "bwrap", false, false, finderExecOffNoBackend},
		{"auto bwrap available", "auto", "bwrap", true, false, finderExecReal},
		{"auto unsandboxed always off", "auto", "unsandboxed", true, false, finderExecOffUnsandboxedAuto},
		{"auto unsandboxed even with verify", "auto", "unsandboxed", true, true, finderExecOffUnsandboxedAuto},
		{"auto empty mode", "", "unsandboxed", true, false, finderExecOffUnsandboxedAuto},
		{"auto empty mode real", "", "", true, false, finderExecReal},
		{"on empty backend available", "on", "", true, false, finderExecReal},
		{"on unavailable", "on", "podman", false, false, finderExecOffNoBackend},
		{"on unsandboxed", "on", "unsandboxed", false, false, finderExecHost},
		{"on verify already built", "on", "", false, true, finderExecReal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideFinderExecBackend(tc.mode, tc.backend, tc.available, tc.verifyBuilt)
			if got != tc.want {
				t.Errorf("decide(%q,%q,available=%v,verifyBuilt=%v) = %v, want %v",
					tc.mode, tc.backend, tc.available, tc.verifyBuilt, got, tc.want)
			}
		})
	}
}

func gateVerifyOpts(sb sandbox.Sandbox) funnel.SandboxOpts {
	return funnel.SandboxOpts{Enabled: true, Sandbox: sb, MaxExecs: 3}
}

// TestProvisionFinderExecForScanOffRows pins the engine-side OffReason
// strings (the funnel test joins them into the full status lines).
func TestProvisionFinderExecForScanOffRows(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"mode off", gateCfg("off", ""), "scan.finder_exec=off"},
		{"mode off unsandboxed", gateCfg("off", "unsandboxed"), "scan.finder_exec=off"},
		{"auto unsandboxed", gateCfg("auto", "unsandboxed"), "auto never runs unsandboxed; set scan.finder_exec: on"},
		{"on unsandboxed is not off", gateCfg("on", "unsandboxed"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errOut bytes.Buffer
			opts, err := provisionFinderExecForScan(tc.cfg, funnel.SandboxOpts{}, &errOut)
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			if opts.OffReason != tc.want {
				t.Errorf("OffReason = %q, want %q", opts.OffReason, tc.want)
			}
		})
	}
}

// TestProvisionFinderExecForScanNoBackend: an unavailable real backend turns
// auto/on off with the unavailable detail in the reason.
func TestProvisionFinderExecForScanNoBackend(t *testing.T) {
	cfg := gateCfg("auto", "podman")
	cfg.Sandbox.Runtime = "definitely-not-a-runtime-ypei"
	var errOut bytes.Buffer
	opts, err := provisionFinderExecForScan(cfg, funnel.SandboxOpts{}, &errOut)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	want := "no sandbox backend available: definitely-not-a-runtime-ypei not on PATH"
	if opts.OffReason != want {
		t.Errorf("OffReason = %q, want %q", opts.OffReason, want)
	}
	if opts.Sandbox != nil {
		t.Error("Sandbox set on an off decision")
	}
}

// TestProvisionUnsandboxedOn (P2/P4): mode on + unsandboxed provisions
// HostExec with host network, no deps inputs, and writes the banner.
func TestProvisionUnsandboxedOn(t *testing.T) {
	cfg := gateCfg("on", "unsandboxed")
	cfg.Sandbox.DepStrategy = "host"
	cfg.Sandbox.SetupCmds = [][]string{{"true"}}
	cfg.Sandbox.LocalMounts = []config.LocalMount{{Host: t.TempDir(), Container: "/sibling"}}
	var errOut bytes.Buffer
	opts, err := provisionFinderExecForScan(cfg, funnel.SandboxOpts{}, &errOut)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, ok := opts.Sandbox.(*sandbox.HostExec); !ok {
		t.Fatalf("Sandbox = %T, want *sandbox.HostExec", opts.Sandbox)
	}
	if !opts.Unsandboxed || !opts.SkipProbe {
		t.Error("Unsandboxed/SkipProbe not set")
	}
	if opts.Network != sandbox.NetworkHost {
		t.Errorf("Network = %q, want host", opts.Network)
	}
	if len(opts.SetupCmds) != 0 || len(opts.LocalMounts) != 0 || opts.DepStrategy != "" {
		t.Errorf("unsandboxed deps inputs = %+v, want none (A3)", opts)
	}
	wantBanner := "!!! UNSANDBOXED: finder probes run directly on this host (workspace copy, no container isolation, network=host, your OS user's privileges). Not applied on the host: sandbox.image, cpus, memory_mb, pids_limit, idle_timeout_seconds, scratch_size_mb, workspace_growth_ceiling_mb, dep_strategy, setup_cmds, local_mounts, host_toolchains. !!!\n"
	if errOut.String() != wantBanner {
		t.Errorf("banner = %q, want %q", errOut.String(), wantBanner)
	}
	if opts.OffReason != "" {
		t.Errorf("OffReason = %q, want empty", opts.OffReason)
	}
}

// TestProvisionBannerOnlyWhenUnsandboxedActive (P4): the banner appears iff
// the unsandboxed backend is actually provisioned.
func TestProvisionBannerOnlyWhenUnsandboxedActive(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cfg        config.Config
		wantBanner bool
	}{
		{"auto unsandboxed stays off", gateCfg("auto", "unsandboxed"), false},
		{"mode off unsandboxed", gateCfg("off", "unsandboxed"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var errOut bytes.Buffer
			_, err := provisionFinderExecForScan(tc.cfg, funnel.SandboxOpts{}, &errOut)
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			if (errOut.Len() > 0) != tc.wantBanner {
				t.Errorf("banner present = %v, want %v", errOut.Len() > 0, tc.wantBanner)
			}
		})
	}
}

// TestProvisionReusesVerifySandbox (P3): with verify.sandbox_exec on, the
// SAME instance serves both features, network is pinned none, and the deps
// inputs come from cfg.Sandbox.
func TestProvisionReusesVerifySandbox(t *testing.T) {
	cfg := gateCfg("auto", "")
	cfg.Sandbox.DepStrategy = "host"
	cfg.Sandbox.SetupCmds = [][]string{{"apt-get", "update"}}
	verifySB := gateVerifyOpts(sandbox.NewMock(sandbox.MockResponse{}))
	var errOut bytes.Buffer
	opts, err := provisionFinderExecForScan(cfg, verifySB, &errOut)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if opts.Sandbox != verifySB.Sandbox {
		t.Error("finder exec did not reuse the verify sandbox instance")
	}
	if opts.Network != sandbox.NetworkNone {
		t.Errorf("Network = %q, want none", opts.Network)
	}
	if opts.SkipProbe {
		t.Error("auto mode must run the language probe")
	}
	if string(opts.DepStrategy) != "host" || len(opts.SetupCmds) != 1 {
		t.Errorf("deps inputs = %+v, want from cfg.Sandbox", opts)
	}
	if len(opts.LocalMounts) != 0 {
		t.Errorf("LocalMounts = %+v, want none for a config without local_mounts", opts.LocalMounts)
	}
	if opts.Probe == nil {
		t.Error("Probe closure not set")
	}
	if errOut.Len() != 0 {
		t.Errorf("banner written for a real backend: %q", errOut.String())
	}

	// mode on over the same shared instance skips the probe.
	optsOn, err := provisionFinderExecForScan(gateCfg("on", ""), verifySB, &errOut)
	if err != nil {
		t.Fatalf("provision on: %v", err)
	}
	if !optsOn.SkipProbe || optsOn.Sandbox != verifySB.Sandbox {
		t.Errorf("mode on = %+v, want shared instance with SkipProbe", optsOn)
	}
}

// TestProvisionUnsandboxedAutoGivesNoSandbox (P1 mutant leg): auto +
// unsandboxed must NOT provision a sandbox even when verify built one.
func TestProvisionUnsandboxedAutoGivesNoSandbox(t *testing.T) {
	verifySB := gateVerifyOpts(sandbox.NewMock(sandbox.MockResponse{}))
	var errOut bytes.Buffer
	opts, err := provisionFinderExecForScan(gateCfg("auto", "unsandboxed"), verifySB, &errOut)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if opts.Sandbox != nil {
		t.Errorf("Sandbox = %T, want nil under auto+unsandboxed", opts.Sandbox)
	}
	if opts.OffReason != "auto never runs unsandboxed; set scan.finder_exec: on" {
		t.Errorf("OffReason = %q", opts.OffReason)
	}
}

// TestSandboxDegradedWarningUnsandboxedCause (P2): under unsandboxed the
// degraded warning names the mode, not a missing container runtime.
func TestSandboxDegradedWarningUnsandboxedCause(t *testing.T) {
	cfg := gateCfg("auto", "unsandboxed")
	cfg.Verify.SandboxExec = true
	opts, degraded, err := buildSandboxOpts(cfg)
	if err != nil {
		t.Fatalf("buildSandboxOpts: %v", err)
	}
	if !degraded {
		t.Error("degraded = false, want true (Available is false under unsandboxed)")
	}
	if opts.Enabled {
		t.Error("verify sandbox_exec enabled under unsandboxed")
	}
	var buf bytes.Buffer
	printSandboxDegradedWarningFor(&buf, cfg)
	if !strings.Contains(buf.String(), "unsandboxed") {
		t.Errorf("warning = %q, want it to name unsandboxed", buf.String())
	}

	// Classic configs keep the byte-identical historical text.
	var classic bytes.Buffer
	printSandboxDegradedWarningFor(&classic, config.Default())
	if classic.String() != "Warning: "+SandboxDegradedWarning+"\n" {
		t.Errorf("classic warning = %q, want the historical text", classic.String())
	}
}

// TestProvisionCarriesLimits: the per-scan limits come from the config.
func TestProvisionCarriesLimits(t *testing.T) {
	cfg := gateCfg("on", "unsandboxed")
	cfg.Scan.FinderExecMaxExecs = 5
	cfg.Scan.FinderExecTimeoutSeconds = 45
	cfg.Scan.FinderExecOutputBytes = 4096
	opts, err := provisionFinderExecForScan(cfg, funnel.SandboxOpts{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if opts.MaxExecs != 5 || opts.Timeout != 45*time.Second || opts.OutputCap != 4096 {
		t.Errorf("limits = %d/%v/%d, want 5/45s/4096", opts.MaxExecs, opts.Timeout, opts.OutputCap)
	}
}

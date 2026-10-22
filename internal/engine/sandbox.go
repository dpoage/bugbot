package engine

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/sandboxenv"
	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/llmkit/sandbox"
)

// buildSandboxOpts constructs a funnel.SandboxOpts from the config. When
// verify.sandbox_exec is false (the default) it returns a zero-value
// SandboxOpts (feature disabled). When the flag is enabled but no sandbox
// backend is available it also returns the zero value, with degraded=true so
// the caller can warn the user (the scan still runs, just without the
// empirical tool). An error is returned only when sandbox_exec is explicitly
// enabled and the sandbox backend cannot be constructed.
func buildSandboxOpts(cfg config.Config) (opts funnel.SandboxOpts, degraded bool, err error) {
	if !cfg.Verify.SandboxExec {
		return funnel.SandboxOpts{}, false, nil
	}
	if !sandboxenv.Available(cfg.Sandbox) {
		return funnel.SandboxOpts{}, true, nil
	}
	// sb is handed off inside funnel.SandboxOpts and lives for the whole funnel
	// run across scan/daemon/review/sweep/verify with no single natural
	// defer-Close scope here; NewCLI/NewBwrap's stale-cache purge is the
	// backstop.
	sb, err := sandboxenv.New(cfg.Sandbox)
	if err != nil {
		return funnel.SandboxOpts{}, false, fmt.Errorf("build verify sandbox: %w", err)
	}
	// Verify's sandbox_exec tool is refuter-driven adversarial evidence
	// gathering, not a repro run — writable local_mounts (bugbot-wjc2) are
	// scoped to the repro paths (execute/preview/verify smoke/capability
	// probe/playbook battery); this seam only ever needs the RO half.
	ro, _ := sandboxenv.LocalMounts(cfg.Sandbox)
	return funnel.SandboxOpts{
		Sandbox:     sb,
		Enabled:     true,
		MinSeverity: cfg.Verify.SandboxMinSeverity,
		MaxExecs:    cfg.Verify.SandboxMaxExecs,
		DepStrategy: sandboxenv.DepStrategy(cfg.Sandbox.DepStrategy),
		SetupCmds:   cfg.Sandbox.SetupCmds,
		LocalMounts: ro,
	}, false, nil
}

// SandboxRemediationHint returns the doctor-facing remediation suggestion for
// a failed sandbox toolchain check, phrased for whichever backend is
// configured: the container backend needs a different (toolchain-capable)
// image; bwrap needs the missing toolchain added to sandbox.host_toolchains
// instead — advising an operator to set sandbox.image under bwrap would be
// actively wrong, since bwrap ignores it entirely.
func SandboxRemediationHint(cfg config.Config) string {
	if cfg.Sandbox.Backend == "bwrap" {
		return "add the missing toolchain to sandbox.host_toolchains"
	}
	return "set sandbox.image to a toolchain-capable image"
}

// CloseSandbox releases a sandbox.Sandbox's own workspace-cache resources
// (CLI.Close / Bwrap.Close), if the concrete backend behind the interface
// has a Close method — Sandbox itself declares none, since Mock and test
// fakes have no such resources to release. Every caller that constructs a
// backend via sandboxenv.New/BuildReproducer and does not hand it off
// to a longer-lived consumer should defer this.
func CloseSandbox(sb sandbox.Sandbox) {
	if c, ok := sb.(interface{ Close() error }); ok {
		_ = c.Close()
	}
}

// closeSandbox is the in-package alias other functions here call.
func closeSandbox(sb sandbox.Sandbox) { CloseSandbox(sb) }

// packageSummaryProvider returns the lookup the reproducer uses to fetch a
// package's cached cartographer summary (store-backed). It powers the
// reproducer's task-prompt orientation and its get_package_context tool, so
// the agent reuses the finder's repo cartography instead of rediscovering the
// build/test layout from scratch. A miss (no cached row, or a query error)
// returns found=false and the reproducer degrades gracefully.
//
// Unlike the funnel's consumers (cartographer.go), this deliberately does NOT
// gate on the row's Fingerprint: the summary is orientation-only (the prompt
// tells the agent to "confirm specifics by reading files"), and within a scan
// the funnel has just refreshed summaries for the snapshot. A slightly stale
// summary at worst points the agent at the right package to read.
func packageSummaryProvider(st *store.Store) func(ctx context.Context, pkg string) (string, bool) {
	return func(ctx context.Context, pkg string) (string, bool) {
		sums, err := st.GetPackageSummaries(ctx, []string{pkg})
		if err != nil {
			return "", false
		}
		s, ok := sums[pkg]
		if !ok {
			return "", false
		}
		return s.Summary, true
	}
}

// depProbeInputs resolves the mounts/env ProbeCapabilities needs to see the
// SAME dependency provisioning a real repro run would have:
// cfg.Sandbox.DepStrategy's resolved mounts/env and local_mounts (host
// toolchains are mounted by the backend itself). sb is the constructed
// backend, passed as DepOptions.FetchSandbox so backend-conditional
// resolution matches the real run; the FETCH Prefetch closure is only built
// here, never run.
//
// A resolution error (e.g. a HOST cache genuinely missing on this host)
// degrades to nil/nil/nil rather than propagating: the probe must run BEFORE
// repro.New exists (its result feeds repro.Options.Capabilities), and a
// missing cache is exactly the kind of gap the resulting CapabilitySet
// should legitimately report as unavailable, not a reason to abort probing
// altogether. The third return (rwMounts) carries any operator "writable:
// true" local_mounts entries (bugbot-wjc2), so a probe sees the same
// writability the real run gets.
func depProbeInputs(cfg config.Config, sb sandbox.Sandbox, repoDir string) (roMounts, rwMounts []sandbox.ROMount, env []string) {
	ro, rw := sandboxenv.LocalMounts(cfg.Sandbox)
	res, err := sandboxenv.ResolveDeps(repoDir, sandboxenv.DepOptions{
		Strategy:       sandboxenv.DepStrategy(cfg.Sandbox.DepStrategy),
		FetchSandbox:   sb,
		LocalMounts:    ro,
		LocalRWMounts:  rw,
		HostToolchains: cfg.Sandbox.HostToolchains,
	})
	if err != nil {
		return nil, nil, nil
	}
	// bugbot-gu0o D3: a per-ecosystem degradation (e.g. an unvettable
	// requirements.txt) is carried on res.Warnings, not an error — log it
	// so a doctor/capability probe never silently drops it.
	for _, w := range res.Warnings {
		slog.Default().Warn("sandbox dependency resolution degraded", "repo", repoDir, "reason", w)
	}
	return res.ROMounts, res.RWMounts, res.Env
}

// probeCapabilities probes sb's ecosystem capabilities against repoDir with
// the same dependency mounts/env a real repro run gets. A refused probe Spec
// means every repro run on sb would be refused too, so it is an error.
func probeCapabilities(ctx context.Context, cfg config.Config, sb sandbox.Sandbox, repoDir string) (sandbox.CapabilitySet, error) {
	ro, rw, env := depProbeInputs(cfg, sb, repoDir)
	caps, err := sandboxenv.ProbeCapabilities(ctx, sb, cfg.Sandbox, sandbox.Spec{RepoDir: repoDir, ROMounts: ro, RWMounts: rw, Env: env})
	if err != nil {
		return nil, fmt.Errorf("probe sandbox capabilities: %w", err)
	}
	return caps, nil
}

// resolveDepsForPlaybook resolves repoDir's dependency strategy identically
// to depProbeInputs (same sandboxenv.ResolveDeps call, same DepOptions), but
// returns the full sandboxenv.Resolution instead of just (ROMounts, RWMounts,
// Env): repro.PlaybookOnce needs SetupCmds and Fingerprints too, to run the
// SAME battery spec a real repro run's dependency resolution would produce
// and to key its cache correctly (see resolutionFingerprint in
// internal/repro/playbook.go). Calling ResolveDeps a second time here
// (rather than threading depProbeInputs' internal result out) is cheap: it
// is stat-based dependency-marker detection, not network I/O — the FETCH
// strategy's Prefetch closure is only BUILT here, never invoked. A
// resolution error degrades to a zero Resolution, matching depProbeInputs'
// own degrade-not-abort behavior: PlaybookOnce with mount-less/env-less
// inputs still produces a valid (just less complete) battery instead of
// blocking reproducer construction.
func resolveDepsForPlaybook(cfg config.Config, sb sandbox.Sandbox, repoDir string) sandboxenv.Resolution {
	ro, rw := sandboxenv.LocalMounts(cfg.Sandbox)
	res, err := sandboxenv.ResolveDeps(repoDir, sandboxenv.DepOptions{
		Strategy:       sandboxenv.DepStrategy(cfg.Sandbox.DepStrategy),
		FetchSandbox:   sb,
		LocalMounts:    ro,
		LocalRWMounts:  rw,
		HostToolchains: cfg.Sandbox.HostToolchains,
	})
	if err != nil {
		return sandboxenv.Resolution{}
	}
	for _, w := range res.Warnings {
		slog.Default().Warn("sandbox dependency resolution degraded", "repo", repoDir, "reason", w)
	}
	return res
}

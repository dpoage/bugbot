package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// buildSandboxOpts constructs a funnel.SandboxOpts from the config. When
// verify.sandbox_exec is false (the default) it returns a zero-value
// SandboxOpts (feature disabled). When the flag is enabled but no container
// runtime is available it also returns the zero value, with degraded=true so
// the caller can warn the user (the scan still runs, just without the
// empirical tool). An error is returned only when sandbox_exec is explicitly
// enabled and the sandbox backend cannot be constructed.
func buildSandboxOpts(cfg config.Config) (opts funnel.SandboxOpts, degraded bool, err error) {
	if !cfg.Verify.SandboxExec {
		return funnel.SandboxOpts{}, false, nil
	}
	runtime, ok := sandbox.Detect()
	if !ok {
		return funnel.SandboxOpts{}, true, nil
	}
	// sb is handed off inside funnel.SandboxOpts and lives for the whole funnel
	// run across scan/daemon/review/sweep/verify with no single natural
	// defer-Close scope here; NewCLI's 24h stale-cache purge is the backstop.
	sb, err := sandbox.NewCLI(runtime, cfg.Sandbox.Image, sandboxRunOpts(cfg)...)
	if err != nil {
		return funnel.SandboxOpts{}, false, fmt.Errorf("build verify sandbox: %w", err)
	}
	return funnel.SandboxOpts{
		Sandbox:     sb,
		Enabled:     true,
		MinSeverity: cfg.Verify.SandboxMinSeverity,
		MaxExecs:    cfg.Verify.SandboxMaxExecs,
		DepStrategy: sandbox.DepStrategy(cfg.Sandbox.DepStrategy),
		SetupCmds:   cfg.Sandbox.SetupCmds,
		LocalMounts: localMountsFromConfig(cfg),
	}, false, nil
}

// sandboxRunOpts returns the sandbox.Option list every sandbox in the app
// shares, derived from config. It enables the idle-timeout watchdog (dynamic,
// progress-based cancellation) when sandbox.idle_timeout_seconds > 0. Built
// once here so scan, verify, the analyzer seed, and the daemon stay
// consistent.
func sandboxRunOpts(cfg config.Config) []sandbox.Option {
	var opts []sandbox.Option
	if cfg.Sandbox.Network != "" {
		// Apply the operator's configured network as the sandbox DEFAULT for every
		// stage (probe, verify, repro, patch). A Spec that leaves Network unset
		// inherits this; stages no longer hardcode "none" and silently drop the
		// config (which broke CMake FetchContent builds under network=host).
		opts = append(opts, sandbox.WithNetwork(cfg.Sandbox.Network))
	}
	if cfg.Sandbox.CPUs > 0 {
		// Operator-configured CPU cap. Without this every sandbox run kept the
		// backend default (2 CPUs) and silently ignored sandbox.cpus.
		opts = append(opts, sandbox.WithCPUs(float64(cfg.Sandbox.CPUs)))
	}
	if cfg.Sandbox.MemoryMB > 0 {
		// Operator-configured memory cap. Without this every sandbox run kept the
		// backend default (2048 MB) and silently ignored sandbox.memory_mb. A
		// Spec's own MemoryMB still wins.
		opts = append(opts, sandbox.WithMemoryMB(cfg.Sandbox.MemoryMB))
	}
	if cfg.Sandbox.PidsLimit > 0 {
		// Operator-configured pids cap. Without this every sandbox run kept the
		// backend default (256), which is far too low for build systems that spawn
		// worker/virtual-thread pools: the Bazel JVM crashes at "unable to create
		// native thread: ... process/resource limits reached" during analysis, so
		// every Bazel-repo reproduction failed as environment_error and no finding
		// was ever promoted to Tier-1.
		opts = append(opts, sandbox.WithPidsLimit(cfg.Sandbox.PidsLimit))
	}
	if cfg.Sandbox.IdleTimeoutSeconds > 0 {
		opts = append(opts, sandbox.WithIdleTimeout(time.Duration(cfg.Sandbox.IdleTimeoutSeconds)*time.Second))
	}
	if cfg.Sandbox.TimeoutSeconds > 0 {
		// Hard wall-clock ceiling for every sandbox run. Previously dropped: the
		// backend kept its 10-minute default and the reproducer forced 90s, so a
		// heavy build (vendored deps + engine + test) was killed long before it
		// could finish. A Spec's own Timeout still wins; repro sets it from this
		// same config value so both agree.
		opts = append(opts, sandbox.WithTimeout(time.Duration(cfg.Sandbox.TimeoutSeconds)*time.Second))
	}
	return opts
}

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

// localMountsFromConfig converts the operator's sandbox.local_mounts config
// entries into read-only sandbox.ROMounts. They are Shared=true (host-owned
// source trees that must NOT be SELinux :Z relabeled) per the local-mount
// contract; absolute-path, container-uniqueness, and existence checks already
// ran in config.Validate. Shared by the repro and funnel sandbox paths so both
// expose the same out-of-tree dependency directories offline.
func localMountsFromConfig(cfg config.Config) []sandbox.ROMount {
	if len(cfg.Sandbox.LocalMounts) == 0 {
		return nil
	}
	mounts := make([]sandbox.ROMount, len(cfg.Sandbox.LocalMounts))
	for i, m := range cfg.Sandbox.LocalMounts {
		mounts[i] = sandbox.ROMount{HostPath: m.Host, ContainerPath: m.Container, Shared: true}
	}
	return mounts
}

// SandboxRunOpts is the exported wrapper for sandboxRunOpts, for callers
// outside engine that build their own sandbox.CLI against the app's shared
// config-derived defaults (e.g. `bugbot bundle replay`, internal/cli/bundle.go)
// without going through engine.Open/BuildReproducer.
func SandboxRunOpts(cfg config.Config) []sandbox.Option { return sandboxRunOpts(cfg) }

// LocalMountsFromConfig is the exported wrapper for localMountsFromConfig,
// for the same external callers as SandboxRunOpts.
func LocalMountsFromConfig(cfg config.Config) []sandbox.ROMount { return localMountsFromConfig(cfg) }

// hostToolchainProbeInputs resolves cfg.Sandbox.HostToolchains into the
// ROMounts/Env pair ProbeCapabilities needs to see a mounted host toolchain
// (bugbot-14g0 acceptance 4). It duplicates the resolution repro.New performs
// internally via DepOptions.HostToolchains — cheap, deterministic, host-only
// filesystem/PATH work with no side effects — because the capability probe
// must run BEFORE repro.New exists (its result feeds repro.Options.Capabilities).
func hostToolchainProbeInputs(cfg config.Config) ([]sandbox.ROMount, []string) {
	if len(cfg.Sandbox.HostToolchains) == 0 {
		return nil, nil
	}
	tc, err := sandbox.ResolveHostToolchains(cfg.Sandbox.HostToolchains)
	if err != nil || len(tc.Mounts) == 0 {
		return nil, nil
	}
	var env []string
	if tc.PathPrepend != "" {
		env = []string{"PATH=" + tc.PathPrepend + ":" + sandbox.DefaultContainerPath}
	}
	return tc.Mounts, env
}

package funnel

// finder_exec_gate.go resolves the finder-exec decision for a run: the
// language half of the auto gate (probe), the one-time dependency prefetch,
// the FinderExecStatus line, and the finderExecState S3 consumes (seam R2).
import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/dpoage/llmkit/sandbox"

	"github.com/dpoage/bugbot/internal/ecosystem"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/sandboxenv"
)

// FinderExecLinePrefix starts every finder-exec status line (cli scan stdout
// and the slog line); the wording is a user-taught surface (X6).
const FinderExecLinePrefix = "Finder exec: "

// FinderExecOpts is the engine's provisioning output for finder exec. The
// zero value means the path that built the Funnel did not provision the
// feature (review / sweep / daemon cycles — ruling E5): no status line is
// emitted and no tool is attached.
type FinderExecOpts struct {
	// Sandbox is the one backend instance for the scan (shared with verify's
	// sandbox_exec when that is also on). Nil means OffReason is set.
	Sandbox sandbox.Sandbox
	// Unsandboxed marks Sandbox as the bench-only host backend: network is
	// host and no mounts/setup/dep env apply.
	Unsandboxed bool
	// BackendLabel names the isolating backend for the on status line
	// ("podman" | "docker" | "bwrap").
	BackendLabel string
	// Image names the sandbox image in the probe-fail status line; empty
	// under bwrap (no image concept).
	Image string
	// Network is the network mode every finder-exec Spec must set
	// explicitly: none on isolating backends, host on unsandboxed (E1 —
	// cfg sandbox.network is not consulted for finder probes).
	Network sandbox.NetworkMode
	// SkipProbe is true for scan.finder_exec: on — the language/toolchain
	// probe is skipped and the status line omits language=.
	SkipProbe bool
	// MaxExecs bounds run_code calls per finder unit (>= 1); Timeout bounds
	// one exec (> 0); OutputCap bounds each rendered output stream (> 0).
	MaxExecs  int
	Timeout   time.Duration
	OutputCap int
	// DepStrategy, SetupCmds, LocalMounts are the f.deps inputs (from
	// cfg.Sandbox), independent of verify.sandbox_exec (A1). Nil under
	// unsandboxed — nothing may be mounted or run as setup (A3).
	DepStrategy sandboxenv.DepStrategy
	SetupCmds   [][]string
	LocalMounts []sandbox.ROMount
	// Probe runs the capability probe against Sandbox with the given base
	// Spec (RepoDir + f.deps mounts/env); set whenever Sandbox is set.
	Probe func(ctx context.Context, base sandbox.Spec) (sandbox.CapabilitySet, error)
	// OffReason is the fully-resolved reason when provisioning decided off
	// without the snapshot (mode off, auto+unsandboxed, no usable backend).
	OffReason string
}

// finderExecState is the resolved decision S3 consumes (seam R2): zero value
// = off; else Sandbox is non-nil and f.deps is populated.
type finderExecState struct {
	Sandbox   sandbox.Sandbox
	Network   sandbox.NetworkMode
	MaxExecs  int
	Timeout   time.Duration
	OutputCap int
}

func (s finderExecState) enabled() bool { return s.Sandbox != nil }

func finderExecOff(reason string) string {
	return FinderExecLinePrefix + "off (" + reason + ")"
}

// resolveFinderExec runs the gate for a modeFull run: language half of the
// auto gate, prefetch, the Funnel's decision state, Result.FinderExecStatus,
// and one slog line. No-op when the Funnel was not provisioned (E5).
func (f *Funnel) resolveFinderExec(ctx context.Context, result *Result, langs []ingest.Language) {
	fe := f.opts.FinderExec
	if fe.Sandbox == nil && fe.OffReason == "" {
		return
	}
	status, state := f.decideFinderExec(ctx, fe, langs)
	f.finderExec = state
	result.FinderExecStatus = status
	if status != "" {
		slog.Info(status)
	}
}

// decideFinderExec is the gate body; see resolveFinderExec. Off outcomes
// return a zero state.
func (f *Funnel) decideFinderExec(ctx context.Context, fe FinderExecOpts, langs []ingest.Language) (string, finderExecState) {
	if fe.Sandbox == nil {
		return finderExecOff(fe.OffReason), finderExecState{}
	}
	language := ""
	if !fe.SkipProbe {
		if len(langs) == 0 {
			return finderExecOff("no target language in scan scope"), finderExecState{}
		}
		lang := langs[0]
		eco := ecosystem.FromLanguage(lang)
		if eco == "" {
			return finderExecOff("no toolchain probe for " + string(lang)), finderExecState{}
		}
		base := sandbox.Spec{RepoDir: f.repo.Root(), ROMounts: f.deps.ROMounts, Env: f.deps.Env}
		caps, err := fe.Probe(ctx, base)
		if err != nil {
			return finderExecOff("capability probe failed: " + err.Error()), finderExecState{}
		}
		// STRICT: a probe that never ran counts as unavailable (no Go
		// unprobed=available rule).
		if !caps.Available(eco, ecosystem.BaseMode(eco)) {
			binary := ecosystem.ToolchainBinary(eco)
			if fe.BackendLabel == sandboxenv.BackendBwrap {
				return finderExecOff("host lacks " + binary + " under bwrap"), finderExecState{}
			}
			return finderExecOff("sandbox image " + fe.Image + " lacks " + binary), finderExecState{}
		}
		language = string(lang)
	}
	if err := f.ensureDepPrefetch(ctx); err != nil {
		return finderExecOff("dependency prefetch failed: " + err.Error()), finderExecState{}
	}
	state := finderExecState{
		Sandbox: fe.Sandbox, Network: fe.Network,
		MaxExecs: fe.MaxExecs, Timeout: fe.Timeout, OutputCap: fe.OutputCap,
	}
	if fe.Unsandboxed {
		return fmt.Sprintf("%son (sandbox=UNSANDBOXED host, network=host, max_execs=%d per finder)", FinderExecLinePrefix, fe.MaxExecs), state
	}
	line := fmt.Sprintf("%son (sandbox=%s", FinderExecLinePrefix, fe.BackendLabel)
	if language != "" {
		line += ", language=" + language
	}
	line += fmt.Sprintf(", network=none, dep_strategy=%s, max_execs=%d per finder)", f.deps.Strategy, fe.MaxExecs)
	return line, state
}

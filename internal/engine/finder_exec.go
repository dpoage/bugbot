package engine

// finder_exec.go provisions the finder-exec backend for one scan: the
// snapshot-independent half of the scan.finder_exec decision (mode x backend
// x availability). Called from Dispatcher.Scan ONLY (ruling E5).
import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/dpoage/llmkit/sandbox"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/sandboxenv"
)

// unsandboxedBanner is written to the scan's ErrOut whenever finder probes
// run on the host backend, before any Finder exec line (wording pinned by
// the round's approved status-line table).
const unsandboxedBanner = "!!! UNSANDBOXED: finder probes run directly on this host (workspace copy, no container isolation, network=host, your OS user's privileges). Not applied on the host: sandbox.image, cpus, memory_mb, pids_limit, idle_timeout_seconds, scratch_size_mb, workspace_growth_ceiling_mb, dep_strategy, setup_cmds, local_mounts, host_toolchains. !!!"

type finderExecClass int

const (
	finderExecOffMode            finderExecClass = iota // scan.finder_exec=off
	finderExecOffUnsandboxedAuto                        // auto never runs unsandboxed
	finderExecOffNoBackend                              // no usable isolating backend
	finderExecHost                                      // NewHostExec, bench-only
	finderExecReal                                      // build or reuse an isolating backend
)

// decideFinderExecBackend is the pure decision table over the
// snapshot-independent inputs. verifyBuilt reports whether the verify path
// already constructed a sandbox for this scan.
func decideFinderExecBackend(mode, cfgBackend string, realAvailable, verifyBuilt bool) finderExecClass {
	if mode == "off" {
		return finderExecOffMode
	}
	unsandboxed := cfgBackend == sandboxenv.BackendUnsandboxed
	if unsandboxed && mode != "on" {
		return finderExecOffUnsandboxedAuto
	}
	if unsandboxed {
		return finderExecHost
	}
	if !realAvailable && !verifyBuilt {
		return finderExecOffNoBackend
	}
	return finderExecReal
}

// newFinderExecSandbox constructs the finder-exec backend; a var so tests can observe Close.
var newFinderExecSandbox = sandboxenv.New

// provisionFinderExecForScan resolves finder-exec provisioning for one Scan
// and writes the UNSANDBOXED banner when the host backend is active. owned is
// the backend this call constructed (nil when off or shared with verify); the
// caller must CloseSandbox it after the funnel finishes.
func provisionFinderExecForScan(cfg config.Config, verifySB funnel.SandboxOpts, errOut io.Writer) (opts funnel.FinderExecOpts, owned sandbox.Sandbox) {
	mode := cfg.Scan.FinderExec
	verifyBuilt := verifySB.Enabled && verifySB.Sandbox != nil
	switch decideFinderExecBackend(mode, cfg.Sandbox.Backend, sandboxenv.Available(cfg.Sandbox), verifyBuilt) {
	case finderExecOffMode:
		return funnel.FinderExecOpts{OffReason: "scan.finder_exec=off"}, nil
	case finderExecOffUnsandboxedAuto:
		return funnel.FinderExecOpts{OffReason: "auto never runs unsandboxed; set scan.finder_exec: on"}, nil
	case finderExecOffNoBackend:
		return funnel.FinderExecOpts{OffReason: "no sandbox backend available: " + sandboxenv.UnavailableReason(cfg.Sandbox)}, nil
	case finderExecHost:
		_, _ = fmt.Fprintln(errOut, unsandboxedBanner)
		host := sandbox.NewHostExec()
		return funnel.FinderExecOpts{
			Sandbox:     host,
			Unsandboxed: true,
			Network:     sandbox.NetworkHost,
			SkipProbe:   true,
			MaxExecs:    cfg.Scan.FinderExecMaxExecs,
			Timeout:     time.Duration(cfg.Scan.FinderExecTimeoutSeconds) * time.Second,
			OutputCap:   cfg.Scan.FinderExecOutputBytes,
		}, host
	default:
		sb := verifySB.Sandbox
		if sb == nil {
			b, err := newFinderExecSandbox(cfg.Sandbox)
			if err != nil {
				return funnel.FinderExecOpts{OffReason: "sandbox build failed: " + err.Error()}, nil
			}
			sb, owned = b, b
		}
		strategy, setup, ro := sandboxDepInputs(cfg)
		return funnel.FinderExecOpts{
			Sandbox:      sb,
			BackendLabel: sandboxenv.BackendLabel(cfg.Sandbox),
			Image:        cfg.Sandbox.Image,
			Network:      sandbox.NetworkNone,
			SkipProbe:    mode == "on",
			MaxExecs:     cfg.Scan.FinderExecMaxExecs,
			Timeout:      time.Duration(cfg.Scan.FinderExecTimeoutSeconds) * time.Second,
			OutputCap:    cfg.Scan.FinderExecOutputBytes,
			DepStrategy:  strategy,
			SetupCmds:    setup,
			LocalMounts:  ro,
			Probe: func(ctx context.Context, base sandbox.Spec) (sandbox.CapabilitySet, error) {
				return sandboxenv.ProbeCapabilities(ctx, sb, cfg.Sandbox, base)
			},
		}, owned
	}
}

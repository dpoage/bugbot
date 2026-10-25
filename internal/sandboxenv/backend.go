// Package sandboxenv is bugbot's policy layer over llmkit/sandbox: it builds
// the configured backend from config.Sandbox, resolves each ecosystem's
// offline dependency provisioning (ResolveDeps), and probes and caches the
// backend's ecosystem capabilities (ProbeCapabilities). Execution itself —
// every backend, the Spec/Result contract, and the watchdog — is llmkit's.
package sandboxenv

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/dpoage/llmkit/sandbox"

	"github.com/dpoage/bugbot/internal/config"
)

// BackendBwrap is the config.Sandbox.Backend value selecting the bubblewrap
// backend; every other accepted value selects the container CLI backend.
const BackendBwrap = "bwrap"

// BackendUnsandboxed is the config.Sandbox.Backend value selecting the
// bench-only unsandboxed host backend. It is never an isolating backend:
// New refuses it and Available reports false, so verify sandbox_exec,
// analyzer seeding, repro, and doctor behave as "no sandbox backend".
const BackendUnsandboxed = "unsandboxed"

// New constructs the backend cfg.Backend selects, with every sandbox knob
// mapped onto its llmkit option. A knob the backend cannot honor fails here
// rather than being dropped: llmkit refuses unusable limits and network
// modes, and New refuses a host_toolchains entry that does not resolve.
func New(cfg config.Sandbox) (sandbox.Sandbox, error) {
	if cfg.Backend == BackendUnsandboxed {
		return nil, fmt.Errorf("sandboxenv: backend %q provides no isolating sandbox (bench-only; only finder-exec provisioning with scan.finder_exec: on runs on the host)", BackendUnsandboxed)
	}
	if cfg.Backend != BackendBwrap {
		cli, err := NewCLI(cfg, cfg.Image)
		if err != nil {
			return nil, err
		}
		return cli, nil
	}
	opts, err := sharedOptions(cfg)
	if err != nil {
		return nil, err
	}
	policy := sandbox.CapRequired
	if cfg.AllowUncapped {
		policy = sandbox.CapBestEffort
	}
	bw, err := sandbox.NewBwrap(append(opts, sandbox.WithCapPolicy(policy))...)
	if err != nil {
		return nil, err
	}
	return bw, nil
}

// NewCLI constructs the container CLI backend from cfg with image as its
// default image (bundle replay overrides cfg.Image with the bundle's own).
// An empty cfg.Runtime auto-detects podman, then docker.
func NewCLI(cfg config.Sandbox, image string) (*sandbox.CLI, error) {
	opts, err := sharedOptions(cfg)
	if err != nil {
		return nil, err
	}
	return sandbox.NewCLI(append(opts, sandbox.WithRuntime(cfg.Runtime), sandbox.WithImage(image))...)
}

// sharedOptions maps the knobs both backends honor. The numeric knobs are
// passed unconditionally: config.Validate already refuses the values llmkit
// refuses, and 0 is an explicit disable for idle timeout and growth ceiling.
func sharedOptions(cfg config.Sandbox) ([]sandbox.Option, error) {
	opts := []sandbox.Option{
		sandbox.WithCPUs(float64(cfg.CPUs)),
		sandbox.WithMemoryMB(cfg.MemoryMB),
		sandbox.WithPidsLimit(cfg.PidsLimit),
		sandbox.WithTimeout(time.Duration(cfg.TimeoutSeconds) * time.Second),
		sandbox.WithIdleTimeout(time.Duration(cfg.IdleTimeoutSeconds) * time.Second),
		sandbox.WithScratchSizeMB(cfg.ScratchSizeMB),
		sandbox.WithWorkspaceGrowthCeilingMB(cfg.WorkspaceGrowthCeilingMB),
	}
	if cfg.Network != "" {
		opts = append(opts, sandbox.WithNetwork(sandbox.NetworkMode(cfg.Network)))
	}
	if len(cfg.HostToolchains) > 0 {
		res := sandbox.ResolveHostToolchains(cfg.HostToolchains)
		if len(res.Unresolved) > 0 {
			return nil, fmt.Errorf("sandbox.host_toolchains: %s did not resolve on this host", strings.Join(res.Unresolved, ", "))
		}
		opts = append(opts, sandbox.WithHostToolchains(res))
	}
	return opts, nil
}

// Available reports whether the configured backend can run on this host,
// without constructing it.
func Available(cfg config.Sandbox) bool {
	if cfg.Backend == BackendUnsandboxed {
		return false
	}
	if cfg.Backend == BackendBwrap {
		ok, _ := sandbox.DetectBwrap()
		return ok
	}
	_, ok := containerRuntime(cfg)
	return ok
}

// BackendLabel names the configured backend for log and status lines:
// "bwrap", the container runtime, or "none" when it is unavailable.
func BackendLabel(cfg config.Sandbox) string {
	if cfg.Backend == BackendUnsandboxed {
		return BackendUnsandboxed
	}
	if cfg.Backend == BackendBwrap {
		return BackendBwrap
	}
	if rt, ok := containerRuntime(cfg); ok {
		return rt
	}
	return "none"
}

// UnavailableReason is the operator-facing detail for the "no sandbox
// backend available" status line: "" when a real isolating backend is
// available, otherwise why it is not.
func UnavailableReason(cfg config.Sandbox) string {
	if cfg.Backend == BackendUnsandboxed {
		return BackendUnsandboxed + " provides no isolating sandbox"
	}
	if cfg.Backend == BackendBwrap {
		if ok, reason := sandbox.DetectBwrap(); !ok {
			return "bwrap unusable (" + reason + ")"
		}
		return ""
	}
	if cfg.Runtime != "" {
		return cfg.Runtime + " not on PATH"
	}
	return "podman/docker not on PATH"
}

// containerRuntime resolves the runtime NewCLI would use: cfg.Runtime when
// set (and on PATH), else the auto-detected one.
func containerRuntime(cfg config.Sandbox) (string, bool) {
	if cfg.Runtime == "" {
		return sandbox.Detect()
	}
	if _, err := exec.LookPath(cfg.Runtime); err != nil {
		return "", false
	}
	return cfg.Runtime, true
}

// RunImage returns the Spec.Image runs on sb should record: cfg.Image on the
// container backend, "" on backends with no image concept (Bwrap and
// HostExec refuse a non-empty Spec.Image).
func RunImage(sb sandbox.Sandbox, cfg config.Sandbox) string {
	if _, ok := sb.(*sandbox.CLI); ok {
		return cfg.Image
	}
	return ""
}

// LocalMounts converts cfg.LocalMounts into Shared (never SELinux-relabeled)
// mounts, split into read-only and writable. config.Validate has already
// checked paths and container-path uniqueness.
func LocalMounts(cfg config.Sandbox) (ro, rw []sandbox.ROMount) {
	for _, m := range cfg.LocalMounts {
		mount := sandbox.ROMount{HostPath: m.Host, ContainerPath: m.Container, Shared: true}
		if m.Writable {
			rw = append(rw, mount)
			continue
		}
		ro = append(ro, mount)
	}
	return ro, rw
}

// KillReason is sandbox.Result.KillReason in bugbot's terms: a growth-ceiling
// kill names the sandbox.workspace_growth_ceiling_mb knob an operator can
// change rather than llmkit's option name. "" when res was not InfraKilled.
func KillReason(res sandbox.Result) string {
	if res.WorkspaceQuotaExceeded {
		return "workspace growth exceeded the configured quota (sandbox.workspace_growth_ceiling_mb)"
	}
	return res.KillReason()
}

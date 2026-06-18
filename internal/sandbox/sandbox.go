// Package sandbox provides an isolated execution layer for running
// model-generated code against snapshots of arbitrary repositories.
//
// Because the commands executed here originate from an LLM and run against
// untrusted code, the package is built around defense in depth: the original
// repository is never mounted read-write, the workspace copy is the only
// writable mount, the container drops all capabilities and gains no new
// privileges, the root filesystem is read-only where practical, and the
// network defaults to "none".
//
// Read-only bind mounts (Spec.ROMounts) are the one window into host content:
// they let the container read shared, immutable data such as a Go module cache
// without granting write access. Because untrusted code can read whatever is
// mounted, callers MUST only mount public/cache content (e.g. a module cache of
// public package source) and NEVER secrets, credentials, or private trees.
//
// The primary backend (NewCLI) shells out to a container runtime CLI
// (podman or docker) rather than talking to a daemon API, which keeps the
// implementation runtime-agnostic and dependency-free (standard library only).
//
// A scriptable Mock backend (NewMock) implements the same Sandbox interface so
// that callers — for example the funnel and reproduce stages — can be tested
// without a real container runtime.
package sandbox

import (
	"context"
	"time"
)

// DefaultMaxOutputBytes is the per-stream cap applied to captured stdout and
// stderr. Output beyond this size is discarded and Result records that it was
// truncated.
const DefaultMaxOutputBytes = 1 << 20 // 1 MiB

// Spec describes a single sandboxed execution.
type Spec struct {
	// RepoDir is the host path to the repository snapshot to run against. It is
	// copied into a fresh temporary workspace before execution; the original is
	// never mounted writable (and is not mutated). Required.
	RepoDir string

	// Cmd is the command (argv) executed inside the container's workspace.
	// Required and non-empty.
	Cmd []string

	// Env is a list of KEY=VALUE environment variables to set inside the
	// container, in the same form as os.Environ.
	Env []string

	// Image overrides the backend's default image for this execution. When
	// empty, the backend's configured default image is used.
	Image string

	// CPUs is the CPU limit (e.g. 1.5 for one and a half cores). When <= 0 the
	// backend's default is used.
	CPUs float64

	// MemoryMB is the memory limit in megabytes. When <= 0 the backend's
	// default is used.
	MemoryMB int

	// Timeout bounds the execution wall-clock time as a HARD ceiling. When <= 0
	// the backend's default timeout is used. On expiry the container is forcibly
	// removed and Result.TimedOut is set.
	Timeout time.Duration

	// IdleTimeout bounds wall-clock time with NO observable progress (output
	// bytes or workspace filesystem activity). A run that keeps making progress
	// is allowed to continue up to Timeout; one that stalls for IdleTimeout is
	// cancelled and Result.TimedOut is set. When <= 0 the backend default is
	// used, and a zero backend default disables the watchdog (Timeout only).
	IdleTimeout time.Duration

	// Network selects the container network mode. The default (empty) resolves
	// to "none", disabling all network egress.
	Network string

	// WriteFiles are files to write into the workspace before execution, keyed
	// by path relative to the workspace root. This is how reproduction tests
	// are injected into the snapshot. Parent directories are created as needed.
	// Paths that escape the workspace (absolute, or containing "..") are
	// rejected as an error.
	WriteFiles map[string][]byte

	// ROMounts are additional host directories bind-mounted read-only into the
	// container, in addition to the writable workspace. They exist so a
	// dependency cache (e.g. a Go module cache) can be made available to an
	// otherwise network-none run without copying it into the workspace.
	//
	// Each mount is rendered as `-v host:ctr:ro,Z` and is NEVER writable. Both
	// paths must be absolute; empty paths and duplicate ContainerPaths are
	// rejected as an error by Exec.
	//
	// SECURITY: a read-only mount exposes host content to untrusted, model-
	// driven code. Callers must only mount public/cache content and never
	// secrets or private trees. See the package doc.
	ROMounts []ROMount

	// RWMounts are host directories bind-mounted WRITABLE into the container.
	// They exist ONLY for the dependency-prefetch step (DepStrategyFetch), where
	// a network-enabled, otherwise-hardened container runs `go mod download` to
	// populate a bugbot-managed module cache on the host. That same cache is
	// then exposed to the untrusted network-none run read-only via ROMounts.
	//
	// Do NOT use RWMounts for normal model-driven runs: the writable workspace
	// copy is the only writable surface those runs are meant to have. Rendered
	// as `-v host:ctr:rw,Z`; same absolute-path/uniqueness validation as
	// ROMounts. ContainerPaths must be unique across ROMounts and RWMounts
	// combined.
	RWMounts []ROMount

	// SetupCmds are optional ordered commands executed inside the container, in
	// the same network-none run, in the workspace directory, BEFORE Cmd. They
	// exist so non-Go ecosystems (npm, pip, cargo, etc.) can perform offline
	// package installation from a pre-mounted cache without altering the main
	// command.
	//
	// Examples: ["npm","ci","--offline"] or ["pip","install","--no-index","--find-links=/pipcache","."]
	//
	// When SetupCmds is non-empty the CLI backend wraps the execution in
	// /bin/sh: each command is shell-quoted and chained with "|| exit 125" so
	// any setup failure exits with code 125. Exit 125 is intentional:
	// internal/repro/interpret.go and patch.go both classify container exit
	// 125/126/127 as an environment_error, NOT a bug demonstration — a failed
	// "npm ci --offline" must never be misread as a successful repro. The
	// original Cmd is exec'd (via sh's exec builtin) so it retains its own
	// exit code and signal mask.
	//
	// Requires /bin/sh in the container image. Images used only for Go (the
	// default) set no SetupCmds, so existing images and behavior are untouched.
	SetupCmds [][]string
}

// ROMount is a single read-only bind mount of a host directory into the
// container. It is never writable.
type ROMount struct {
	// HostPath is the absolute host path to expose. Required.
	HostPath string
	// ContainerPath is the absolute path the mount appears at inside the
	// container. Required and unique across a Spec's ROMounts.
	ContainerPath string
	// Shared, when true, suppresses the SELinux :Z relabel suffix on this
	// mount. Use Shared=true for host directories that are NOT owned exclusively
	// by bugbot — in particular, the user's shared Go module cache
	// (~/go/pkg/mod). On SELinux-enforcing hosts (Fedora, RHEL — rootless
	// podman's home turf) :Z recursively relabels the target to a
	// container-PRIVATE MCS label. That is correct for bugbot-owned dirs (it
	// isolates them), but catastrophic for a shared cache: it is slow on
	// multi-GB trees, breaks the host go toolchain, and breaks any other
	// container concurrently sharing the same cache.
	//
	// When Shared=true the mount is rendered :ro with NO label suffix. This
	// means the container accesses the directory under its existing SELinux
	// context. Under a strict enforcing policy the container may get EACCES if
	// that policy does not allow the container domain to read the host user's
	// home content. That is the correct conservative failure — a permission
	// error is loud and actionable, whereas :Z silently corrupts shared state.
	// Users who hit EACCES can opt in to :z (lowercase, shared relabel) by
	// configuring a custom container_opts or by switching to dep_strategy:
	// fetch, which mounts a bugbot-owned cache (Shared=false, gets :Z).
	//
	// Bugbot-owned dirs (the fetch cache, prefetch RW target) leave Shared
	// false so they receive :Z isolation.
	Shared bool
}

// Result is the faithful outcome of a sandboxed execution.
//
// A non-zero ExitCode is NOT reported as a Go error: callers interpret exit
// codes themselves (a failing repro test is expected to exit non-zero). Only
// infrastructure failures — a missing runtime, a failed workspace copy, an
// inability to launch the container — are returned as errors from Exec.
type Result struct {
	// ExitCode is the process exit code from the command inside the container.
	// On timeout it is -1.
	ExitCode int

	// Stdout and Stderr are the captured output streams, each capped at the
	// backend's max output size. Truncation is recorded in the Truncated flags
	// and a trailing marker is appended to the captured text.
	Stdout string
	Stderr string

	// StdoutTruncated / StderrTruncated report whether the corresponding stream
	// exceeded the cap and was truncated.
	StdoutTruncated bool
	StderrTruncated bool

	// Duration is the measured wall-clock time of the execution.
	Duration time.Duration

	// TimedOut is true when the execution was killed because it exceeded the
	// effective timeout.
	TimedOut bool
}

// Sandbox is an isolated command executor. Implementations must be safe for
// concurrent use by multiple goroutines.
type Sandbox interface {
	// Exec runs spec to completion (or until the timeout / ctx cancellation)
	// and returns the captured Result. It returns a non-nil error only for
	// infrastructure failures; a non-zero exit code is reported via
	// Result.ExitCode, not as an error.
	Exec(ctx context.Context, spec Spec) (Result, error)
}

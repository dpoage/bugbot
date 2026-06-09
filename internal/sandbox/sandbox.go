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

	// Timeout bounds the execution wall-clock time. When <= 0 the backend's
	// default timeout is used. On expiry the container is forcibly removed and
	// Result.TimedOut is set.
	Timeout time.Duration

	// Network selects the container network mode. The default (empty) resolves
	// to "none", disabling all network egress.
	Network string

	// WriteFiles are files to write into the workspace before execution, keyed
	// by path relative to the workspace root. This is how reproduction tests
	// are injected into the snapshot. Parent directories are created as needed.
	// Paths that escape the workspace (absolute, or containing "..") are
	// rejected as an error.
	WriteFiles map[string][]byte
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

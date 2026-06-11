package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// candidateRuntimes is the auto-detect search order for the container runtime
// CLI. Podman is preferred (rootless, daemonless), with docker as a fallback.
var candidateRuntimes = []string{"podman", "docker"}

// Detect reports the first available container runtime CLI on PATH and whether
// one was found. Callers (and tests) use it to skip gracefully when no runtime
// is installed.
func Detect() (runtime string, ok bool) {
	for _, rt := range candidateRuntimes {
		if _, err := exec.LookPath(rt); err == nil {
			return rt, true
		}
	}
	return "", false
}

// CLI is a Sandbox backed by a container runtime CLI (podman or docker). It is
// safe for concurrent use: each Exec prepares its own workspace and launches
// its own uniquely-named container.
type CLI struct {
	runtime        string
	defaultImage   string
	defaultCPUs    float64
	defaultMemory  int
	defaultTimeout time.Duration
	defaultNetwork string
	pidsLimit      int
	maxOutputBytes int
}

// Option configures a CLI sandbox.
type Option func(*CLI)

// WithCPUs sets the default CPU limit applied when a Spec leaves CPUs unset.
func WithCPUs(c float64) Option { return func(s *CLI) { s.defaultCPUs = c } }

// WithMemoryMB sets the default memory limit (MB) applied when a Spec leaves
// MemoryMB unset.
func WithMemoryMB(m int) Option { return func(s *CLI) { s.defaultMemory = m } }

// WithTimeout sets the default execution timeout applied when a Spec leaves
// Timeout unset.
func WithTimeout(d time.Duration) Option { return func(s *CLI) { s.defaultTimeout = d } }

// WithNetwork sets the default network mode applied when a Spec leaves Network
// unset. The package default is "none".
func WithNetwork(n string) Option { return func(s *CLI) { s.defaultNetwork = n } }

// WithPidsLimit sets the --pids-limit cap. A value <= 0 disables the flag.
func WithPidsLimit(n int) Option { return func(s *CLI) { s.pidsLimit = n } }

// WithMaxOutputBytes overrides the per-stream output cap.
func WithMaxOutputBytes(n int) Option { return func(s *CLI) { s.maxOutputBytes = n } }

// NewCLI constructs a CLI sandbox. When runtime is empty it is auto-detected
// (podman, then docker); if none is found an error is returned. image is the
// default container image used when a Spec does not override it.
func NewCLI(runtime, image string, opts ...Option) (*CLI, error) {
	if runtime == "" {
		detected, ok := Detect()
		if !ok {
			return nil, errors.New("sandbox: no container runtime found on PATH (tried podman, docker)")
		}
		runtime = detected
	} else if _, err := exec.LookPath(runtime); err != nil {
		return nil, fmt.Errorf("sandbox: container runtime %q not found on PATH: %w", runtime, err)
	}

	if image == "" {
		return nil, errors.New("sandbox: a default image is required")
	}

	s := &CLI{
		runtime:        runtime,
		defaultImage:   image,
		defaultCPUs:    2,
		defaultMemory:  2048,
		defaultTimeout: 10 * time.Minute,
		defaultNetwork: "none",
		pidsLimit:      256,
		maxOutputBytes: DefaultMaxOutputBytes,
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Runtime returns the resolved runtime binary name (podman or docker).
func (s *CLI) Runtime() string { return s.runtime }

// randToken returns a 128-bit random hex string used to give each container a
// unique, collision-resistant name (so it can be reaped by name on timeout).
func randToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is catastrophic and effectively never happens;
		// fall back to a time-based token so naming still works.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// resolveParams applies backend defaults to a Spec, producing the concrete
// runParams for the run (workspace and containerName are filled in by Exec).
func (s *CLI) resolveParams(spec Spec) runParams {
	p := runParams{
		image:     s.defaultImage,
		network:   s.defaultNetwork,
		cpus:      s.defaultCPUs,
		memoryMB:  s.defaultMemory,
		pidsLimit: s.pidsLimit,
		env:       spec.Env,
		cmd:       spec.Cmd,
		roMounts:  spec.ROMounts,
		rwMounts:  spec.RWMounts,
	}
	if spec.Image != "" {
		p.image = spec.Image
	}
	if spec.Network != "" {
		p.network = spec.Network
	}
	if spec.CPUs > 0 {
		p.cpus = spec.CPUs
	}
	if spec.MemoryMB > 0 {
		p.memoryMB = spec.MemoryMB
	}
	return p
}

// Exec implements Sandbox. See the Sandbox interface for the error contract:
// only infrastructure failures are returned as errors; a non-zero exit code is
// reported in Result.ExitCode.
func (s *CLI) Exec(ctx context.Context, spec Spec) (Result, error) {
	if len(spec.Cmd) == 0 {
		return Result{}, errors.New("sandbox: spec.Cmd must be non-empty")
	}
	if err := validateMounts(spec.ROMounts, spec.RWMounts); err != nil {
		return Result{}, err
	}

	ws, err := prepareWorkspace(spec.RepoDir, spec.WriteFiles)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = os.RemoveAll(ws) }()

	p := s.resolveParams(spec)
	p.workspace = ws
	p.containerName = "bugbot-" + randToken()

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = s.defaultTimeout
	}

	// runCtx bounds the run by the timeout, but is also cancelled if the
	// caller's ctx is cancelled first.
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := buildRunArgs(p)
	cmd := exec.CommandContext(runCtx, s.runtime, args...)

	stdout := newCappedBuffer(s.maxOutputBytes)
	stderr := newCappedBuffer(s.maxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	res := Result{Duration: duration}
	res.Stdout, res.StdoutTruncated = stdout.result()
	res.Stderr, res.StderrTruncated = stderr.result()

	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	if timedOut {
		res.TimedOut = true
		res.ExitCode = -1
		// The runtime may not have torn the container down in time; reap it by
		// name to honor the always-clean-up guarantee. Best-effort with its own
		// short timeout so cleanup cannot itself hang.
		s.forceRemove(p.containerName)
		return res, nil
	}

	// Caller cancellation (not our timeout): surface as an error.
	if ctxErr := ctx.Err(); ctxErr != nil {
		s.forceRemove(p.containerName)
		return res, fmt.Errorf("sandbox: execution cancelled: %w", ctxErr)
	}

	if runErr == nil {
		res.ExitCode = 0
		return res, nil
	}

	// A non-zero exit is reported via ExitCode, not as an error.
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}

	// Anything else (binary missing, failed to start) is infrastructure error.
	return res, fmt.Errorf("sandbox: run %s: %w", s.runtime, runErr)
}

// forceRemove best-effort removes a container by name, used to guarantee
// cleanup of a container that outran its timeout.
func (s *CLI) forceRemove(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.runtime, removeArgs(name)...)
	_ = cmd.Run()
}

var _ Sandbox = (*CLI)(nil)

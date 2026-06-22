package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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
	// defaultIdleTimeout is the inactivity window applied when a Spec leaves
	// IdleTimeout unset. Zero disables the idle watchdog (absolute timeout only).
	defaultIdleTimeout time.Duration
	defaultNetwork     string
	pidsLimit          int
	maxOutputBytes     int
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

// WithIdleTimeout sets the default idle (no-progress) window applied when a Spec
// leaves IdleTimeout unset. A run is cancelled only after this long with no
// observable progress; the absolute WithTimeout remains a hard ceiling. Zero
// disables the watchdog.
func WithIdleTimeout(d time.Duration) Option { return func(s *CLI) { s.defaultIdleTimeout = d } }

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

// Limits returns the effective resource caps the backend applies to a Spec that
// does not override them: the default CPU count, memory ceiling (MB), and pids
// limit. Exposed so status/doctor and tests can confirm the configured
// sandbox.cpus / sandbox.memory_mb actually reached the backend.
func (s *CLI) Limits() (cpus float64, memoryMB, pidsLimit int) {
	return s.defaultCPUs, s.defaultMemory, s.pidsLimit
}

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
		setupCmds: spec.SetupCmds,
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
	idleTimeout := spec.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = s.defaultIdleTimeout
	}

	// runCtx bounds the run by the absolute timeout (a hard ceiling) and is
	// cancelled if the caller's ctx is cancelled first or the idle watchdog
	// fires.
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := buildRunArgs(p)
	cmd := exec.CommandContext(runCtx, s.runtime, args...)

	stdout := newCappedBuffer(s.maxOutputBytes)
	stderr := newCappedBuffer(s.maxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Idle watchdog: instead of killing a healthy-but-slow run at a fixed
	// deadline, cancel only after idleTimeout elapses with NO observable
	// progress. Progress is language-agnostic and layered cheapest-first:
	//   1. bytes written to stdout/stderr, and any change to the writable
	//      workspace tree (build caches, compiled artifacts, generated files —
	//      every ecosystem writes one or the other while it works);
	//   2. only when (1) is flat, a container-CPU probe, so a compiler churning
	//      silently on one large translation unit (no output, no fs writes yet)
	//      still counts as progress.
	// The absolute timeout above stays a hard ceiling.
	var idleKilled atomic.Bool
	done := make(chan struct{})
	if idleTimeout > 0 {
		fingerprint := func() progressSnapshot {
			ps := progressSnapshot{outputBytes: stdout.written() + stderr.written()}
			ps.fsSize, ps.fsCount, ps.fsMaxModNano = workspaceProgress(ws)
			return ps
		}
		active := func() bool { return s.containerCPUBusy(p.containerName) }
		go watchIdle(done, fingerprint, active, idleTimeout, idlePollInterval(idleTimeout), &idleKilled, cancel)
	}

	start := time.Now()
	runErr := cmd.Run()
	close(done)
	duration := time.Since(start)

	res := Result{Duration: duration}
	res.Stdout, res.StdoutTruncated = stdout.result()
	res.Stderr, res.StderrTruncated = stderr.result()

	// Outcome precedence. A process that returned its OWN status — a clean exit
	// or a real non-zero code — was not killed by us, so those win first: a
	// watchdog (or deadline) firing in the same instant can never mask a genuine
	// repro verdict. Our kills surface as a signal (ExitCode -1) and fall through
	// to the timeout/cancel branches below.
	if runErr == nil {
		res.ExitCode = 0
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && exitErr.ExitCode() >= 0 {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}

	// Caller cancellation (not our timeout): surface as an error.
	if ctxErr := ctx.Err(); ctxErr != nil {
		s.forceRemove(p.containerName)
		return res, fmt.Errorf("sandbox: execution cancelled: %w", ctxErr)
	}

	// Idle watchdog or absolute deadline: a timeout, not a demonstration. The
	// runtime may not have torn the container down in time; reap it by name to
	// honor the always-clean-up guarantee.
	if idleKilled.Load() || errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
		s.forceRemove(p.containerName)
		return res, nil
	}

	// Anything else (binary missing, failed to start, unexpected signal).
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

// progressSnapshot is a language-agnostic activity fingerprint of a running
// sandbox execution. It is comparable; ANY field change between successive
// samples counts as progress and resets the idle watchdog's clock.
type progressSnapshot struct {
	outputBytes  int64 // total bytes written to stdout+stderr (incl. discarded over cap)
	fsSize       int64 // sum of regular-file sizes under the workspace
	fsCount      int64 // number of entries under the workspace
	fsMaxModNano int64 // newest mtime under the workspace, unix nanoseconds
}

// idlePollInterval derives how often the watchdog samples progress from the
// idle window: frequent enough to notice a stall promptly, but bounded so the
// workspace walk stays cheap. Clamped to [1s, 30s].
func idlePollInterval(idleTimeout time.Duration) time.Duration {
	d := idleTimeout / 4
	if d < time.Second {
		d = time.Second
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// watchIdle samples progress every pollEvery and cancels the run once no
// progress has occurred for idleTimeout. fingerprint is the cheap signal
// (output bytes + workspace filesystem state); activeFallback is consulted ONLY
// when the fingerprint is unchanged, so its cost (a container-CPU probe) is paid
// just on otherwise-idle ticks. It sets killed BEFORE calling cancel so the flag
// is visible (through the atomic barrier) by the time the cancelled command
// returns. It returns when the run finishes (done closed) or after it fires;
// idleTimeout <= 0 disables it. activeFallback may be nil.
func watchIdle(done <-chan struct{}, fingerprint func() progressSnapshot, activeFallback func() bool, idleTimeout, pollEvery time.Duration, killed *atomic.Bool, cancel func()) {
	if idleTimeout <= 0 {
		return
	}
	last := fingerprint()
	lastChange := time.Now()
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			cur := fingerprint()
			if cur != last {
				last = cur
				lastChange = now
				continue
			}
			// Cheap signals flat: consult the costlier fallback before deciding.
			if activeFallback != nil && activeFallback() {
				lastChange = now
				continue
			}
			if now.Sub(lastChange) >= idleTimeout {
				killed.Store(true)
				cancel()
				return
			}
		}
	}
}

// cpuBusyThreshold is the container CPU percentage above which a run counts as
// making progress even when it writes nothing — e.g. a compiler working on one
// large generated source file. Below it, CPU is treated as idle.
const cpuBusyThreshold = 1.0

// cpuProbeTimeout bounds a single CPU probe; `stats --no-stream` samples for
// about a second, so this stays generous but finite.
const cpuProbeTimeout = 5 * time.Second

// containerCPUBusy reports whether the named container is currently consuming
// CPU above cpuBusyThreshold. It is a BEST-EFFORT progress signal: any failure
// (runtime quirk, container already gone, unparsable output) returns false so
// the watchdog falls back to the output/filesystem signals. It can only PREVENT
// a false idle-kill, never cause one. `--format {{.CPUPerc}}` (e.g. "12.34%")
// is supported by both podman and docker.
func (s *CLI) containerCPUBusy(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), cpuProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.runtime, "stats", "--no-stream", "--format", "{{.CPUPerc}}", name).Output()
	if err != nil {
		return false
	}
	field := strings.TrimSuffix(strings.TrimSpace(string(out)), "%")
	pct, err := strconv.ParseFloat(field, 64)
	if err != nil {
		return false
	}
	return pct > cpuBusyThreshold
}

// workspaceProgress returns an aggregate fingerprint of dir: total regular-file
// byte count, entry count, and newest mtime (unix nanos). It is best-effort
// (unreadable entries are skipped) and walks the whole tree so it captures
// activity anywhere a build/test writes — caches, compiled artifacts, generated
// files — independent of language. Cost is bounded by the poll interval; the
// workspace is a single repo copy.
func workspaceProgress(dir string) (size, count, maxModNano int64) {
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		count++
		if d.Type().IsRegular() {
			size += info.Size()
		}
		if m := info.ModTime().UnixNano(); m > maxModNano {
			maxModNano = m
		}
		return nil
	})
	return
}

var _ Sandbox = (*CLI)(nil)

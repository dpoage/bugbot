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
	// defaultScratchSizeMB is the size (MB) of the writable /tmp tmpfs
	// scratch space (bugbot-yrox). <= 0 is treated as unset and falls back
	// to fallbackScratchSizeMB (the package constant) in buildRunArgs.
	defaultScratchSizeMB int
	// defaultGrowthCeilingBytes bounds NET workspace growth (the fsSize
	// delta, not cumulative bytes written — see workspaceProgress) since a
	// run starts, tolerated by the shared idle watchdog before
	// killing the run with the distinct Result.WorkspaceQuotaExceeded reason
	// (bugbot-bdqf), independent of idle-stall detection. <= 0 disables the
	// ceiling.
	defaultGrowthCeilingBytes int64
	// defaultFileCountCeiling bounds NET workspace entry-COUNT growth (the
	// fsCount delta, from the SAME per-tick workspaceProgress walk fsSize
	// uses — no second WalkDir) since a run starts, tolerated by the shared
	// idle watchdog before killing the run with the distinct
	// Result.WorkspaceFileCountExceeded reason (bugbot-gb3o), independent of
	// both idle-stall detection AND the byte-size growth ceiling above: a
	// workload that creates many near-zero-byte files (the motivating case —
	// 10,000 files totaling 20 KB) never trips a byte ceiling while still
	// exhausting real host inodes/dentries. <= 0 disables the ceiling.
	defaultFileCountCeiling int64
	// wsCache is the pristine-materialization cache backing prepareWorkspace.
	// Zero value is ready to use; see wsCache's doc comment.
	wsCache wsCache
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

// WithScratchSizeMB sets the size (MB) of the writable /tmp tmpfs scratch
// space (sandbox.scratch_size_mb, bugbot-yrox). Values <= 0 fall back to
// fallbackScratchSizeMB.
func WithScratchSizeMB(mb int) Option { return func(s *CLI) { s.defaultScratchSizeMB = mb } }

// WithWorkspaceGrowthCeilingMB sets the workspace-growth ceiling (MB of NET
// workspace-size growth, not cumulative bytes written) the shared idle watchdog
// enforces independent of idle-stall detection (sandbox.
// workspace_growth_ceiling_mb, bugbot-bdqf): a run whose workspace grows
// past this is killed with Result.WorkspaceQuotaExceeded, regardless of
// whether it is otherwise "making progress" by the idle-stall definition.
// <= 0 disables the ceiling entirely.
func WithWorkspaceGrowthCeilingMB(mb int) Option {
	return func(s *CLI) { s.defaultGrowthCeilingBytes = int64(mb) * 1024 * 1024 }
}

// WithWorkspaceFileCountCeiling sets the workspace file-COUNT ceiling (NET
// entry-count growth, not absolute count) the shared idle watchdog enforces
// independent of both idle-stall detection and the byte-size growth ceiling
// above (sandbox.workspace_file_count_ceiling, bugbot-gb3o): a run whose
// workspace entry count grows past this is killed with
// Result.WorkspaceFileCountExceeded, regardless of whether it is otherwise
// "making progress" by the idle-stall definition or under the byte-size
// ceiling (many tiny files can exhaust host inodes/dentries while adding
// almost no bytes). <= 0 disables the ceiling entirely.
func WithWorkspaceFileCountCeiling(n int) Option {
	return func(s *CLI) { s.defaultFileCountCeiling = int64(n) }
}

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
		runtime:                   runtime,
		defaultImage:              image,
		defaultCPUs:               2,
		defaultMemory:             2048,
		defaultTimeout:            10 * time.Minute,
		defaultNetwork:            "none",
		pidsLimit:                 256,
		maxOutputBytes:            DefaultMaxOutputBytes,
		defaultScratchSizeMB:      fallbackScratchSizeMB,
		defaultGrowthCeilingBytes: defaultWorkspaceGrowthCeilingBytes,
		defaultFileCountCeiling:   defaultWorkspaceFileCountCeiling,
	}
	for _, o := range opts {
		o(s)
	}
	// Best-effort hygiene: purge any workspace-cache parent dirs a previous,
	// non-Closed CLI instance (or a crashed process) left behind. See
	// purgeStaleWorkspaceCaches.
	purgeStaleWorkspaceCaches()
	return s, nil
}

// Close removes this CLI instance's workspace-cache parent directory (see
// wsCache), if one was ever materialized. wsCache's mutex still guards
// concurrent access, so Close atomically claims the parent-dir path before
// removing it; an Exec racing Close afterward simply re-lazily-inits a fresh
// cache dir. That mutex protects the CACHE STATE ONLY (which pristine is
// current, where the parent dir lives) — it says nothing about an in-flight
// Exec's OWN per-run workspace clone, which Close never touches. Safety for a
// concurrent Exec instead comes from construction-site scoping: a caller that
// defers Close only does so after every Exec using that *CLI has returned, so
// Close and Close-observing Execs are never concurrent by construction, not
// because the mutex would arbitrate a race if they were. Safe to call on a
// nil receiver and multiple times.
//
// Callers that hold a *CLI across a natural scope (a single command's RunE, a
// function that both builds and exhausts the sandbox) should defer Close.
// Where no such scope exists (e.g. a sandbox handed off to a longer-lived
// consumer), the 24h purge in NewCLI is the backstop.
func (s *CLI) Close() error {
	if s == nil {
		return nil
	}
	return s.wsCache.close()
}

// MaterializeWorkspace clones the pristine-workspace cache for repoDir (see
// wsCache) into a fresh, caller-owned workspace directory and returns its
// path with no files written into it beyond the clone itself. It is the
// public seam behind Spec.Workspace: a caller that wants to write into and
// run repeated Execs against ONE persistent workspace (e.g. the reproducer's
// workspace exec tool, iterating across several sandbox runs before committing to
// a final plan) materializes it once here, then passes the returned path as
// Spec.Workspace on each Exec instead of letting Exec copy a fresh one every
// time.
//
// The caller owns the returned directory's entire lifecycle: MaterializeWorkspace
// applies no WriteFiles and Exec(Workspace: ...) never removes it, so the
// caller MUST os.RemoveAll it when done (typically via defer at the scope that
// bounds all the iteration's Execs).
func (s *CLI) MaterializeWorkspace(repoDir string) (string, error) {
	ws, _, err := s.prepareWorkspace(repoDir, nil)
	return ws, err
}

// staleWorkspaceCacheAge bounds how long an orphaned workspace-cache parent
// dir (see wsCache, purgeStaleWorkspaceCaches) is allowed to linger in the OS
// temp dir before a later process reclaims it.
const staleWorkspaceCacheAge = 24 * time.Hour

// purgeStaleWorkspaceCaches best-effort removes bugbot-wscache-* directories
// in the OS temp dir older than staleWorkspaceCacheAge. It exists because not
// every *CLI construction site has a natural defer-Close scope (see Close's
// doc comment), and any process crash bypasses a deferred Close regardless —
// so this purge, run once per NewCLI, is the backstop that actually bounds
// disk usage. All failures are swallowed: this is hygiene, never a reason to
// fail sandbox construction.
func purgeStaleWorkspaceCaches() {
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "bugbot-wscache-*"))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleWorkspaceCacheAge)
	for _, dir := range matches {
		info, statErr := os.Stat(dir)
		if statErr != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(dir)
	}
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

// ScratchAndGrowthCeiling returns the effective /tmp tmpfs scratch size (MB)
// and workspace-growth ceiling (bytes) the backend applies when a Spec
// doesn't override them, mirroring Limits' "confirm config reached the
// backend" purpose — including the explicit-zero-disables case
// (bugbot-bdqf/bugbot-yrox): sandbox.workspace_growth_ceiling_mb: 0 must be
// observable as a truly disabled (0) ceiling here, not the backend's own
// non-zero built-in default.
func (s *CLI) ScratchAndGrowthCeiling() (scratchSizeMB int, growthCeilingBytes int64) {
	return s.defaultScratchSizeMB, s.defaultGrowthCeilingBytes
}

// FileCountCeiling returns the effective workspace file-count ceiling (net
// entry-count growth, in files) the backend applies when a Spec doesn't
// override it, mirroring ScratchAndGrowthCeiling's "confirm config reached
// the backend" purpose — including the explicit-zero-disables case
// (bugbot-gb3o): sandbox.workspace_file_count_ceiling: 0 must be observable
// as a truly disabled (0) ceiling here, not the backend's own non-zero
// built-in default. Kept as its own accessor rather than folded into
// ScratchAndGrowthCeiling's signature so existing callers of that method
// (elsewhere in the tree) are unaffected by this additive knob.
func (s *CLI) FileCountCeiling() int64 {
	return s.defaultFileCountCeiling
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
		image:         s.defaultImage,
		network:       s.defaultNetwork,
		cpus:          s.defaultCPUs,
		memoryMB:      s.defaultMemory,
		pidsLimit:     s.pidsLimit,
		scratchSizeMB: s.defaultScratchSizeMB,
		env:           spec.Env,
		cmd:           spec.Cmd,
		roMounts:      spec.ROMounts,
		rwMounts:      spec.RWMounts,
		setupCmds:     spec.SetupCmds,
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
	capturePaths, err := sanitizeCapturePaths(spec.CaptureFiles)
	if err != nil {
		return Result{}, err
	}

	prepStart := time.Now()
	var ws string
	var cacheHit bool
	if spec.Workspace != "" {
		// Caller-owned iteration workspace (see Spec.Workspace doc): skip the
		// fresh-copy/pristine-cache path entirely and apply WriteFiles directly
		// onto the given directory. No defer RemoveAll — lifecycle is the
		// caller's, not ours.
		//
		// Require an absolute path: Workspace is trusted verbatim (see the
		// Spec doc's TRUST note) as a directory this process itself created,
		// which is always an absolute path (MaterializeWorkspace returns one).
		// A relative path would resolve against the CLI process's current
		// working directory instead of the caller's intended location — an
		// easy-to-miss caller bug that this guard turns into an immediate,
		// unambiguous error instead of a silent wrong-directory write.
		if !filepath.IsAbs(spec.Workspace) {
			return Result{}, fmt.Errorf("sandbox: workspace %q must be an absolute path", spec.Workspace)
		}
		info, statErr := os.Stat(spec.Workspace)
		if statErr != nil {
			return Result{}, fmt.Errorf("sandbox: stat workspace %q: %w", spec.Workspace, statErr)
		}
		if !info.IsDir() {
			return Result{}, fmt.Errorf("sandbox: workspace %q is not a directory", spec.Workspace)
		}
		ws = spec.Workspace
		if err := applyWriteFiles(ws, spec.WriteFiles); err != nil {
			return Result{}, err
		}
	} else {
		var err error
		ws, cacheHit, err = s.prepareWorkspace(spec.RepoDir, spec.WriteFiles)
		if err != nil {
			return Result{}, err
		}
		defer func() { _ = os.RemoveAll(ws) }()
	}
	prepDuration := time.Since(prepStart)

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
	//
	// Independently, a workspace-GROWTH ceiling (bugbot-bdqf, byte size) and
	// a workspace FILE-COUNT ceiling (bugbot-gb3o) bound NET growth in
	// workspace size and entry count, respectively, since the run started
	// (fsSize/fsCount; a write-then-delete churn nets out and never trips
	// either): a process that only fills disk — with either many bytes or
	// many tiny files — resets the idle clock forever under the progress
	// definition above and would otherwise run undetected until the
	// absolute Timeout. watchIdle checks BOTH ceilings on the SAME per-tick
	// workspaceProgress call the fingerprint below already makes — no extra
	// filesystem walk for either — and kills with the distinct
	// Result.WorkspaceQuotaExceeded / Result.WorkspaceFileCountExceeded
	// reason (never plain TimedOut) when the respective ceiling is
	// exceeded, regardless of whether output/CPU activity would otherwise
	// read as "progress". base is captured HERE (once, before the command
	// starts) rather than inside the goroutine so Exec's post-run
	// checkGrowthCeiling call below shares the EXACT same baseline the tick
	// loop uses — see checkGrowthCeiling's doc for why that final check
	// exists.
	var idleKilled atomic.Bool
	var quotaExceeded atomic.Bool
	var fileCountExceeded atomic.Bool
	done := make(chan struct{})
	var fingerprint func() progressSnapshot
	var growthBase progressSnapshot
	if idleTimeout > 0 || s.defaultGrowthCeilingBytes > 0 || s.defaultFileCountCeiling > 0 {
		fingerprint = func() progressSnapshot {
			ps := progressSnapshot{outputBytes: stdout.written() + stderr.written()}
			ps.fsSize, ps.fsCount, ps.fsMaxModNano = workspaceProgress(ws)
			return ps
		}
		growthBase = fingerprint()
		active := func() bool { return s.containerCPUBusy(p.containerName) }
		limits := watchdogLimits{
			idleTimeout:        idleTimeout,
			growthCeilingBytes: s.defaultGrowthCeilingBytes,
			fileCountCeiling:   s.defaultFileCountCeiling,
		}
		go watchIdle(watchdogArgs{
			done:              done,
			fingerprint:       fingerprint,
			activeFallback:    active,
			limits:            limits,
			base:              growthBase,
			pollEvery:         effectivePollInterval(idleTimeout, s.defaultGrowthCeilingBytes, s.defaultFileCountCeiling),
			killed:            &idleKilled,
			quotaExceeded:     &quotaExceeded,
			fileCountExceeded: &fileCountExceeded,
			cancel:            cancel,
		})
	}

	start := time.Now()
	runErr := cmd.Run()
	close(done)
	duration := time.Since(start)

	// Post-run growth check (bugbot-bdqf oracle review B1b; extended to the
	// file-count ceiling by bugbot-gb3o). See checkGrowthCeiling's doc.
	// Must run BEFORE the outcome-precedence branches below — a breach is a
	// hard invariant, not a race heuristic, so it is never allowed to lose
	// to a "genuine" exit code the way an idle-stall kill legitimately can.
	checkGrowthCeiling(fingerprint, growthBase, s.defaultGrowthCeilingBytes, s.defaultFileCountCeiling, &quotaExceeded, &fileCountExceeded)

	res := Result{Duration: duration, PrepDuration: prepDuration, WorkspaceCacheHit: cacheHit}
	res.Stdout, res.StdoutTruncated = stdout.result()
	res.Stderr, res.StderrTruncated = stderr.result()
	res.Captured = captureWorkspaceFiles(ws, capturePaths, s.maxOutputBytes)

	// Caller cancellation takes ABSOLUTE priority, checked FIRST, ahead of
	// EVERY other outcome signal (growth-ceiling breach, exit code, or
	// infra timeout) — bugbot-bdqf oracle review, cancellation precedence.
	// checkGrowthCeiling above already ran unconditionally (its cost is
	// paid either way), but a caller cancel landing in the same window as
	// a breach — or even a clean exit — must always surface as the
	// documented "sandbox: execution cancelled" error, never silently
	// reinterpreted as a quota kill or a stale success: the caller no
	// longer wants this result at all, regardless of what our own
	// machinery observed.
	if ctxErr := ctx.Err(); ctxErr != nil {
		s.forceRemove(p.containerName)
		return res, fmt.Errorf("sandbox: execution cancelled: %w", ctxErr)
	}

	// Outcome precedence. A growth-ceiling breach (size OR file-count)
	// ALWAYS wins over the process's own reported outcome (bugbot-bdqf
	// oracle review B1b) — see checkGrowthCeiling's doc for why this does
	// not follow the "genuine exit code wins over a racing watchdog" rule
	// below. Size is checked first, matching the tick loop's own check
	// order in watchIdle; a run that (improbably) breaches both in the
	// same window reports WorkspaceQuotaExceeded, which is still a
	// correct, InfraKilled()-true classification. This quota-first
	// precedence CAN report the byte reason even when the file-count
	// ceiling was the one that actually cancelled the run: if a tick
	// catches the count breach (setting ONLY fileCountExceeded) but the
	// process keeps writing bytes for a brief window before it actually
	// dies from the resulting cancel(), and checkGrowthCeiling's post-run
	// resample still had a reason to run (e.g. a burst that finished
	// before any tick fired, so BOTH thresholds are found breached in
	// that single resample), both flags can end up true — quota still
	// wins here, by design, not a bug (A-N2 fix-round note). This is now
	// rarer than it once was: checkGrowthCeiling skips its resample
	// entirely once EITHER flag is already set (see its doc), so the
	// common "tick caught count, would-be resample also finds bytes"
	// case no longer happens — only a genuine simultaneous burst can
	// still trigger it.
	if quotaExceeded.Load() {
		res.WorkspaceQuotaExceeded = true
		res.ExitCode = -1
		s.forceRemove(p.containerName)
		return res, nil
	}
	if fileCountExceeded.Load() {
		res.WorkspaceFileCountExceeded = true
		res.ExitCode = -1
		s.forceRemove(p.containerName)
		return res, nil
	}

	// A process that returned its OWN status — a clean exit or a real
	// non-zero code — was not killed by us, so those win next: an idle
	// watchdog (or absolute deadline) firing in the same instant can never
	// mask a genuine repro verdict. Our kills surface as a signal
	// (ExitCode -1) and fall through to the timeout branch below.
	if runErr == nil {
		res.ExitCode = 0
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && exitErr.ExitCode() >= 0 {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}

	// Idle watchdog or absolute deadline: a timeout, not a demonstration.
	// The runtime may not have torn the container down in time; reap it by
	// name to honor the always-clean-up guarantee. quotaExceeded was
	// already handled above, so reaching here means a plain idle-stall (or
	// absolute-deadline) kill.
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

// defaultWorkspaceGrowthCeilingBytes bounds cumulative workspace growth
// (the NET size delta since a run started, sampled via workspaceProgress —
// write-then-delete churn nets out and never trips it) before the shared
// idle watchdog kills the run with the distinct Result.WorkspaceQuotaExceeded
// reason (bugbot-bdqf), when no operator override
// (sandbox.workspace_growth_ceiling_mb) is configured. Deliberately
// generous — this exists to catch a runaway/malicious disk-filler, not to
// constrain a legitimate build's disk usage (a full toolchain build plus
// test artifacts can easily reach several hundred MB); 2 GiB comfortably
// clears that bar while still bounding an unbounded write loop's blast
// radius well short of exhausting a typical CI/dev host's disk.
const defaultWorkspaceGrowthCeilingBytes int64 = 2 * 1024 * 1024 * 1024 // 2 GiB

// defaultWorkspaceFileCountCeiling bounds cumulative workspace entry-COUNT
// growth (the NET fsCount delta since a run started, sampled via the SAME
// workspaceProgress walk fsSize uses) before the shared idle watchdog kills
// the run with the distinct Result.WorkspaceFileCountExceeded reason
// (bugbot-gb3o), when no operator override
// (sandbox.workspace_file_count_ceiling) is configured. Deliberately
// generous — this exists to catch a runaway/malicious many-tiny-files
// generator (the motivating case: 10,000 files totaling 20 KB, which never
// trips the 2 GiB byte ceiling above while still taking 10k inodes/dentries
// of host pressure), not to constrain a legitimate build's file count. It
// is NOT "far under" every real workload, though: oracle-measured real
// installs (fix round A-B1/B-N2) found 80,589 entries for a 1,819-package
// npm tree and 184,935 (92.5% of this 200,000 default) for a small
// 3-package monorepo's node_modules — this ceiling can sit uncomfortably
// close to a legitimate large JS install. What actually protects those
// workloads in practice is the SIBLING byte-size ceiling above: across
// seven real workloads measured (npm ci at multiple scales, a Go build
// with GOCACHE+GOMODCACHE on the workspace, a Python wheelhouse), byte-%
// of its 2 GiB ceiling exceeded file-% of this 200,000 ceiling in EVERY
// case, so workspace_growth_ceiling_mb binds first on real builds and
// this default rarely becomes the first thing to fire — but an operator
// running an unusually file-count-heavy, byte-light install (e.g. a
// monorepo with many small packages) should size this knob explicitly
// rather than trust "generous" alone.
//
// Overshoot past whichever ceiling fires first is RATE-bounded, not
// count-bounded (mirrors growthPollInterval's byte-overshoot rationale
// below): a tick can only observe growth once per second, so a workload
// creating files fast enough overshoots by however many it creates in
// that window. Measured worst cases: 2,003 files past this 200,000
// default (~17k files/s), 20,826 past a smaller test ceiling (~46k
// files/s), and up to ~5,400 extra on podman specifically from its
// post-cancel teardown window (the container keeps writing briefly after
// SIGKILL is sent). An operator setting a SMALL ceiling gets a
// proportionally LARGE overshoot relative to their own limit — size
// accordingly for a fast-writing workload, not just a slow one.
const defaultWorkspaceFileCountCeiling int64 = 200_000

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

// growthPollInterval is the sampling cadence for the workspace growth
// ceilings — byte-size (bugbot-bdqf) AND file-count (bugbot-gb3o), both
// share this one cadence — fixed and independent of idleTimeout — see
// effectivePollInterval for why it must NOT be derived from
// idlePollInterval.
//
// Deriving the growth-check cadence from idlePollInterval(idleTimeout) let a
// breach overshoot the ceiling by tens of GB under the SHIPPED DEFAULT
// (idle_timeout_seconds: 120 -> idlePollInterval = 30s; a host writing at
// ~2 GB/s overshoots the default 2 GiB ceiling by ~4-60 GiB inside one
// window before the tick that would have caught it — oracle-measured
// 72x-1024x overshoot in review). The same overshoot risk applies to the
// file-count ceiling: a tight loop can create tens of thousands of tiny
// files per second, so a loose poll cadence would let a count breach
// overshoot by a similar multiple. 1s is a DELIBERATE, hardcoded constant —
// not idlePollInterval(0)'s [1s,30s]-clamped floor, which is an ACCIDENT of
// a disabled idle window, not a chosen cadence for this knob. It is tight
// enough to bound worst-case overshoot at multi-GB/s NVMe throughput (or
// tens-of-thousands-of-files/s creation rates) to a small multiple of
// either default ceiling, and cheap enough that one extra workspaceProgress
// walk per second is negligible next to the disk I/O it bounds (a
// warm-cache walk over 50k files measured ~76ms — well under the 1s
// budget).
const growthPollInterval = 1 * time.Second

// effectivePollInterval derives watchIdle's actual sampling cadence: the
// TIGHTER of idlePollInterval(idleTimeout) (the existing idle-stall
// cadence) and growthPollInterval, whenever EITHER growth ceiling (byte-size
// or file-count) is active — so growth detection is never slower than its
// own dedicated cadence just because the operator's idle-stall window
// happens to be long (or disabled). When idleTimeout <= 0,
// idlePollInterval(0)'s floor is an artifact of a DISABLED window, never a
// deliberate growth-sampling choice, so growthPollInterval alone governs in
// that case. When BOTH growth ceilings are disabled (growthCeilingBytes<=0
// AND fileCountCeiling<=0), this returns EXACTLY idlePollInterval(idleTimeout),
// preserving byte-identical behavior for idle-only configurations
// (including the pre-existing 1s floor at idleTimeout<=0 when no ceiling is
// configured at all).
func effectivePollInterval(idleTimeout time.Duration, growthCeilingBytes, fileCountCeiling int64) time.Duration {
	if growthCeilingBytes <= 0 && fileCountCeiling <= 0 {
		return idlePollInterval(idleTimeout)
	}
	if idleTimeout <= 0 {
		return growthPollInterval
	}
	if pollEvery := idlePollInterval(idleTimeout); pollEvery < growthPollInterval {
		return pollEvery
	}
	return growthPollInterval
}

// watchdogLimits bundles watchIdle's three independent kill conditions so
// its parameter list doesn't grow unbounded as more are added:
//   - idleTimeout: kill after this long with NO observable progress (the
//     original idle-stall detector). <= 0 disables it.
//   - growthCeilingBytes: kill as soon as the workspace has grown by more
//     than this many bytes since watchIdle started sampling, REGARDLESS of
//     whether the fingerprint otherwise reads as "making progress"
//     (bugbot-bdqf) — a process that only fills disk resets the idle clock
//     forever under the plain progress definition, so this check runs
//     independently of it. <= 0 disables it.
//   - fileCountCeiling: kill as soon as the workspace's ENTRY COUNT has
//     grown by more than this many entries since watchIdle started
//     sampling, independently of both idleTimeout and growthCeilingBytes
//     (bugbot-gb3o) — a workload that creates many near-zero-byte files
//     never trips growthCeilingBytes while still exhausting host
//     inodes/dentries. <= 0 disables it.
//
// At least one must be positive for watchIdle to do anything; all three may
// be active simultaneously (whichever fires first wins).
type watchdogLimits struct {
	idleTimeout        time.Duration
	growthCeilingBytes int64
	fileCountCeiling   int64
}

// watchdogArgs bundles every input watchIdle needs. Introduced (alongside
// effectivePollInterval and checkGrowthCeiling) when an oracle round
// required a growth-ceiling baseline shared between the tick loop AND
// Exec's post-run check (see checkGrowthCeiling's doc) — bundling avoids
// the parameter list growing without bound as future kill conditions are
// added.
type watchdogArgs struct {
	done <-chan struct{}
	// fingerprint is the cheap per-tick progress/growth signal (output
	// bytes + workspace filesystem state).
	fingerprint func() progressSnapshot
	// activeFallback is consulted ONLY when fingerprint is unchanged AND
	// neither growth ceiling is implicated, so its cost (a container-CPU
	// probe) is paid just on otherwise-idle ticks. May be nil.
	activeFallback func() bool
	limits         watchdogLimits
	// base is the fingerprint taken once, before the run starts (by the
	// caller — see checkGrowthCeiling), and is the SAME baseline both the
	// tick loop's growth checks and Exec's post-run growth checks measure
	// against, so the two never disagree about what counts as "growth
	// since the run started".
	base      progressSnapshot
	pollEvery time.Duration
	// killed is set ONLY on a plain idle-stall kill (never on a
	// growth-ceiling kill — see quotaExceeded/fileCountExceeded).
	killed *atomic.Bool
	// quotaExceeded is set on a byte-size growth-ceiling kill, whether
	// detected here (a tick observes the breach and cancels the run) or by
	// Exec's post-run checkGrowthCeiling call (the run exited on its own
	// before any tick could observe the breach).
	quotaExceeded *atomic.Bool
	// fileCountExceeded is fileCountCeiling's analogue of quotaExceeded
	// (bugbot-gb3o): set on a file-count growth-ceiling kill, whether
	// detected here or by the post-run checkGrowthCeiling call.
	fileCountExceeded *atomic.Bool
	cancel            func()
}

// watchIdle samples progress every a.pollEvery and cancels the run when any
// of a.limits' three independent conditions trips: no progress for
// a.limits.idleTimeout, NET workspace byte growth past
// a.limits.growthCeilingBytes, or NET workspace entry-count growth past
// a.limits.fileCountCeiling — both measured since a.base. a.fingerprint is
// the cheap signal; a.activeFallback is consulted ONLY when the fingerprint
// is unchanged AND neither growth ceiling is implicated, so its cost (a
// container-CPU probe) is paid just on otherwise-idle ticks. Both growth
// checks reuse the SAME per-tick a.fingerprint() call the idle-stall check
// already makes (all three derive from one workspaceProgress walk) rather
// than sampling the filesystem twice.
//
// On a byte-size breach it sets a.quotaExceeded; on a file-count breach it
// sets a.fileCountExceeded (NEVER a.killed either way — that flag is
// reserved for a plain idle-stall kill, see watchdogArgs) and calls
// a.cancel; on an idle-stall timeout it sets a.killed and calls a.cancel.
// Whichever flag applies is set BEFORE cancel runs, so it is visible
// (through the atomic barrier) by the time the cancelled command returns.
// It returns when the run finishes (a.done closed) or after it fires; when
// all three of a.limits' fields are <= 0 it returns immediately without
// sampling. This is the PROACTIVE half of growth-ceiling enforcement — a
// run that breaches a ceiling and exits before the next tick escapes this
// loop entirely; Exec's unconditional post-run checkGrowthCeiling call is
// what catches that case (bugbot-bdqf oracle review B1b; extended to
// file-count by bugbot-gb3o).
func watchIdle(a watchdogArgs) {
	if a.limits.idleTimeout <= 0 && a.limits.growthCeilingBytes <= 0 && a.limits.fileCountCeiling <= 0 {
		return
	}
	last := a.base
	lastChange := time.Now()
	t := time.NewTicker(a.pollEvery)
	defer t.Stop()
	for {
		select {
		case <-a.done:
			return
		case now := <-t.C:
			cur := a.fingerprint()

			// Both growth ceilings are checked FIRST and independent of the
			// idle-stall logic below: unlike output/CPU activity, ongoing
			// workspace growth (bytes or file count) must never be treated
			// as a reason to let the run continue — that is exactly the
			// disk-filler / inode-exhaustion behavior these ceilings exist
			// to catch (bugbot-bdqf, bugbot-gb3o).
			if a.limits.growthCeilingBytes > 0 && cur.fsSize-a.base.fsSize > a.limits.growthCeilingBytes {
				a.quotaExceeded.Store(true)
				a.cancel()
				return
			}
			if a.limits.fileCountCeiling > 0 && cur.fsCount-a.base.fsCount > a.limits.fileCountCeiling {
				a.fileCountExceeded.Store(true)
				a.cancel()
				return
			}

			if a.limits.idleTimeout <= 0 {
				continue
			}
			if cur != last {
				last = cur
				lastChange = now
				continue
			}
			// Cheap signals flat: consult the costlier fallback before deciding.
			if a.activeFallback != nil && a.activeFallback() {
				lastChange = now
				continue
			}
			if now.Sub(lastChange) >= a.limits.idleTimeout {
				a.killed.Store(true)
				a.cancel()
				return
			}
		}
	}
}

// checkGrowthCeiling performs the DEFINITIVE post-run workspace-growth
// check (bugbot-bdqf oracle review B1b; extended to the file-count ceiling
// by bugbot-gb3o). watchIdle's tick loop only evaluates growth periodically
// (every effectivePollInterval); a run that breaches a ceiling and exits
// before the NEXT tick fires — a single large burst write (or file-create
// burst), or any process fast enough to finish inside one poll window —
// would otherwise escape classification entirely, regardless of its own
// exit code. Exec calls this exactly once, unconditionally, immediately
// after the command exits (success, failure, or signal) and BEFORE
// consulting runErr — growth-ceiling enforcement is a measured, absolute
// invariant on final disk usage, not a race-prone liveness heuristic like
// idle-stall detection, so it does NOT participate in the "a genuine exit
// code wins over a racing watchdog" precedence rule Exec applies to
// TimedOut: a breach always overrides whatever exit code the process
// itself reported.
//
// fingerprint may be nil (when neither idleTimeout nor either growth
// ceiling was configured, Exec never allocates one); both ceilings<=0 is
// checked FIRST so a nil fingerprint is never dereferenced in that case.
// A no-op, without sampling the filesystem again, once EITHER flag is
// already true — a tick that caught ONE breach and cancelled the run
// already fully determines Exec's outcome (Exec's precedence checks
// quotaExceeded before fileCountExceeded and returns on the first true
// one, so the other ceiling's status can never change what gets
// reported), so resampling to also check the OTHER, still-unsettled
// ceiling would be a pure wasted walk in the shipped-default shape where
// BOTH ceilings are non-zero simultaneously (A-N1 fix round: the
// previous per-ceiling sizeSettled/countSettled check only skipped when
// BOTH were settled, so a tick-caught breach with the sibling ceiling
// still enabled — the actual shipped default — silently triggered a
// redundant third filesystem walk per run; see
// TestCheckGrowthCeiling_AlreadyExceededSkipsResample_BothCeilingsShippedShape).
func checkGrowthCeiling(fingerprint func() progressSnapshot, base progressSnapshot, growthCeilingBytes, fileCountCeiling int64, quotaExceeded, fileCountExceeded *atomic.Bool) {
	if growthCeilingBytes <= 0 && fileCountCeiling <= 0 {
		return
	}
	if quotaExceeded.Load() || fileCountExceeded.Load() {
		return
	}
	final := fingerprint()
	if growthCeilingBytes > 0 && final.fsSize-base.fsSize > growthCeilingBytes {
		quotaExceeded.Store(true)
	}
	if fileCountCeiling > 0 && final.fsCount-base.fsCount > fileCountCeiling {
		fileCountExceeded.Store(true)
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

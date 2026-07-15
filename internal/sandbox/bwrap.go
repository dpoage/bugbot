package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"
)

// bwrapProbeTimeout bounds DetectBwrap's userns capability probe. bwrap with
// a trivial argv (running /bin/true inside an unshared user namespace) exits
// almost instantly; this stays generous without letting a wedged host hang
// detection.
const bwrapProbeTimeout = 5 * time.Second

// DetectBwrap reports whether the bwrap backend is usable on this host and,
// when it is not, an actionable reason. Three independent conditions gate
// usability, checked in the order a user would want to fix them:
//  1. the host must be Linux (bwrap depends on Linux-only namespace syscalls);
//  2. the bwrap binary must be on PATH;
//  3. unprivileged user namespaces must actually work — some distributions
//     ship bwrap but disable unprivileged userns via sysctl
//     (kernel.unprivileged_userns_clone=0) or an AppArmor profile
//     (Ubuntu 24.04's default "restrict unprivileged user namespaces"), in
//     which case bwrap is present but every real run would fail. This is
//     probed directly (attempting the actual unshare) rather than by reading
//     a specific sysctl, since the exact gate mechanism differs across
//     distributions (sysctl vs AppArmor vs SELinux) and a direct probe is
//     the one check that agrees with reality on all of them.
func DetectBwrap() (ok bool, reason string) {
	if runtime.GOOS != "linux" {
		return false, fmt.Sprintf("bwrap backend requires Linux (running on %s); use sandbox.backend: cli (podman/docker) instead", runtime.GOOS)
	}
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return false, "bwrap not found on PATH; install bubblewrap (e.g. `apt install bubblewrap` / `dnf install bubblewrap` / `pacman -S bubblewrap`)"
	}
	if err := probeBwrapUserns(path); err != nil {
		return false, fmt.Sprintf("unprivileged user namespaces are unavailable (%v); check kernel.unprivileged_userns_clone, an AppArmor userns-restriction profile, or run as a user with CAP_SYS_ADMIN — or use sandbox.backend: cli (podman/docker) instead", err)
	}
	return true, ""
}

// probeBwrapUserns attempts the smallest possible real bwrap run — unshare
// every namespace and immediately exit — to prove unprivileged user
// namespaces actually work end to end, rather than inferring it from a
// sysctl file whose name and meaning vary across distributions.
//
// The probe run needs SOME executable reachable inside the sandbox, but it
// must not assume fixedROAllowlist's paths exist: on non-FHS hosts (NixOS,
// Guix) /bin and /sbin are absent entirely, and even `true` lives under
// /nix/store or /run rather than /bin. Since this ephemeral, immediately-
// exiting process runs nothing untrusted and only exists to answer "does
// unshare(2) actually work here", binding the entire host root read-only for
// it (rather than the real run's narrow allowlist) is safe and portable: it
// works identically regardless of which distro layout the host uses.
//
// INVARIANT: the --ro-bind / / below is acceptable ONLY because this probe
// always execs a fixed, hardcoded, trusted command (`true`, resolved by
// this function itself) — it must NEVER be reused to run caller-supplied or
// model-generated code. Every other bwrap invocation in this package goes
// through buildBwrapArgs' narrow fixedROAllowlist instead; this is the one
// deliberate exception, and it must stay that way.
func probeBwrapUserns(bwrapPath string) error {
	truePath, err := exec.LookPath("true")
	if err != nil {
		// Every POSIX system ships a `true` somewhere on PATH; this is
		// belt-and-suspenders in case PATH is unusually stripped down.
		truePath = "/bin/true"
	}
	ctx, cancel := context.WithTimeout(context.Background(), bwrapProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bwrapPath, "--unshare-all", "--die-with-parent", "--ro-bind", "/", "/", truePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		if len(out) > 0 {
			return fmt.Errorf("%s: %s", err, string(out))
		}
		return err
	}
	return nil
}

// Bwrap is a Sandbox backed by the host's bubblewrap (bwrap) binary: an
// unprivileged-user-namespace sandbox that runs directly on host toolchains
// instead of a baked container image (see bwrap_command.go for the security
// posture). It is safe for concurrent use: each Exec prepares its own
// workspace and launches its own bwrap process.
//
// Spec.Image is meaningless here — there is no image to select — so it is
// ignored; ProbeCapabilities callers should key the probe cache on a
// host-toolchain fingerprint instead (see CapabilityFingerprint).
type Bwrap struct {
	bwrapPath          string
	defaultCPUs        float64
	defaultMemory      int
	defaultTimeout     time.Duration
	defaultIdleTimeout time.Duration
	defaultNetwork     string
	pidsLimit          int
	maxOutputBytes     int
	// allowUncapped permits Exec to proceed with no resource-limit
	// enforcement when neither systemd-run --user --scope nor a delegated
	// cgroup v2 subtree is available (sandbox.allow_uncapped). Default false:
	// Exec fails loudly instead of silently running uncapped.
	allowUncapped bool
	// toolchainBinds are extra read-only binds (beyond fixedROAllowlist)
	// resolved by the host-toolchain resolver, applied to every run.
	toolchainBinds []ROMount
	// toolchainPathPrepend is the PATH prefix (ResolveHostToolchains'
	// PathPrepend) for the same resolution that produced toolchainBinds;
	// see bwrapParams.toolchainPathPrepend for how it reaches buildBwrapArgs.
	toolchainPathPrepend string
	// capabilityFingerprint identifies the host-toolchain state this backend
	// was constructed against, for ProbeCapabilities cache keying — the bwrap
	// analogue of the container backend's image string.
	capabilityFingerprint string
	// wsCache is the pristine-materialization cache backing prepareWorkspace,
	// shared with the CLI backend via prepareWorkspaceCached (workspace.go).
	wsCache wsCache
}

// BwrapOption configures a Bwrap sandbox.
type BwrapOption func(*Bwrap)

// WithBwrapCPUs sets the default CPU limit applied when a Spec leaves CPUs
// unset.
func WithBwrapCPUs(c float64) BwrapOption { return func(s *Bwrap) { s.defaultCPUs = c } }

// WithBwrapMemoryMB sets the default memory limit (MB) applied when a Spec
// leaves MemoryMB unset.
func WithBwrapMemoryMB(m int) BwrapOption { return func(s *Bwrap) { s.defaultMemory = m } }

// WithBwrapTimeout sets the default execution timeout applied when a Spec
// leaves Timeout unset.
func WithBwrapTimeout(d time.Duration) BwrapOption {
	return func(s *Bwrap) { s.defaultTimeout = d }
}

// WithBwrapIdleTimeout sets the default idle (no-progress) window applied
// when a Spec leaves IdleTimeout unset. Zero disables the watchdog.
func WithBwrapIdleTimeout(d time.Duration) BwrapOption {
	return func(s *Bwrap) { s.defaultIdleTimeout = d }
}

// WithBwrapNetwork sets the default network mode ("none" or "host") applied
// when a Spec leaves Network unset.
func WithBwrapNetwork(n string) BwrapOption { return func(s *Bwrap) { s.defaultNetwork = n } }

// WithBwrapPidsLimit sets the process-count cap enforced via the resolved
// resource-limit mechanism. A value <= 0 disables the cap.
func WithBwrapPidsLimit(n int) BwrapOption { return func(s *Bwrap) { s.pidsLimit = n } }

// WithBwrapMaxOutputBytes overrides the per-stream output cap.
func WithBwrapMaxOutputBytes(n int) BwrapOption {
	return func(s *Bwrap) { s.maxOutputBytes = n }
}

// WithBwrapAllowUncapped permits Exec to run without enforced resource
// limits when no enforcement mechanism (systemd-run --user --scope or a
// delegated cgroup v2 subtree) is available on this host, instead of
// failing. Mirrors sandbox.allow_uncapped.
func WithBwrapAllowUncapped(allow bool) BwrapOption {
	return func(s *Bwrap) { s.allowUncapped = allow }
}

// WithBwrapToolchainBinds adds extra read-only binds (beyond fixedROAllowlist)
// to every run, resolved by the host-toolchain resolver.
func WithBwrapToolchainBinds(mounts []ROMount) BwrapOption {
	return func(s *Bwrap) { s.toolchainBinds = mounts }
}

// WithBwrapToolchainPath sets the PATH prefix (ResolveHostToolchains'
// PathPrepend) for the same resolution passed to WithBwrapToolchainBinds, so
// resolved toolchain binaries are actually reachable via PATH inside the
// sandbox rather than merely bind-mounted. Callers pass both options from
// the same ToolchainResolution.
func WithBwrapToolchainPath(prepend string) BwrapOption {
	return func(s *Bwrap) { s.toolchainPathPrepend = prepend }
}

// WithBwrapCapabilityFingerprint sets the cache key ProbeCapabilities should
// use for this backend's probe results, in place of an image string. Callers
// derive it from the resolved host toolchains (see the host-toolchain
// resolver's fingerprint output) so a probe result never survives a
// toolchain change it never actually observed.
func WithBwrapCapabilityFingerprint(fp string) BwrapOption {
	return func(s *Bwrap) { s.capabilityFingerprint = fp }
}

// NewBwrap constructs a Bwrap sandbox. It fails fast with the same
// actionable reasons as DetectBwrap when the backend is not usable on this
// host, so a misconfigured `sandbox.backend: bwrap` is caught at
// construction time rather than on the first real run.
func NewBwrap(opts ...BwrapOption) (*Bwrap, error) {
	ok, reason := DetectBwrap()
	if !ok {
		return nil, errors.New("sandbox: " + reason)
	}
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("sandbox: bwrap not found on PATH: %w", err)
	}
	s := &Bwrap{
		bwrapPath:      path,
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
	purgeStaleWorkspaceCaches()
	return s, nil
}

// Close removes this Bwrap instance's workspace-cache parent directory, if
// one was ever materialized. See CLI.Close's doc comment for the concurrency
// contract this mirrors; the same reasoning applies unchanged. Safe to call
// on a nil receiver and multiple times.
func (s *Bwrap) Close() error {
	if s == nil {
		return nil
	}
	return s.wsCache.close()
}

// MaterializeWorkspace clones the pristine-workspace cache for repoDir into a
// fresh, caller-owned workspace directory. See CLI.MaterializeWorkspace's
// doc comment; the contract is identical.
func (s *Bwrap) MaterializeWorkspace(repoDir string) (string, error) {
	ws, _, err := prepareWorkspaceCached(&s.wsCache, repoDir, nil)
	return ws, err
}

// CapabilityFingerprint returns the host-toolchain fingerprint this backend
// was constructed with, for ProbeCapabilities cache keying (see Spec.Image's
// role in the container backend's probe cache).
func (s *Bwrap) CapabilityFingerprint() string { return s.capabilityFingerprint }

// Limits returns the effective resource caps the backend applies to a Spec
// that does not override them, mirroring CLI.Limits.
func (s *Bwrap) Limits() (cpus float64, memoryMB, pidsLimit int) {
	return s.defaultCPUs, s.defaultMemory, s.pidsLimit
}

// resolveBwrapParams applies backend defaults to a Spec, producing the
// concrete bwrapParams for the run (workspace is filled in by Exec).
func (s *Bwrap) resolveBwrapParams(spec Spec) (bwrapParams, error) {
	network := s.defaultNetwork
	if spec.Network != "" {
		network = spec.Network
	}
	network, err := validateBwrapNetwork(network)
	if err != nil {
		return bwrapParams{}, err
	}
	return bwrapParams{
		network:              network,
		env:                  spec.Env,
		cmd:                  spec.Cmd,
		roMounts:             spec.ROMounts,
		rwMounts:             spec.RWMounts,
		setupCmds:            spec.SetupCmds,
		toolchainBinds:       s.toolchainBinds,
		toolchainPathPrepend: s.toolchainPathPrepend,
	}, nil
}

// Exec implements Sandbox. See the Sandbox interface for the error contract:
// only infrastructure failures are returned as errors; a non-zero exit code
// is reported in Result.ExitCode. Process supervision mirrors CLI.Exec
// (context/timeout/force-kill discipline) with --die-with-parent + a
// process-group kill replacing container rm — bwrap has no daemon-tracked
// object for a "docker rm -f" equivalent to reap.
func (s *Bwrap) Exec(ctx context.Context, spec Spec) (Result, error) {
	if len(spec.Cmd) == 0 {
		return Result{}, errors.New("sandbox: spec.Cmd must be non-empty")
	}
	if err := validateBwrapMounts(spec.ROMounts, spec.RWMounts); err != nil {
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
		// Caller-owned iteration workspace — identical contract to CLI.Exec's
		// Spec.Workspace handling; see its doc comment for the rationale.
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
		ws, cacheHit, err = prepareWorkspaceCached(&s.wsCache, spec.RepoDir, spec.WriteFiles)
		if err != nil {
			return Result{}, err
		}
		defer func() { _ = os.RemoveAll(ws) }()
	}
	prepDuration := time.Since(prepStart)

	p, err := s.resolveBwrapParams(spec)
	if err != nil {
		return Result{}, err
	}
	p.workspace = ws

	cpus := s.defaultCPUs
	if spec.CPUs > 0 {
		cpus = spec.CPUs
	}
	memoryMB := s.defaultMemory
	if spec.MemoryMB > 0 {
		memoryMB = spec.MemoryMB
	}

	capMethod := detectBwrapCapMethod(ctx)
	if capMethod == bwrapCapNone && !s.allowUncapped {
		return Result{}, bwrapCapError
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = s.defaultTimeout
	}
	idleTimeout := spec.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = s.defaultIdleTimeout
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bwrapArgv := buildBwrapArgs(p)
	wrap, err := s.newResourceCapWrap(capMethod, bwrapArgv, cpus, memoryMB, s.pidsLimit)
	if err != nil {
		return Result{}, err
	}
	defer wrap.cleanup()

	cmd := exec.CommandContext(runCtx, wrap.name, wrap.args...)
	setBwrapProcAttr(cmd)

	stdout := newCappedBuffer(s.maxOutputBytes)
	stderr := newCappedBuffer(s.maxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	var idleKilled atomic.Bool
	done := make(chan struct{})
	var pidForCPU atomic.Int64
	if idleTimeout > 0 {
		fingerprint := func() progressSnapshot {
			ps := progressSnapshot{outputBytes: stdout.written() + stderr.written()}
			ps.fsSize, ps.fsCount, ps.fsMaxModNano = workspaceProgress(ws)
			return ps
		}
		active := func() bool {
			pid := pidForCPU.Load()
			if pid == 0 {
				return false
			}
			return procTreeCPUBusy(int(pid))
		}
		go watchIdle(done, fingerprint, active, idleTimeout, idlePollInterval(idleTimeout), &idleKilled, cancel)
	}

	start := time.Now()
	if startErr := cmd.Start(); startErr != nil {
		close(done)
		return Result{}, fmt.Errorf("sandbox: start %s: %w", wrap.name, startErr)
	}
	if cmd.Process != nil {
		pidForCPU.Store(int64(cmd.Process.Pid))
		if joinErr := wrap.joinCgroup(cmd.Process.Pid); joinErr != nil {
			killBwrapProcessGroup(cmd)
			_ = cmd.Wait()
			close(done)
			return Result{}, joinErr
		}
	}
	runErr := cmd.Wait()
	close(done)
	duration := time.Since(start)

	res := Result{Duration: duration, PrepDuration: prepDuration, WorkspaceCacheHit: cacheHit}
	res.Stdout, res.StdoutTruncated = stdout.result()
	res.Stderr, res.StderrTruncated = stderr.result()
	res.Captured = captureWorkspaceFiles(ws, capturePaths, s.maxOutputBytes)

	if runErr == nil {
		res.ExitCode = 0
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && exitErr.ExitCode() >= 0 {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		killBwrapProcessGroup(cmd)
		return res, fmt.Errorf("sandbox: execution cancelled: %w", ctxErr)
	}

	if idleKilled.Load() || errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
		killBwrapProcessGroup(cmd)
		return res, nil
	}

	return res, fmt.Errorf("sandbox: run %s: %w", wrap.name, runErr)
}

// resourceCapWrap is the resolved (binary, args) to exec for a run's cap
// method, plus the hooks Exec drives at the right point in the process
// lifecycle: joinCgroup (called once, right after Start, with the started
// pid) and cleanup (always called via defer, whether or not the run
// succeeded).
type resourceCapWrap struct {
	name       string
	args       []string
	joinCgroup func(pid int) error
	cleanup    func()
}

// newResourceCapWrap builds the resourceCapWrap for the resolved cap method:
//   - systemd-run: the bwrap argv is wrapped in a transient --user --scope
//     unit at exec time; MemoryMax/CPUQuota/TasksMax are systemd unit
//     properties, so no post-Start action is needed and --collect lets
//     systemd reap the transient unit itself (cleanup is a no-op).
//   - cgroup v2: a delegated subtree is created with the resolved limits
//     BEFORE Start (cgroup v2 requires the controllers configured before
//     population), bwrap execs directly (no wrapper binary), and joinCgroup
//     moves the just-started pid into the subtree by writing it to
//     cgroup.procs — cgroup v2 membership propagates to every process the
//     joined pid subsequently forks/execs, so bwrap and everything it runs
//     inside the sandbox is covered. cleanup removes the (by-then-empty)
//     subtree.
//   - none (only reachable with allowUncapped): bwrap execs directly with
//     no wrapper and no cap enforcement.
func (s *Bwrap) newResourceCapWrap(method bwrapCapMethod, bwrapArgv []string, cpus float64, memoryMB, pidsLimit int) (resourceCapWrap, error) {
	noop := func(int) error { return nil }
	switch method {
	case bwrapCapSystemdRun:
		full := systemdRunWrapArgs(s.bwrapPath, bwrapArgv, cpus, memoryMB, pidsLimit)
		return resourceCapWrap{name: full[0], args: full[1:], joinCgroup: noop, cleanup: func() {}}, nil
	case bwrapCapCgroupV2:
		parent, ok := delegatedCgroupV2Dir()
		if !ok {
			// Lost the delegated subtree between detection and use (e.g. a
			// concurrent process reconfigured cgroups); fail rather than
			// silently running uncapped.
			return resourceCapWrap{}, bwrapCapError
		}
		dir := filepath.Join(parent, "bugbot-"+randToken())
		if err := os.Mkdir(dir, 0o755); err != nil {
			return resourceCapWrap{}, fmt.Errorf("sandbox: create cgroup subtree: %w", err)
		}
		if err := writeCgroupLimits(dir, cpus, memoryMB, pidsLimit); err != nil {
			_ = os.Remove(dir)
			return resourceCapWrap{}, err
		}
		return resourceCapWrap{
			name: s.bwrapPath,
			args: bwrapArgv,
			joinCgroup: func(pid int) error {
				if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
					return fmt.Errorf("sandbox: join cgroup subtree: %w", err)
				}
				return nil
			},
			cleanup: func() { _ = os.Remove(dir) },
		}, nil
	default:
		return resourceCapWrap{name: s.bwrapPath, args: bwrapArgv, joinCgroup: noop, cleanup: func() {}}, nil
	}
}

// writeCgroupLimits writes the resolved memory.max/cpu.max/pids.max
// controller files into dir. A limit that resolves to "omit" (see
// cgroupV2Limits) is left unwritten, so that controller inherits the
// parent's limit rather than being reset to "max".
func writeCgroupLimits(dir string, cpus float64, memoryMB, pidsLimit int) error {
	memory, cpuMax, pids, memOK, cpuOK, pidsOK := cgroupV2Limits(cpus, memoryMB, pidsLimit)
	if memOK {
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(memory), 0o644); err != nil {
			return fmt.Errorf("sandbox: write memory.max: %w", err)
		}
	}
	if cpuOK {
		if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(cpuMax), 0o644); err != nil {
			return fmt.Errorf("sandbox: write cpu.max: %w", err)
		}
	}
	if pidsOK {
		if err := os.WriteFile(filepath.Join(dir, "pids.max"), []byte(pids), 0o644); err != nil {
			return fmt.Errorf("sandbox: write pids.max: %w", err)
		}
	}
	return nil
}

// procTreeCPUBusy is the bwrap backend's activeFallback progress signal
// (watchIdle's costlier-fallback argument): it reports whether the process
// tree rooted at pid is currently consuming CPU above cpuBusyThreshold,
// sampled via /proc — the host-process analogue of containerCPUBusy's
// `runtime stats` probe, since bwrap has no container object to query.
func procTreeCPUBusy(pid int) bool {
	before, ok := procTreeCPUTicks(pid)
	if !ok {
		return false
	}
	time.Sleep(cpuSampleWindow)
	after, ok := procTreeCPUTicks(pid)
	if !ok {
		return false
	}
	deltaTicks := after - before
	if deltaTicks <= 0 {
		return false
	}
	// Convert ticks busy over the sample window into a CPU percentage
	// (100% == one full core continuously busy for the whole window) and
	// compare against the same threshold containerCPUBusy uses, so the two
	// backends' idle-watchdog "still working" semantics agree.
	ticksPerSec := float64(clockTicksPerSec)
	pct := (float64(deltaTicks) / ticksPerSec) / cpuSampleWindow.Seconds() * 100
	return pct > cpuBusyThreshold
}

// cpuSampleWindow is how long procTreeCPUBusy waits between its two /proc
// samples. Short enough to keep an idle-tick's added latency negligible,
// long enough that scheduler jitter doesn't dominate the measurement.
const cpuSampleWindow = 200 * time.Millisecond

// clockTicksPerSec is USER_HZ, the unit /proc/<pid>/stat's utime/stime
// fields are expressed in on Linux. 100 on every architecture Go supports.
const clockTicksPerSec = 100

// procTreeCPUTicks sums utime+stime (in clock ticks) across pid and every
// live descendant, discovered via /proc/<pid>/task/<tid>/children (Linux
// 3.5+, present on any kernel modern enough to run unprivileged userns
// sandboxes). Best-effort: any failure (process exited mid-walk, kernel
// lacks the children file) returns ok=false so the caller falls back to
// treating the tick as idle, exactly like containerCPUBusy's error handling.
func procTreeCPUTicks(root int) (ticks int64, ok bool) {
	rootTicks, rootOK := procStatTicks(root)
	if !rootOK {
		// The root pid itself must resolve — otherwise this is not "process
		// with zero CPU usage", it's "process does not exist", and the
		// caller must treat that as unknown (ok=false), not as zero ticks.
		return 0, false
	}
	seen := map[int]bool{root: true}
	total := rootTicks
	var walk func(pid int)
	walk = func(pid int) {
		for _, c := range procChildren(pid) {
			if seen[c] {
				continue
			}
			seen[c] = true
			if t, statOK := procStatTicks(c); statOK {
				total += t
			}
			walk(c)
		}
	}
	walk(root)
	return total, true
}

// procStatTicks reads utime (field 14) and stime (field 15) from
// /proc/<pid>/stat. The comm field (2nd, parenthesized) may itself contain
// spaces, so fields are counted from the END of the line rather than
// splitting naively on whitespace from the start.
func procStatTicks(pid int) (int64, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	line := string(data)
	closeParen := lastIndexByte(line, ')')
	if closeParen < 0 || closeParen+2 >= len(line) {
		return 0, false
	}
	rest := splitFields(line[closeParen+2:])
	// rest[0] is field 3 (state); utime is field 14 -> rest index 11; stime
	// is field 15 -> rest index 12.
	if len(rest) < 13 {
		return 0, false
	}
	utime, err1 := strconv.ParseInt(rest[11], 10, 64)
	stime, err2 := strconv.ParseInt(rest[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return utime + stime, true
}

// procChildren returns pid's direct child pids via the Linux 3.5+
// /proc/<pid>/task/<tid>/children interface, aggregated across every thread
// of pid (a multi-threaded process can parent children from any thread).
// Best-effort: a missing/unreadable children file for one thread is skipped
// rather than failing the whole call, since a thread can legitimately exit
// mid-walk.
func procChildren(pid int) []int {
	taskDir := fmt.Sprintf("/proc/%d/task", pid)
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(taskDir, e.Name(), "children"))
		if err != nil {
			continue
		}
		for _, f := range splitFields(string(data)) {
			if n, err := strconv.Atoi(f); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func splitFields(s string) []string {
	var fields []string
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\n' || s[i] == '\t' {
			if start >= 0 {
				fields = append(fields, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		fields = append(fields, s[start:])
	}
	return fields
}

var _ Sandbox = (*Bwrap)(nil)

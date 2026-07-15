package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// bwrap has no cgroups (and no daemon) of its own to enforce resource limits,
// unlike the container backend's --memory/--cpus/--pids-limit flags (see
// buildRunArgs). Two host mechanisms can still enforce them:
//
//   - systemd-run --user --scope wraps the bwrap invocation in a transient
//     systemd unit and applies MemoryMax/CPUQuota/TasksMax via the user's
//     systemd instance — no privilege beyond a running `systemd --user`
//     session and a login manager that set up the delegate hierarchy (true
//     for any modern systemd-logind desktop/server).
//   - cgroup v2 direct: write memory.max/cpu.max/pids.max into a delegated
//     subtree of the CALLING process's own cgroup (found via
//     /proc/self/cgroup) and move the bwrap pid into it after launch.
//
// When NEITHER is available, resource limits would otherwise be silently
// dropped — exactly the failure mode acceptance criterion 4 forbids. The run
// FAILS with an actionable error unless the operator explicitly opts into
// sandbox.allow_uncapped.

// bwrapCapMethod names the resource-limit enforcement mechanism a Bwrap
// backend resolved for the current host.
type bwrapCapMethod int

const (
	// bwrapCapNone means neither systemd-run --user --scope nor a delegated
	// cgroup v2 subtree is usable on this host.
	bwrapCapNone bwrapCapMethod = iota
	bwrapCapSystemdRun
	bwrapCapCgroupV2
)

// systemdRunProbeTimeout bounds the one-time check that the user's systemd
// instance is actually reachable (not merely that the binaries exist on
// PATH) — a headless host with systemd-run installed but no user session bus
// running must fail this probe, not silently be believed capable.
const systemdRunProbeTimeout = 5 * time.Second

// detectBwrapCapMethod picks the best available resource-limit mechanism.
// Every step is best-effort and cheap; failures fall through to the next
// method rather than erroring, since the ultimate "nothing worked" case is
// handled by the caller (Exec), which decides whether that is fatal based on
// allowUncapped.
func detectBwrapCapMethod(ctx context.Context) bwrapCapMethod {
	if systemdRunUserAvailable(ctx) {
		return bwrapCapSystemdRun
	}
	if _, ok := delegatedCgroupV2Dir(); ok {
		return bwrapCapCgroupV2
	}
	return bwrapCapNone
}

// DescribeBwrapCapMethod reports the resource-limit enforcement mechanism
// this host currently supports for the bwrap backend, for doctor's advisory
// reporting: "enforced" (systemd-run --user --scope or cgroup v2 available,
// so runs get their configured caps) or a reason why neither is available
// (runs would fail unless sandbox.allow_uncapped is set — see errBwrapNoCapMethod).
func DescribeBwrapCapMethod(ctx context.Context) (label string, enforced bool) {
	switch detectBwrapCapMethod(ctx) {
	case bwrapCapSystemdRun:
		return "systemd-run --user --scope", true
	case bwrapCapCgroupV2:
		return "delegated cgroup v2 subtree", true
	default:
		return "none (neither systemd-run --user --scope nor a delegated cgroup v2 subtree; runs fail unless sandbox.allow_uncapped is set)", false
	}
}

// systemdRunUserAvailable reports whether `systemd-run --user --scope` can
// actually launch a transient unit on this host: both systemd-run and
// systemctl must be on PATH, and the user's systemd instance must answer a
// trivial query within systemdRunProbeTimeout. A binary merely existing on
// PATH is not sufficient — a container or minimal host can have the client
// tools installed with no systemd PID 1 and no user session bus behind them.
func systemdRunUserAvailable(ctx context.Context) bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, systemdRunProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "systemctl", "--user", "show-environment")
	return cmd.Run() == nil
}

// cgroupV2Root is the standard cgroup v2 mount point.
const cgroupV2Root = "/sys/fs/cgroup"

// delegatedCgroupV2Dir returns the directory of the calling process's own
// cgroup v2 membership, if the host mounts cgroup v2 (cgroup.controllers
// present at cgroupV2Root) AND that directory is writable by this process —
// the signal that the cgroup was delegated to the (possibly unprivileged)
// user rather than being root-owned. A subdirectory created there inherits
// delegation and can set memory.max/cpu.max/pids.max for its own descendants.
func delegatedCgroupV2Dir() (string, bool) {
	if _, err := os.Stat(cgroupV2Root + "/cgroup.controllers"); err != nil {
		return "", false
	}
	self, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", false
	}
	// cgroup v2 has exactly one line: "0::<path>".
	line := strings.TrimSpace(string(self))
	parts := strings.SplitN(line, ":", 3)
	if len(parts) != 3 {
		return "", false
	}
	dir := cgroupV2Root + parts[2]
	// Writability is the actual delegation signal: probe with a throwaway
	// subdirectory rather than trusting ownership bits, since ACLs/systemd
	// delegation can grant write access without matching Unix ownership.
	probe := dir + "/.bugbot-cgroup-probe"
	if err := os.Mkdir(probe, 0o755); err != nil {
		return "", false
	}
	_ = os.Remove(probe)
	return dir, true
}

// errBwrapNoCapMethod is returned when resource limits were requested (the normal
// case) but neither enforcement mechanism is available and the operator has
// not opted into running uncapped. It is a distinct type so callers/tests can
// assert on it without string matching.
var errBwrapNoCapMethod = errors.New("sandbox: bwrap backend found no resource-limit mechanism (systemd-run --user --scope or a delegated cgroup v2 subtree); set sandbox.allow_uncapped to run without enforced memory/CPU/pids limits")

// systemdRunWrapArgs prepends a systemd-run --user --scope invocation (with
// MemoryMax/CPUQuota/TasksMax properties) around the given bwrap binary +
// args, returning the full argv to exec. cpus <= 0 / memoryMB <= 0 / pids <=
// 0 omit the corresponding property, matching buildRunArgs' "omit when
// unset" convention for --memory/--cpus/--pids-limit.
func systemdRunWrapArgs(bwrapPath string, bwrapArgs []string, cpus float64, memoryMB, pidsLimit int) []string {
	args := []string{"systemd-run", "--user", "--scope", "--quiet", "--collect"}
	if memoryMB > 0 {
		args = append(args, "-p", fmt.Sprintf("MemoryMax=%dM", memoryMB))
	}
	if cpus > 0 {
		// CPUQuota is a percentage of one core; 1.5 cores -> "150%".
		pct := cpus * 100
		args = append(args, "-p", fmt.Sprintf("CPUQuota=%s%%", strconv.FormatFloat(pct, 'f', -1, 64)))
	}
	if pidsLimit > 0 {
		args = append(args, "-p", fmt.Sprintf("TasksMax=%d", pidsLimit))
	}
	args = append(args, "--", bwrapPath)
	args = append(args, bwrapArgs...)
	return args
}

// cgroupV2Limits renders the raw file contents for a delegated cgroup v2
// subtree's memory.max, cpu.max, and pids.max controllers. cpu.max's format
// is "<quota> <period>" in microseconds; a 100000us (100ms) period with
// quota = cpus*period gives the same fractional-core semantics as the
// container backend's --cpus. Zero/negative inputs are reported as omit=true
// so the caller writes nothing for that controller (leaving it at the
// parent's inherited limit, mirroring "omit when unset" elsewhere).
func cgroupV2Limits(cpus float64, memoryMB, pidsLimit int) (memory, cpuMax, pids string, memOK, cpuOK, pidsOK bool) {
	if memoryMB > 0 {
		memory = strconv.FormatInt(int64(memoryMB)*1024*1024, 10)
		memOK = true
	}
	if cpus > 0 {
		const periodUS = 100000
		quota := int64(cpus * float64(periodUS))
		cpuMax = fmt.Sprintf("%d %d", quota, periodUS)
		cpuOK = true
	}
	if pidsLimit > 0 {
		pids = strconv.Itoa(pidsLimit)
		pidsOK = true
	}
	return
}

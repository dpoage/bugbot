package sandbox

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestSystemdRunWrapArgs(t *testing.T) {
	args := systemdRunWrapArgs("/usr/bin/bwrap", []string{"--unshare-all", "true"}, 1.5, 512, 128)

	if args[0] != "systemd-run" {
		t.Fatalf("expected systemd-run as argv[0], got %q", args[0])
	}
	mustContainSeq(t, args, "--user")
	mustContainSeq(t, args, "--scope")
	mustContainSeq(t, args, "-p", "MemoryMax=512M")
	mustContainSeq(t, args, "-p", "CPUQuota=150%")
	mustContainSeq(t, args, "-p", "TasksMax=128")
	mustContainSeq(t, args, "--", "/usr/bin/bwrap")

	// The bwrap argv must be preserved verbatim at the tail.
	tail := args[len(args)-2:]
	if !slices.Equal(tail, []string{"--unshare-all", "true"}) {
		t.Errorf("bwrap argv not preserved at tail: %q", tail)
	}
}

func TestSystemdRunWrapArgsOmitsUnsetLimits(t *testing.T) {
	args := systemdRunWrapArgs("/usr/bin/bwrap", []string{"true"}, 0, 0, 0)
	for _, prop := range []string{"MemoryMax", "CPUQuota", "TasksMax"} {
		for _, a := range args {
			if strings.HasPrefix(a, prop) {
				t.Errorf("expected %s to be omitted when unset; args=%q", prop, args)
			}
		}
	}
}

func TestCgroupV2Limits(t *testing.T) {
	memory, cpuMax, pids, memOK, cpuOK, pidsOK := cgroupV2Limits(1.5, 512, 128)
	if !memOK || memory != "536870912" {
		t.Errorf("memory = %q ok=%v, want 536870912 bytes", memory, memOK)
	}
	if !cpuOK || cpuMax != "150000 100000" {
		t.Errorf("cpuMax = %q ok=%v, want '150000 100000'", cpuMax, cpuOK)
	}
	if !pidsOK || pids != "128" {
		t.Errorf("pids = %q ok=%v, want 128", pids, pidsOK)
	}
}

func TestCgroupV2LimitsOmitsUnset(t *testing.T) {
	_, _, _, memOK, cpuOK, pidsOK := cgroupV2Limits(0, 0, 0)
	if memOK || cpuOK || pidsOK {
		t.Errorf("expected all limits omitted for zero inputs, got mem=%v cpu=%v pids=%v", memOK, cpuOK, pidsOK)
	}
}

func TestDelegatedCgroupV2Dir(t *testing.T) {
	// Best-effort: only assert the function does not panic and, when it does
	// report a dir, that dir path is non-empty and rooted under cgroupV2Root.
	// The real availability varies by host (CI containers commonly run
	// without a delegated cgroup v2 subtree), so this cannot assert a fixed
	// outcome.
	dir, ok := delegatedCgroupV2Dir()
	if ok && !strings.HasPrefix(dir, cgroupV2Root) {
		t.Errorf("delegated dir %q must be rooted under %q", dir, cgroupV2Root)
	}
}

func TestSystemdRunUserAvailableDoesNotHang(t *testing.T) {
	// Whatever the host's real answer is, the probe must return within its
	// own timeout instead of blocking the test.
	ctx, cancel := context.WithTimeout(context.Background(), systemdRunProbeTimeout*2)
	defer cancel()
	done := make(chan struct{})
	go func() {
		systemdRunUserAvailable(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("systemdRunUserAvailable did not return within 2x its own timeout")
	}
}

func TestDetectBwrapCapMethodNeverPanics(t *testing.T) {
	// detectBwrapCapMethod must always resolve to one of the three defined
	// methods and never block indefinitely, regardless of host capability.
	ctx, cancel := context.WithTimeout(context.Background(), systemdRunProbeTimeout*2)
	defer cancel()
	method := detectBwrapCapMethod(ctx)
	if method != bwrapCapNone && method != bwrapCapSystemdRun && method != bwrapCapCgroupV2 {
		t.Errorf("unexpected cap method %v", method)
	}
}

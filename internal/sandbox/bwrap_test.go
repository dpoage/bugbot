package sandbox

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestDetectBwrapReasonsAreActionable(t *testing.T) {
	ok, reason := DetectBwrap()
	if runtime.GOOS != "linux" {
		if ok {
			t.Fatalf("DetectBwrap must never report ok on %s", runtime.GOOS)
		}
		if reason == "" {
			t.Error("expected a non-empty reason on a non-Linux host")
		}
		return
	}
	// On Linux the outcome depends on whether this host actually has bwrap
	// and usable userns; either way a false result must explain why.
	if !ok && reason == "" {
		t.Error("DetectBwrap reported unavailable with no reason")
	}
}

// TestDetectBwrapTrueWhenUsable is the regression the reviewer's finding
// exposed: TestDetectBwrapReasonsAreActionable above accepts ok==false
// unconditionally on Linux ("either way a false result must explain why"),
// which let a probeBwrapUserns bug that made EVERY host report unavailable
// ship unnoticed (the probe ran `bwrap --unshare-all --die-with-parent
// /bin/true` against bwrap's empty tmpfs root, where /bin/true never
// exists). This test independently confirms userns is usable (bwrap on
// PATH + the kernel's own /proc/sys/user/max_user_namespaces > 0) and then
// asserts DetectBwrap agrees — so a probe that spuriously reports
// unavailable on a genuinely usable host fails loudly instead of blending
// into "either way" silence.
func TestDetectBwrapTrueWhenUsable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap is Linux-only")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not found on PATH")
	}
	raw, err := os.ReadFile("/proc/sys/user/max_user_namespaces")
	if err != nil {
		t.Skipf("cannot read /proc/sys/user/max_user_namespaces: %v", err)
	}
	max, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || max <= 0 {
		t.Skip("unprivileged user namespaces are disabled on this host (max_user_namespaces <= 0)")
	}
	ok, reason := DetectBwrap()
	if !ok {
		t.Fatalf("DetectBwrap reported unavailable on a host with bwrap on PATH and max_user_namespaces=%d: %s", max, reason)
	}
}

func TestNewBwrapFailsFastWhenUnavailable(t *testing.T) {
	ok, _ := DetectBwrap()
	if ok {
		t.Skip("bwrap is available on this host; NewBwrap success path is covered by the integration suite")
	}
	if _, err := NewBwrap(); err == nil {
		t.Fatal("expected NewBwrap to fail when DetectBwrap reports unavailable")
	}
}

func TestBwrapOptionsApplyDefaults(t *testing.T) {
	s := &Bwrap{}
	WithBwrapCPUs(3)(s)
	WithBwrapMemoryMB(1024)(s)
	WithBwrapPidsLimit(64)(s)
	WithBwrapNetwork("host")(s)
	WithBwrapAllowUncapped(true)(s)
	fp := "go1.25+node20"
	WithBwrapCapabilityFingerprint(fp)(s)

	cpus, mem, pids := s.Limits()
	if cpus != 3 || mem != 1024 || pids != 64 {
		t.Errorf("Limits() = %v %v %v, want 3 1024 64", cpus, mem, pids)
	}
	if s.defaultNetwork != "host" {
		t.Errorf("defaultNetwork = %q, want host", s.defaultNetwork)
	}
	if !s.allowUncapped {
		t.Error("allowUncapped should be true")
	}
	if s.CapabilityFingerprint() != fp {
		t.Errorf("CapabilityFingerprint() = %q, want %q", s.CapabilityFingerprint(), fp)
	}
}

func TestBwrapResolveParamsRejectsBadNetwork(t *testing.T) {
	s := &Bwrap{defaultNetwork: "none"}
	if _, err := s.resolveBwrapParams(Spec{Cmd: []string{"true"}, Network: "bridge"}); err == nil {
		t.Error("expected an error for an unsupported bwrap network mode")
	}
	p, err := s.resolveBwrapParams(Spec{Cmd: []string{"true"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.network != "none" {
		t.Errorf("expected default network 'none', got %q", p.network)
	}
}

func TestBwrapExecRejectsEmptyCmd(t *testing.T) {
	s := &Bwrap{}
	if _, err := s.Exec(nil, Spec{}); err == nil { //nolint:staticcheck // nil ctx is fine; Exec must fail before using it.
		t.Error("expected Exec to reject an empty Spec.Cmd before touching ctx")
	}
}

func TestBwrapExecRejectsMountCollisionBeforeAnyWork(t *testing.T) {
	s := &Bwrap{}
	_, err := s.Exec(nil, Spec{ //nolint:staticcheck
		Cmd:      []string{"true"},
		ROMounts: []ROMount{{HostPath: "/host", ContainerPath: "/usr"}},
	})
	if err == nil {
		t.Error("expected Exec to reject a mount colliding with the fixed allowlist")
	}
}

// --- /proc-based CPU sampling (activeFallback) --------------------------

func TestProcTreeCPUTicksSelf(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs is Linux-only")
	}
	ticks, ok := procTreeCPUTicks(os.Getpid())
	if !ok {
		t.Fatal("expected to read this process's own /proc/<pid>/stat")
	}
	if ticks < 0 {
		t.Errorf("ticks = %d, want >= 0", ticks)
	}
}

func TestProcTreeCPUTicksUnknownPid(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs is Linux-only")
	}
	// A very large, almost certainly unused PID should not resolve.
	if _, ok := procTreeCPUTicks(1 << 30); ok {
		t.Error("expected ok=false for a pid that does not exist")
	}
}

func TestProcStatTicksParsesOwnProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs is Linux-only")
	}
	// Exercise the real parser against this process's own stat line; comm
	// rarely has spaces in the test binary's name, but the parser must not
	// depend on that — it locates fields from the trailing ')' regardless.
	if _, ok := procStatTicks(os.Getpid()); !ok {
		t.Fatal("expected to parse this process's own /proc/<pid>/stat")
	}
}

func TestSplitFields(t *testing.T) {
	got := splitFields("  1  2\t3\n")
	want := []string{"1", "2", "3"}
	if len(got) != len(want) {
		t.Fatalf("splitFields = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitFields[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLastIndexByte(t *testing.T) {
	if got := lastIndexByte("a)b)c", ')'); got != 3 {
		t.Errorf("lastIndexByte = %d, want 3", got)
	}
	if got := lastIndexByte("abc", ')'); got != -1 {
		t.Errorf("lastIndexByte = %d, want -1", got)
	}
}

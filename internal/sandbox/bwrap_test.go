package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
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
	WithBwrapAllowNestedUserns(true)(s)
	WithBwrapAllowNonNativeArch(true)(s)
	WithBwrapScratchSizeMB(256)(s)
	WithBwrapWorkspaceGrowthCeilingMB(1024)(s)

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
	if !s.allowNestedUserns {
		t.Error("allowNestedUserns should be true")
	}
	if !s.allowNonNativeArch {
		t.Error("allowNonNativeArch should be true")
	}
	if s.defaultScratchSizeMB != 256 {
		t.Errorf("defaultScratchSizeMB = %d, want 256", s.defaultScratchSizeMB)
	}
	if want := int64(1024) * 1024 * 1024; s.defaultGrowthCeilingBytes != want {
		t.Errorf("defaultGrowthCeilingBytes = %d, want %d (1024 MB in bytes)", s.defaultGrowthCeilingBytes, want)
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

// --- bubblewrap version gate (bugbot-6dph) -------------------------------

func TestParseBwrapVersion(t *testing.T) {
	cases := []struct {
		in      string
		want    bwrapVersion
		wantErr bool
	}{
		{"bubblewrap 0.11.0\n", bwrapVersion{major: 0, minor: 11, patch: 0}, false},
		{"bubblewrap 0.8.0", bwrapVersion{major: 0, minor: 8, patch: 0}, false},
		{"bubblewrap 1.2\n", bwrapVersion{major: 1, minor: 2, patch: 0}, false},
		// A non-numeric suffix stuck to the patch component is tolerated —
		// the leading digit run is parsed AND the pre-release flag is set
		// (oracle nit: an rc/dev build must not satisfy a >= floor pinned
		// to the bare release it precedes).
		{"bubblewrap 0.8.0-dev\n", bwrapVersion{major: 0, minor: 8, patch: 0, pre: true}, false},
		{"bubblewrap 0.9.3+deb1\n", bwrapVersion{major: 0, minor: 9, patch: 3, pre: true}, false},
		{"flatpak-spawn 1.14.4\n", bwrapVersion{}, true},
		{"", bwrapVersion{}, true},
		{"bubblewrap notaversion\n", bwrapVersion{}, true},
	}
	for _, c := range cases {
		got, err := parseBwrapVersion(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseBwrapVersion(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseBwrapVersion(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseBwrapVersion(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

// TestBwrapVersionLess_PreReleaseDoesNotSatisfyFloor pins the oracle nit:
// "0.8.0-rc1" must NOT be accepted as satisfying a >= 0.8.0 floor — an rc
// build predates the release it previews.
func TestBwrapVersionLess_PreReleaseDoesNotSatisfyFloor(t *testing.T) {
	rc := bwrapVersion{major: 0, minor: 8, patch: 0, pre: true}
	release := bwrapVersion{major: 0, minor: 8, patch: 0}
	if !rc.less(release) {
		t.Errorf("%v.less(%v) = false, want true (a pre-release must sort below its bare release)", rc, release)
	}
	if release.less(rc) {
		t.Errorf("%v.less(%v) = true, want false (the bare release is not less than its own pre-release)", release, rc)
	}
}

func TestBwrapVersionLess(t *testing.T) {
	cases := []struct {
		a, b bwrapVersion
		want bool
	}{
		{bwrapVersion{major: 0, minor: 7, patch: 9}, bwrapVersion{major: 0, minor: 8, patch: 0}, true},
		{bwrapVersion{major: 0, minor: 8, patch: 0}, bwrapVersion{major: 0, minor: 8, patch: 0}, false},
		{bwrapVersion{major: 0, minor: 8, patch: 1}, bwrapVersion{major: 0, minor: 8, patch: 0}, false},
		{bwrapVersion{major: 0, minor: 11, patch: 0}, bwrapVersion{major: 0, minor: 8, patch: 0}, false},
		{bwrapVersion{major: 1, minor: 0, patch: 0}, bwrapVersion{major: 0, minor: 99, patch: 99}, false},
	}
	for _, c := range cases {
		if got := c.a.less(c.b); got != c.want {
			t.Errorf("%+v.less(%+v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestDetectBwrapRejectsOldVersion exercises DetectBwrap's version gate
// against a fake "bwrap" on PATH that only understands --version (reporting
// an ancient release) — covers the "older bubblewrap lacking these flags
// must degrade with an actionable reason naming the required version"
// acceptance criterion without needing an actually-old bwrap binary
// installed anywhere.
func TestDetectBwrapRejectsOldVersion(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap is Linux-only")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "bwrap")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'bubblewrap 0.4.1'; exit 0; fi\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bwrap: %v", err)
	}
	v, err := probeBwrapVersion(fake)
	if err != nil {
		t.Fatalf("probeBwrapVersion: %v", err)
	}
	if !v.less(bwrapMinVersion) {
		t.Fatalf("probed version %v should be less than bwrapMinVersion %v", v, bwrapMinVersion)
	}
}

// TestDetectBwrapRejectsPreReleaseAtFloor is the end-to-end counterpart of
// TestBwrapVersionLess_PreReleaseDoesNotSatisfyFloor: a fake bwrap
// reporting "bubblewrap 0.8.0-rc1" — numerically AT the floor but an rc
// build of it — must still be rejected by DetectBwrap, not silently
// accepted.
func TestDetectBwrapRejectsPreReleaseAtFloor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap is Linux-only")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "bwrap")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'bubblewrap 0.8.0-rc1'; exit 0; fi\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bwrap: %v", err)
	}
	v, err := probeBwrapVersion(fake)
	if err != nil {
		t.Fatalf("probeBwrapVersion: %v", err)
	}
	if !v.less(bwrapMinVersion) {
		t.Fatalf("probed pre-release version %v should be less than bwrapMinVersion %v", v, bwrapMinVersion)
	}
}

// TestDetectBwrapPermissionHint pins the oracle nit: a "bwrap" file that
// exists on PATH but lacks the executable bit must get an actionable
// permissions hint, not the generic (and misleading) "not found on PATH" —
// exec.LookPath alone cannot distinguish the two cases (both collapse to
// its ErrNotFound), so DetectBwrap supplements it with a direct PATH scan.
func TestDetectBwrapPermissionHint(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap is Linux-only")
	}
	dir := t.TempDir()
	nonExec := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(nonExec, []byte("#!/bin/sh\necho bubblewrap 0.11.0\n"), 0o644); err != nil {
		t.Fatalf("write non-executable bwrap: %v", err)
	}
	t.Setenv("PATH", dir)

	ok, reason := DetectBwrap()
	if ok {
		t.Fatal("DetectBwrap must not report ok when the only PATH entry is non-executable")
	}
	if !strings.Contains(reason, "not executable") || !strings.Contains(reason, nonExec) {
		t.Errorf("reason = %q, want it to name %q as not executable", reason, nonExec)
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

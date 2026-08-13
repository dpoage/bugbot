package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// newFakePodmanFillerCLI builds a CLI backend whose "runtime" is a fake
// podman script on a scoped PATH (no real container runtime involved — a
// hermetic unit test, matching this package's existing shell-based
// hostexec_test.go pattern). The script recognizes two invocations:
//   - "rm ..." (CLI.forceRemove's cleanup call): a no-op, exit 0.
//   - "run ... -v <ws>:/workspace:rw,Z ... <image> <cmd>": ignores every
//     flag except the workspace bind mount, then writes a new 4 KiB file
//     into that workspace directory every 50ms, up to 200 iterations
//     (bounded so a broken watchdog fails the test instead of hanging it).
//
// This is the oracle-review-requested Exec-level fixture (bugbot-bdqf
// review B1/A-B1): unlike watchdog_test.go's direct watchIdle probes, it
// drives the REAL CLI.Exec code path end to end — the Result field mapping
// (WorkspaceQuotaExceeded vs TimedOut) and the watchdog spawn gate
// (idleTimeout>0 || growthCeilingBytes>0) are both exercised for real, so a
// mutation collapsing either one is caught.
func newFakePodmanFillerCLI(t *testing.T, opts ...Option) *CLI {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-based fake runtime assumes POSIX /bin/sh")
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
set -u
if [ "$1" = "rm" ]; then
  exit 0
fi
ws=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-v" ]; then
    ws="${arg%%:*}"
  fi
  prev="$arg"
done
if [ -z "$ws" ]; then
  echo "fake-podman: no workspace bind found" >&2
  exit 1
fi
i=0
while [ $i -lt 200 ]; do
  dd if=/dev/zero of="$ws/hog.$i" bs=4096 count=1 2>/dev/null
  i=$((i+1))
  sleep 0.05
done
exit 0
`
	scriptPath := filepath.Join(binDir, "podman")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake podman script: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s, err := NewCLI("podman", "unused-image", opts...)
	if err != nil {
		t.Fatalf("NewCLI: %v", err)
	}
	return s
}

// TestCLIExec_QuotaKillFidelity is the Exec-level fidelity test the oracle
// round required (bugbot-bdqf review B1/A-B1): a run whose only activity is
// workspace growth returns WorkspaceQuotaExceeded=true, TimedOut=false,
// ExitCode=-1, err=nil — through the REAL CLI.Exec code path, not a direct
// watchIdle call. Both IdleTimeout and Timeout are generous so growth is
// unambiguously what fires, and well before either could.
func TestCLIExec_QuotaKillFidelity(t *testing.T) {
	s := newFakePodmanFillerCLI(t)
	s.defaultGrowthCeilingBytes = 20_000 // ~5 filler files
	s.defaultIdleTimeout = 60 * time.Second
	s.defaultTimeout = 30 * time.Second

	start := time.Now()
	res, err := s.Exec(context.Background(), Spec{RepoDir: t.TempDir(), Cmd: []string{"irrelevant"}})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.WorkspaceQuotaExceeded {
		t.Errorf("WorkspaceQuotaExceeded = false, want true (res=%+v)", res)
	}
	if res.TimedOut {
		t.Errorf("TimedOut = true, want false — a growth-ceiling kill must never collapse into TimedOut (res=%+v)", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
	if elapsed > 10*time.Second {
		t.Errorf("elapsed = %s, want well under the 30s/60s Timeout/IdleTimeout ceilings", elapsed)
	}
}

// TestCLIExec_QuotaKillFidelity_SpawnGateWithIdleTimeoutUnset pins the
// watchdog spawn gate (bugbot-bdqf review A-B1, mutation M8): the goroutine
// must start from the growth ceiling ALONE, with IdleTimeout completely
// unset (both Spec.IdleTimeout and the backend default are zero) — gutting
// the "idleTimeout>0 || growthCeilingBytes>0" gate back to "idleTimeout>0"
// would silently disable growth-ceiling enforcement whenever an operator
// sets idle_timeout_seconds: 0, reintroducing the original unbounded-disk
// DoS in full.
func TestCLIExec_QuotaKillFidelity_SpawnGateWithIdleTimeoutUnset(t *testing.T) {
	s := newFakePodmanFillerCLI(t)
	s.defaultGrowthCeilingBytes = 20_000
	s.defaultIdleTimeout = 0 // explicitly unset — the exact M8 scenario
	s.defaultTimeout = 30 * time.Second

	res, err := s.Exec(context.Background(), Spec{RepoDir: t.TempDir(), Cmd: []string{"irrelevant"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.WorkspaceQuotaExceeded {
		t.Errorf("WorkspaceQuotaExceeded = false, want true — the watchdog must spawn from the growth ceiling alone (res=%+v)", res)
	}
	if res.TimedOut {
		t.Errorf("TimedOut = true, want false (res=%+v)", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

// TestCLIExec_QuotaCeilingDisabled_RunsToCompletion is the negative control:
// with the growth ceiling disabled entirely (0), the same filler workload
// must run to its natural completion (ExitCode=0, no kill flags) — proves
// the fixture itself is sound (it is the CEILING, not some other mechanism,
// producing the kill in the tests above) and that a disabled ceiling truly
// disables enforcement.
func TestCLIExec_QuotaCeilingDisabled_RunsToCompletion(t *testing.T) {
	s := newFakePodmanFillerCLI(t)
	s.defaultGrowthCeilingBytes = 0
	s.defaultIdleTimeout = 0
	s.defaultTimeout = 30 * time.Second

	res, err := s.Exec(context.Background(), Spec{RepoDir: t.TempDir(), Cmd: []string{"irrelevant"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.WorkspaceQuotaExceeded || res.TimedOut {
		t.Errorf("expected a clean completion with the ceiling disabled, got %+v", res)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// newFakePodmanBurstWriterCLI is a variant of newFakePodmanFillerCLI whose
// fake podman script writes ALL of its filler data in one shot and exits
// immediately — no loop, no sleep — so it is guaranteed to finish inside a
// single growthPollInterval tick.
func newFakePodmanBurstWriterCLI(t *testing.T, opts ...Option) *CLI {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-based fake runtime assumes POSIX /bin/sh")
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
set -u
if [ "$1" = "rm" ]; then
  exit 0
fi
ws=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-v" ]; then
    ws="${arg%%:*}"
  fi
  prev="$arg"
done
if [ -z "$ws" ]; then
  echo "fake-podman: no workspace bind found" >&2
  exit 1
fi
dd if=/dev/zero of="$ws/burst" bs=1048576 count=4 2>/dev/null
exit 0
`
	scriptPath := filepath.Join(binDir, "podman")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake podman script: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s, err := NewCLI("podman", "unused-image", opts...)
	if err != nil {
		t.Fatalf("NewCLI: %v", err)
	}
	return s
}

// TestCLIExec_QuotaKillFidelity_PostRunCheckCatchesBurstExit pins the other
// half of the oracle-round fix (bugbot-bdqf review B1b): a run that
// breaches the ceiling and exits BEFORE any watchdog tick can observe it —
// here, a 4 MiB burst write against a 1 MiB ceiling that completes and
// exits 0 in well under one growthPollInterval tick — must still be
// classified WorkspaceQuotaExceeded=true, ExitCode=-1, not silently
// reported as a clean ExitCode=0 success. This is Exec's unconditional
// post-run checkGrowthCeiling call, not the tick loop.
func TestCLIExec_QuotaKillFidelity_PostRunCheckCatchesBurstExit(t *testing.T) {
	s := newFakePodmanBurstWriterCLI(t)
	s.defaultGrowthCeilingBytes = 1024 * 1024 // 1 MiB; the burst writes 4 MiB
	s.defaultIdleTimeout = 60 * time.Second
	s.defaultTimeout = 30 * time.Second

	res, err := s.Exec(context.Background(), Spec{RepoDir: t.TempDir(), Cmd: []string{"irrelevant"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.WorkspaceQuotaExceeded {
		t.Errorf("WorkspaceQuotaExceeded = false, want true — a burst write that exits before any tick must still be caught (res=%+v)", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (the breach must override the process's own exit 0)", res.ExitCode)
	}
}

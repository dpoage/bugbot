package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

// newFakePodmanNoopCLI is a fake-podman fixture that touches the workspace
// NOT AT ALL — it exits 0 immediately, writing nothing. Paired with a
// pre-seeded RepoDir, it isolates whether the growth-ceiling baseline is
// captured AFTER workspace preparation (the correct behavior: only bytes
// the COMMAND itself writes should count) or from an empty/zero baseline
// (which would misclassify a large pre-existing repo as an instant
// breach — bugbot-bdqf oracle review, mutation M15).
func newFakePodmanNoopCLI(t *testing.T, opts ...Option) *CLI {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-based fake runtime assumes POSIX /bin/sh")
	}
	binDir := t.TempDir()
	script := "#!/bin/sh\nexit 0\n"
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

// TestCLIExec_QuotaBaselineCapturedAfterWorkspacePrep pins bugbot-bdqf
// mutation M15: the growth-ceiling baseline must be captured from the
// PREPARED workspace (after prepareWorkspace copies RepoDir in), not from
// zero — otherwise a large pre-existing repo would look like instant
// growth and trip the ceiling before the command ever runs. RepoDir is
// seeded with 50 KB of content against a 1 KB ceiling; the fake command
// writes nothing at all, so ANY kill here proves the baseline was wrong.
func TestCLIExec_QuotaBaselineCapturedAfterWorkspacePrep(t *testing.T) {
	s := newFakePodmanNoopCLI(t)
	s.defaultGrowthCeilingBytes = 1000 // 1 KB — far below the seeded content
	s.defaultIdleTimeout = 0
	s.defaultTimeout = 15 * time.Second

	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "big.bin"), make([]byte, 50_000), 0o644); err != nil {
		t.Fatalf("seed repo content: %v", err)
	}

	res, err := s.Exec(context.Background(), Spec{RepoDir: repoDir, Cmd: []string{"irrelevant"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.WorkspaceQuotaExceeded {
		t.Errorf("pre-existing workspace content (50 KB against a 1 KB ceiling) must NOT trip the ceiling — the baseline must be captured AFTER workspace prep, not from zero; got %+v", res)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (the fake command writes nothing and exits cleanly)", res.ExitCode)
	}
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
	if res.WorkspaceFileCountExceeded {
		t.Errorf("WorkspaceFileCountExceeded = true, want false — a byte-size breach must not collapse into the file-count reason (res=%+v)", res)
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
	if res.WorkspaceFileCountExceeded {
		t.Errorf("WorkspaceFileCountExceeded = true, want false (res=%+v)", res)
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
	if res.WorkspaceFileCountExceeded {
		t.Errorf("WorkspaceFileCountExceeded = true, want false — a pure byte-size burst must not collapse into the file-count reason (res=%+v)", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (the breach must override the process's own exit 0)", res.ExitCode)
	}
}

// TestCLIExec_CallerCancellationWinsOverQuotaBreach pins the cancellation-
// precedence fix (bugbot-bdqf oracle review, fix round 2): a caller
// cancellation landing in the same window as a growth-ceiling breach must
// surface as the documented "sandbox: execution cancelled" error — NEVER
// silently reinterpreted as a WorkspaceQuotaExceeded result, since the
// caller no longer wants this outcome at all regardless of what our own
// watchdog machinery observed. The ceiling here is small enough that the
// filler writes well past it in the 200ms window before cancel() fires, so
// checkGrowthCeiling's post-run check WOULD find a breach if it were ever
// consulted — proving this passes because cancellation wins, not because
// no breach happened yet.
func TestCLIExec_CallerCancellationWinsOverQuotaBreach(t *testing.T) {
	s := newFakePodmanFillerCLI(t)
	s.defaultGrowthCeilingBytes = 5_000 // ~1-2 filler iterations blow past this
	s.defaultIdleTimeout = 60 * time.Second
	s.defaultTimeout = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	res, err := s.Exec(ctx, Spec{RepoDir: t.TempDir(), Cmd: []string{"irrelevant"}})
	if err == nil {
		t.Fatalf("expected a cancellation error, got nil err with res=%+v", res)
	}
	if !strings.Contains(err.Error(), "execution cancelled") {
		t.Errorf("error = %v, want it to mention \"execution cancelled\"", err)
	}
	if res.WorkspaceQuotaExceeded {
		t.Errorf("a caller-cancelled run must never report WorkspaceQuotaExceeded, even if a breach was also detected; got %+v", res)
	}
	if res.WorkspaceFileCountExceeded {
		t.Errorf("a caller-cancelled run must never report WorkspaceFileCountExceeded either; got %+v", res)
	}
}

// newFakePodmanManyFilesCLI is newFakePodmanFillerCLI's file-COUNT analogue
// (bugbot-gb3o): the fake podman script's only activity is creating many
// near-zero-byte files (`: > file`, not a 4 KiB dd write) into the
// workspace, one every 5ms, up to 400 iterations (bounded so a broken
// watchdog fails the test instead of hanging it — 400 keeps the disabled-
// ceiling negative control's full run comfortably under the 30s Timeout
// even with real per-iteration fork/exec overhead on top of the 5ms
// sleep). This is the exact motivating shape from the bug report — 10,000
// tiny files totaling 20 KB — scaled down for test speed: fsCount climbs
// fast while fsSize stays negligible, so ONLY a file-count ceiling (never
// the byte-size one) can catch it.
func newFakePodmanManyFilesCLI(t *testing.T, opts ...Option) *CLI {
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
while [ $i -lt 400 ]; do
  : > "$ws/tiny.$i"
  i=$((i+1))
  sleep 0.005
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

// TestCLIExec_FileCountBaselineCapturedAfterWorkspacePrep is
// TestCLIExec_QuotaBaselineCapturedAfterWorkspacePrep's file-count
// analogue and the acceptance-mandated negative test: a normal repo copy
// holding many PRE-EXISTING files (a populated node_modules, a large
// vendored dependency tree, ...) must NOT trip the ceiling — only entries
// created by the COMMAND itself, after the baseline snapshot, count.
// RepoDir is seeded with 500 pre-existing files against a 50-file
// ceiling; the fake command touches nothing at all, so ANY kill here
// proves the baseline was wrong.
func TestCLIExec_FileCountBaselineCapturedAfterWorkspacePrep(t *testing.T) {
	s := newFakePodmanNoopCLI(t)
	s.defaultFileCountCeiling = 50 // far below the seeded file count
	s.defaultGrowthCeilingBytes = 0
	s.defaultIdleTimeout = 0
	s.defaultTimeout = 15 * time.Second

	repoDir := t.TempDir()
	for i := range 500 {
		if err := os.WriteFile(filepath.Join(repoDir, "f"+strconv.Itoa(i)+".txt"), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed repo content: %v", err)
		}
	}

	res, err := s.Exec(context.Background(), Spec{RepoDir: repoDir, Cmd: []string{"irrelevant"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.WorkspaceFileCountExceeded {
		t.Errorf("pre-existing workspace content (500 files against a 50-file ceiling) must NOT trip the ceiling — the baseline must be captured AFTER workspace prep, not from zero; got %+v", res)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (the fake command touches nothing and exits cleanly)", res.ExitCode)
	}
}

// TestCLIExec_FileCountKillFidelity is TestCLIExec_QuotaKillFidelity's
// file-count analogue and the acceptance-mandated positive test: a run
// whose only activity is creating many tiny files is killed by the
// file-count ceiling — WorkspaceFileCountExceeded=true,
// WorkspaceQuotaExceeded=false (the byte-size ceiling never trips; the
// files are near-zero bytes), TimedOut=false, ExitCode=-1, err=nil — well
// before the 30s/60s Timeout/IdleTimeout ceilings, through the REAL
// CLI.Exec code path.
func TestCLIExec_FileCountKillFidelity(t *testing.T) {
	s := newFakePodmanManyFilesCLI(t)
	s.defaultFileCountCeiling = 50 // tripped inside the first ~1s poll tick
	s.defaultGrowthCeilingBytes = 0
	s.defaultIdleTimeout = 60 * time.Second
	s.defaultTimeout = 30 * time.Second

	start := time.Now()
	res, err := s.Exec(context.Background(), Spec{RepoDir: t.TempDir(), Cmd: []string{"irrelevant"}})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.WorkspaceFileCountExceeded {
		t.Errorf("WorkspaceFileCountExceeded = false, want true (res=%+v)", res)
	}
	if res.WorkspaceQuotaExceeded {
		t.Errorf("WorkspaceQuotaExceeded = true, want false — near-zero-byte files must not trip the byte-size ceiling (res=%+v)", res)
	}
	if res.TimedOut {
		t.Errorf("TimedOut = true, want false — a file-count-ceiling kill must never collapse into TimedOut (res=%+v)", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
	if elapsed > 10*time.Second {
		t.Errorf("elapsed = %s, want well under the 30s/60s Timeout/IdleTimeout ceilings", elapsed)
	}
}

// TestCLIExec_FileCountKillFidelity_SpawnGateWithIdleTimeoutUnset pins the
// watchdog spawn gate's third arm (bugbot-gb3o, extending bugbot-bdqf
// review A-B1/mutation M8 to the file-count ceiling): the goroutine must
// start from the file-count ceiling ALONE, with IdleTimeout AND the
// byte-size growth ceiling both completely unset — gutting the
// "idleTimeout>0 || growthCeilingBytes>0 || fileCountCeiling>0" gate back
// to just the first two conditions would silently disable file-count
// enforcement whenever an operator sets idle_timeout_seconds: 0 with the
// byte-size ceiling also disabled.
func TestCLIExec_FileCountKillFidelity_SpawnGateWithIdleTimeoutUnset(t *testing.T) {
	s := newFakePodmanManyFilesCLI(t)
	s.defaultFileCountCeiling = 50
	s.defaultGrowthCeilingBytes = 0
	s.defaultIdleTimeout = 0 // explicitly unset
	s.defaultTimeout = 30 * time.Second

	res, err := s.Exec(context.Background(), Spec{RepoDir: t.TempDir(), Cmd: []string{"irrelevant"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.WorkspaceFileCountExceeded {
		t.Errorf("WorkspaceFileCountExceeded = false, want true — the watchdog must spawn from the file-count ceiling alone (res=%+v)", res)
	}
	if res.TimedOut {
		t.Errorf("TimedOut = true, want false (res=%+v)", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

// TestCLIExec_FileCountCeilingDisabled_RunsToCompletion is the negative
// control mirroring TestCLIExec_QuotaCeilingDisabled_RunsToCompletion:
// with the file-count ceiling disabled entirely (0) and the byte-size
// ceiling also disabled, the same many-tiny-files workload must run to
// its natural completion.
func TestCLIExec_FileCountCeilingDisabled_RunsToCompletion(t *testing.T) {
	s := newFakePodmanManyFilesCLI(t)
	s.defaultFileCountCeiling = 0
	s.defaultGrowthCeilingBytes = 0
	s.defaultIdleTimeout = 0
	s.defaultTimeout = 30 * time.Second

	res, err := s.Exec(context.Background(), Spec{RepoDir: t.TempDir(), Cmd: []string{"irrelevant"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.WorkspaceFileCountExceeded || res.WorkspaceQuotaExceeded || res.TimedOut {
		t.Errorf("expected a clean completion with both ceilings disabled, got %+v", res)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// newFakePodmanFileCountBurstCLI is newFakePodmanBurstWriterCLI's
// file-count analogue: the fake podman script creates a burst of tiny
// files in one tight shell loop (no sleep) and exits immediately, so it
// is guaranteed to finish inside a single growthPollInterval tick.
func newFakePodmanFileCountBurstCLI(t *testing.T, opts ...Option) *CLI {
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
while [ $i -lt 300 ]; do
  : > "$ws/burstfile.$i"
  i=$((i+1))
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

// TestCLIExec_FileCountKillFidelity_PostRunCheckCatchesBurstExit is
// TestCLIExec_QuotaKillFidelity_PostRunCheckCatchesBurstExit's file-count
// analogue: this is the DIRECT test for acceptance criterion 1 ("a burst
// that creates files and exits inside one poll window is still
// classified") — a run that creates 300 tiny files and exits well inside
// a single growthPollInterval tick, against a 50-file ceiling, must still
// be classified WorkspaceFileCountExceeded=true, ExitCode=-1, not
// silently reported as a clean ExitCode=0 success. Dropping Exec's
// unconditional post-run checkGrowthCeiling call (or its file-count half)
// fails this test.
func TestCLIExec_FileCountKillFidelity_PostRunCheckCatchesBurstExit(t *testing.T) {
	s := newFakePodmanFileCountBurstCLI(t)
	s.defaultFileCountCeiling = 50 // the burst creates 300
	s.defaultGrowthCeilingBytes = 0
	s.defaultIdleTimeout = 60 * time.Second
	s.defaultTimeout = 30 * time.Second

	res, err := s.Exec(context.Background(), Spec{RepoDir: t.TempDir(), Cmd: []string{"irrelevant"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.WorkspaceFileCountExceeded {
		t.Errorf("WorkspaceFileCountExceeded = false, want true — a burst of file creation that exits before any tick must still be caught (res=%+v)", res)
	}
	if res.WorkspaceQuotaExceeded {
		t.Errorf("WorkspaceQuotaExceeded = true, want false — a pure file-count burst must not collapse into the byte-size reason (res=%+v)", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (the breach must override the process's own exit 0)", res.ExitCode)
	}
}

// TestCLIExec_CallerCancellationWinsOverFileCountBreach mirrors
// TestCLIExec_CallerCancellationWinsOverQuotaBreach for the file-count
// ceiling: a caller cancellation landing in the same window as a
// file-count breach must surface as "sandbox: execution cancelled",
// never as WorkspaceFileCountExceeded.
func TestCLIExec_CallerCancellationWinsOverFileCountBreach(t *testing.T) {
	s := newFakePodmanManyFilesCLI(t)
	s.defaultFileCountCeiling = 10 // a couple of filler iterations blow past this
	s.defaultGrowthCeilingBytes = 0
	s.defaultIdleTimeout = 60 * time.Second
	s.defaultTimeout = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	res, err := s.Exec(ctx, Spec{RepoDir: t.TempDir(), Cmd: []string{"irrelevant"}})
	if err == nil {
		t.Fatalf("expected a cancellation error, got nil err with res=%+v", res)
	}
	if !strings.Contains(err.Error(), "execution cancelled") {
		t.Errorf("error = %v, want it to mention \"execution cancelled\"", err)
	}
	if res.WorkspaceFileCountExceeded {
		t.Errorf("a caller-cancelled run must never report WorkspaceFileCountExceeded, even if a breach was also detected; got %+v", res)
	}
	if res.WorkspaceQuotaExceeded {
		t.Errorf("a caller-cancelled run must never report WorkspaceQuotaExceeded either; got %+v", res)
	}
}

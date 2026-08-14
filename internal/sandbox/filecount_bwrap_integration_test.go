//go:build integration

// File-count-ceiling integration tests exercise the real bwrap binary and
// unprivileged user namespaces, mirroring bwrap_integration_test.go's
// TestBwrapExec_QuotaKillFidelity family for the byte-size growth ceiling
// (bugbot-bdqf) but for the workspace file-COUNT ceiling (bugbot-gb3o).
// Deliberately kept OUT of bwrap_integration_test.go (owned by a sibling
// slice this round) to avoid editing a file outside this slice's
// ownership; newTestBwrap is reused from there since both files compile
// together in the same package under the same build tag. Run with:
//
//	go test -tags integration ./internal/sandbox/...
//
// They are skipped automatically when bwrap is missing or unprivileged
// userns are unavailable — see DetectBwrap.
package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bwrapFileCountFillerScript creates a NEW near-zero-byte file in the
// current directory every 5ms, up to 1000 iterations (bounded so a broken
// watchdog fails the test instead of hanging it; 1000 keeps natural full-
// loop completion comfortably >= 10s — measured ~12-13s on this host — so
// TestBwrapExec_FileCountKillFidelity's tightened elapsed bound can
// reliably tell "killed by the tick within ~1-2s" apart from "the
// watchdog wiring silently no-op'd and only Exec's unconditional post-run
// check classified it after the command ran to completion"; see A-B1
// fix-round notes on bugbot-gb3o) — the file-count analogue of
// bwrapDiskFillerScript (bwrap_integration_test.go). Plain shell
// redirection (`: > file`) needs no external binary at all (unlike dd),
// so no extra baseline/ROMount resolution is required. This is the exact
// motivating shape from the bug report — 10,000 tiny files totaling 20
// KB — scaled down for test speed: fsCount climbs fast while fsSize
// stays negligible, so ONLY a file-count ceiling (never the byte-size
// one) can catch it.
const bwrapFileCountFillerScript = `i=0
while [ $i -lt 1000 ]; do
  : > tiny.$i
  i=$((i+1))
  sleep 0.005
done`

// TestBwrapExec_FileCountKillFidelity is the bwrap-backend counterpart of
// cli_exec_quota_test.go's TestCLIExec_FileCountKillFidelity (bugbot-gb3o):
// a REAL bwrap run whose only activity is creating many tiny files returns
// WorkspaceFileCountExceeded=true, WorkspaceQuotaExceeded=false (the
// byte-size ceiling never trips; the files are near-zero bytes),
// TimedOut=false, ExitCode=-1, err=nil — through the actual Bwrap.Exec
// code path, well before the Timeout/IdleTimeout ceilings. The elapsed
// bound (~3s) is deliberately TIGHT relative to the filler's ~12-13s
// natural full-loop completion time (see bwrapFileCountFillerScript's
// doc): a mutation that drops the watchdogLimits.fileCountCeiling wiring
// from the tick loop (A-B1 fix round) leaves only Exec's unconditional
// post-run checkGrowthCeiling call to classify the breach — which still
// sets WorkspaceFileCountExceeded=true, but only AFTER the command runs
// to its natural ~12-13s completion, not within the ~1-2s the proactive
// tick achieves. A loose bound (e.g. the ~10s ceiling this test used
// before the fix round) cannot tell those two apart; ~3s can.
func TestBwrapExec_FileCountKillFidelity(t *testing.T) {
	s := newTestBwrap(t, WithBwrapIdleTimeout(60*time.Second))
	s.defaultFileCountCeiling = 50 // tripped inside the first ~1s poll tick
	s.defaultGrowthCeilingBytes = 0
	t.Cleanup(func() { _ = s.Close() })

	start := time.Now()
	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		Timeout: 30 * time.Second,
		Cmd:     []string{"/bin/sh", "-c", bwrapFileCountFillerScript},
	})
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
	if elapsed > 3*time.Second {
		t.Errorf("elapsed = %s, want < 3s — the PROACTIVE tick must catch this, not the post-run check after a ~12-13s natural completion (would indicate the watchdogLimits.fileCountCeiling wiring was dropped)", elapsed)
	}
}

// TestBwrapExec_FileCountKillFidelity_SpawnGateWithIdleTimeoutUnset pins
// the watchdog spawn gate's third arm on the bwrap backend (bugbot-gb3o,
// extending bugbot-bdqf's mutation-M8 coverage): the goroutine must start
// from the file-count ceiling ALONE, with IdleTimeout AND the byte-size
// growth ceiling both completely unset.
func TestBwrapExec_FileCountKillFidelity_SpawnGateWithIdleTimeoutUnset(t *testing.T) {
	s := newTestBwrap(t) // base sets no IdleTimeout -> defaultIdleTimeout stays 0
	s.defaultFileCountCeiling = 50
	s.defaultGrowthCeilingBytes = 0
	t.Cleanup(func() { _ = s.Close() })

	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		Timeout: 30 * time.Second,
		Cmd:     []string{"/bin/sh", "-c", bwrapFileCountFillerScript},
	})
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

// TestBwrapExec_CallerCancellationWinsOverFileCountBreach is the
// bwrap-backend counterpart of cli_exec_quota_test.go's
// TestCLIExec_CallerCancellationWinsOverFileCountBreach: a caller
// cancellation landing in the same window as a real file-count breach must
// surface as "sandbox: execution cancelled", never as
// WorkspaceFileCountExceeded.
func TestBwrapExec_CallerCancellationWinsOverFileCountBreach(t *testing.T) {
	s := newTestBwrap(t, WithBwrapIdleTimeout(60*time.Second))
	s.defaultFileCountCeiling = 10 // a couple of filler iterations blow past this
	s.defaultGrowthCeilingBytes = 0
	t.Cleanup(func() { _ = s.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	res, err := s.Exec(ctx, Spec{
		RepoDir: t.TempDir(),
		Timeout: 30 * time.Second,
		Cmd:     []string{"/bin/sh", "-c", bwrapFileCountFillerScript},
	})
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

// TestBwrapExec_FileCountBaselineCapturedAfterWorkspacePrep is the bwrap
// counterpart of cli_exec_quota_test.go's
// TestCLIExec_FileCountBaselineCapturedAfterWorkspacePrep and the
// acceptance-mandated negative test: a normal repo copy holding many
// PRE-EXISTING files must NOT trip the ceiling — only entries created by
// the COMMAND itself, after the baseline snapshot, count. RepoDir is
// seeded with 500 pre-existing files against a 50-file ceiling; the
// command writes nothing, so any kill proves the baseline was wrong.
func TestBwrapExec_FileCountBaselineCapturedAfterWorkspacePrep(t *testing.T) {
	s := newTestBwrap(t)
	s.defaultFileCountCeiling = 50 // far below the seeded file count
	s.defaultGrowthCeilingBytes = 0
	t.Cleanup(func() { _ = s.Close() })

	repoDir := t.TempDir()
	for i := range 500 {
		if err := os.WriteFile(filepath.Join(repoDir, "f"+strconv.Itoa(i)+".txt"), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed repo content: %v", err)
		}
	}

	res, err := s.Exec(context.Background(), Spec{
		RepoDir: repoDir,
		Timeout: 15 * time.Second,
		Cmd:     []string{"/bin/sh", "-c", "true"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.WorkspaceFileCountExceeded {
		t.Errorf("pre-existing workspace content (500 files against a 50-file ceiling) must NOT trip the ceiling — the baseline must be captured AFTER workspace prep, not from zero; got %+v", res)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// TestBwrapExec_FileCountCeilingDisabled_RunsToCompletion is the
// bwrap-backend counterpart of cli_exec_quota_test.go's
// TestCLIExec_FileCountCeilingDisabled_RunsToCompletion: the negative
// control proving the fixture itself is sound (it is the CEILING, not
// some other mechanism, producing the kill in
// TestBwrapExec_FileCountKillFidelity) and that a disabled ceiling truly
// disables enforcement — the filler runs its full 1000-iteration loop to
// natural completion (~12-13s, well under the 45s Timeout) with the
// file-count and byte-size ceilings both off.
func TestBwrapExec_FileCountCeilingDisabled_RunsToCompletion(t *testing.T) {
	s := newTestBwrap(t)
	s.defaultFileCountCeiling = 0
	s.defaultGrowthCeilingBytes = 0
	t.Cleanup(func() { _ = s.Close() })

	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		Timeout: 45 * time.Second,
		Cmd:     []string{"/bin/sh", "-c", bwrapFileCountFillerScript},
	})
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

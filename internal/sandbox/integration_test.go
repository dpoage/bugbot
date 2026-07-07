//go:build integration

// Integration tests exercise the real container runtime. Run with:
//
//	go test -tags integration ./internal/sandbox/...
//
// They are skipped automatically when no runtime is detected or the test image
// cannot be pulled. Kept under ~60s total.
package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testImage = "docker.io/library/alpine:latest"

// newTestCLI builds a CLI against the detected runtime, skipping the test when
// none is available or the image cannot be pulled/used.
func newTestCLI(t *testing.T) *CLI {
	t.Helper()
	rt, ok := Detect()
	if !ok {
		t.Skip("no container runtime detected; skipping integration test")
	}
	s, err := NewCLI(rt, testImage,
		WithCPUs(1),
		WithMemoryMB(256),
		WithPidsLimit(128),
		WithTimeout(30*time.Second),
	)
	if err != nil {
		t.Skipf("NewCLI: %v", err)
	}
	ensureImage(t, s)
	return s
}

// ensureImage runs a trivial container to force an image pull up front; if it
// fails (e.g. no network to pull), the suite skips rather than failing.
func ensureImage(t *testing.T, s *CLI) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, err := s.Exec(ctx, Spec{RepoDir: t.TempDir(), Cmd: []string{"true"}})
	if err != nil {
		t.Skipf("cannot run test image %q (pull failed?): %v", testImage, err)
	}
}

func TestIntegrationEchoSucceeds(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		Cmd:     []string{"echo", "hi"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if strings.TrimSpace(res.Stdout) != "hi" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "hi")
	}
	if res.TimedOut {
		t.Error("unexpected TimedOut")
	}
}

func TestIntegrationNonZeroExit(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		Cmd:     []string{"sh", "-c", "exit 3"},
	})
	if err != nil {
		t.Fatalf("Exec returned error for non-zero exit (should be reported via ExitCode): %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestIntegrationWriteFilesInjected(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir:    t.TempDir(),
		Cmd:        []string{"cat", "repro/marker.txt"},
		WriteFiles: map[string][]byte{"repro/marker.txt": []byte("INJECTED")},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "INJECTED" {
		t.Errorf("Stdout = %q, want INJECTED", res.Stdout)
	}
}

// TestIntegrationCaptureFilesReadBack — bugbot-ym09 — the structured-output
// capture seam: a file the containerized command writes into the workspace is
// read back into Result.Captured before the workspace is torn down. Also
// covers the missing-file case in the same real-podman run: CaptureFiles asks
// for a second path the command never writes, and it must be silently absent
// rather than causing an error.
func TestIntegrationCaptureFilesReadBack(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir:      t.TempDir(),
		Cmd:          []string{"sh", "-c", "echo '<testsuites></testsuites>' > report.xml"},
		CaptureFiles: []string{"report.xml", "never-written.xml"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	got, present := res.Captured["report.xml"]
	if !present {
		t.Fatal("Captured[report.xml] absent, want the file the container wrote")
	}
	if strings.TrimSpace(string(got)) != "<testsuites></testsuites>" {
		t.Errorf("Captured[report.xml] = %q", got)
	}
	if _, present := res.Captured["never-written.xml"]; present {
		t.Error("Captured[never-written.xml] present, want silently absent")
	}
}

// TestIntegrationCaptureFilesSymlinkEscapeRejected — bugbot-ym09 review
// finding — proves the fix against a REAL container, not just the host-side
// unit tests: the sandboxed command plants a symlink at the capture path
// whose target is an absolute HOST path outside the workspace (created with
// full write access to its own workspace, which is all `ln -s` needs — the
// target need not even resolve inside the container's own mount namespace).
// After the container exits, readCaptureFile runs on the HOST and must
// refuse to follow that symlink: without the fix this would let a
// container-controlled repro plan read arbitrary host files back through
// Result.Captured.
func TestIntegrationCaptureFilesSymlinkEscapeRejected(t *testing.T) {
	s := newTestCLI(t)

	hostSecretDir := t.TempDir()
	hostSecret := filepath.Join(hostSecretDir, "secret.txt")
	if err := os.WriteFile(hostSecret, []byte("host-only-secret"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	res, err := s.Exec(context.Background(), Spec{
		RepoDir:      t.TempDir(),
		Cmd:          []string{"sh", "-c", "ln -s " + hostSecret + " report.xml"},
		CaptureFiles: []string{"report.xml"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	if got, present := res.Captured["report.xml"]; present {
		t.Fatalf("a symlink escaping the workspace to a host path must be refused, got %q (host secret leaked)", got)
	}
}

// TestIntegrationApplyWriteFilesSymlinkEscapeRejected — bugbot-6nqd — proves
// the WRITE-side symlink hardening against a REAL container and a REAL
// workspace REUSE across two Execs, exactly the reproducer's iteration shape
// (bugbot-bkz1):
// call 1's containerized command plants a symlink pointing at an absolute
// HOST path outside the workspace; call 2 asks WriteFiles to write to that
// same relative path. Without the fix, applyWriteFiles (running on the HOST,
// after call 1's container has already exited) would follow the symlink and
// overwrite the host file as the bugbot user — a full sandbox escape, strictly
// worse than the read-side bugbot-ym09 finding above since it is a HOST WRITE.
func TestIntegrationApplyWriteFilesSymlinkEscapeRejected(t *testing.T) {
	s := newTestCLI(t)

	repoDir := t.TempDir()
	ws, err := s.MaterializeWorkspace(repoDir)
	if err != nil {
		t.Fatalf("MaterializeWorkspace: %v", err)
	}
	defer func() { _ = os.RemoveAll(ws) }()

	hostDir := t.TempDir()
	hostTarget := filepath.Join(hostDir, "authorized_keys")
	if err := os.WriteFile(hostTarget, []byte("original\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Call 1 (iteration run #1): the container plants a symlink named
	// "leak" pointing at an absolute host path outside the workspace — all
	// `ln -s` needs is write access to its own workspace.
	res1, err := s.Exec(context.Background(), Spec{
		RepoDir:   repoDir,
		Workspace: ws,
		Cmd:       []string{"sh", "-c", "ln -s " + hostTarget + " leak"},
	})
	if err != nil {
		t.Fatalf("Exec (plant symlink): %v", err)
	}
	if res1.ExitCode != 0 {
		t.Fatalf("plant symlink ExitCode = %d, stderr=%q", res1.ExitCode, res1.Stderr)
	}

	// Call 2 (iteration run #2, SAME workspace): a WriteFiles entry
	// targets the exact relative path the container just planted a symlink at.
	_, err = s.Exec(context.Background(), Spec{
		RepoDir:    repoDir,
		Workspace:  ws,
		Cmd:        []string{"true"},
		WriteFiles: map[string][]byte{"leak": []byte("pwned\n")},
	})
	if err == nil {
		t.Fatal("Exec should have refused to write through the planted symlink")
	}

	got, readErr := os.ReadFile(hostTarget)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "original\n" {
		t.Errorf("host file must not have been overwritten through the symlink: got %q (sandbox escape)", got)
	}
}

func TestIntegrationTimeoutReapsContainer(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		Cmd:     []string{"sleep", "30"},
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("expected TimedOut=true, got %+v", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 on timeout", res.ExitCode)
	}
	if res.Duration > 10*time.Second {
		t.Errorf("Duration = %v, expected to be killed near the 2s timeout", res.Duration)
	}
}

func TestIntegrationNetworkBlocked(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		// wget should fail with --network=none (the default).
		Cmd:     []string{"wget", "-T", "3", "-q", "-O-", "http://example.com"},
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("network egress should be blocked but wget succeeded; stdout=%q", res.Stdout)
	}
	if res.TimedOut {
		t.Error("network test timed out unexpectedly; expected fast failure under --network=none")
	}
}

func TestIntegrationOriginalRepoReadOnly(t *testing.T) {
	s := newTestCLI(t)
	repo := t.TempDir()
	mustWrite(t, repo+"/original.txt", "orig", 0o644)

	res, err := s.Exec(context.Background(), Spec{
		RepoDir: repo,
		// Mutate the workspace copy; the host original must be untouched.
		Cmd: []string{"sh", "-c", "echo changed > /workspace/original.txt && echo new > /workspace/created.txt"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	assertFileContent(t, repo+"/original.txt", "orig")
	if _, statErr := os.Stat(repo + "/created.txt"); statErr == nil {
		t.Error("host repo was mutated: created.txt should not exist on host")
	}
}

// TestIntegrationIdleWatchdogKillsStall: a run that produces no output and no
// workspace writes is cancelled after the idle window, well before its generous
// absolute ceiling.
func TestIntegrationIdleWatchdogKillsStall(t *testing.T) {
	s := newTestCLI(t)
	start := time.Now()
	res, err := s.Exec(context.Background(), Spec{
		RepoDir:     t.TempDir(),
		Cmd:         []string{"sleep", "60"},
		Timeout:     50 * time.Second, // generous hard ceiling
		IdleTimeout: 2 * time.Second,  // no progress -> idle kill
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("expected TimedOut=true on idle stall, got %+v", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 on idle kill", res.ExitCode)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("idle watchdog took %v; expected a kill within a few idle windows, far under the 50s ceiling", d)
	}
}

// TestIntegrationIdleWatchdogAllowsProgress: a run whose total time far exceeds
// the idle window survives because it keeps writing to the workspace within each
// window — the dynamic timeout lets a slow-but-progressing build finish.
func TestIntegrationIdleWatchdogAllowsProgress(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		// ~6s total, a workspace write every 1s; idle window is 3s.
		Cmd:         []string{"sh", "-c", "i=0; while [ $i -lt 6 ]; do echo step$i >> /workspace/progress.log; i=$((i+1)); sleep 1; done"},
		Timeout:     50 * time.Second,
		IdleTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.TimedOut {
		t.Errorf("run made steady workspace progress but was killed as idle: %+v", res)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	if res.Duration < 3*time.Second {
		t.Errorf("Duration = %v; expected the full ~6s progressing run to complete", res.Duration)
	}
}

// TestIntegrationIdleWatchdogCPUBusySurvives: a run that produces NO output and
// NO workspace writes but pegs the CPU (a busy loop, standing in for a compiler
// churning silently on one large translation unit) is kept alive by the CPU
// fallback signal and completes, rather than being falsely idle-killed.
func TestIntegrationIdleWatchdogCPUBusySurvives(t *testing.T) {
	s := newTestCLI(t)
	res, err := s.Exec(context.Background(), Spec{
		RepoDir: t.TempDir(),
		// ~8s of pure CPU spin: no stdout, no filesystem writes. Only the CPU
		// probe can tell this apart from a hang.
		Cmd:         []string{"sh", "-c", "end=$(( $(date +%s) + 8 )); while [ $(date +%s) -lt $end ]; do :; done"},
		Timeout:     50 * time.Second,
		IdleTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.TimedOut {
		t.Errorf("CPU-busy run was falsely idle-killed despite high CPU: %+v", res)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
}

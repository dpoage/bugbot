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

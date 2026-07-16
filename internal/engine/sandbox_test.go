package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// writeTestFile writes content to path, creating parent directories, failing
// the test on any error.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestDepProbeInputsThreadsLocalMounts is the regression test for bugbot-48ya
// gap 3: the pre-fix hostToolchainProbeInputs only threaded HostToolchains,
// so a bwrap (or container) run configured with sandbox.local_mounts probed
// capabilities as if that mount did not exist. depProbeInputs must surface
// it so `bugbot doctor` / claim-time gating see the same mounts a real run
// would have.
func TestDepProbeInputsThreadsLocalMounts(t *testing.T) {
	repoDir := t.TempDir() // no ecosystem markers: dep resolution is a no-op
	mountDir := t.TempDir()

	var cfg config.Config
	cfg.Sandbox.LocalMounts = []config.LocalMount{{Host: mountDir, Container: "/sibling"}}

	sb := sandbox.NewMock(sandbox.MockResponse{})
	mounts, _ := depProbeInputs(cfg, sb, repoDir)

	found := false
	for _, m := range mounts {
		if m.ContainerPath == "/sibling" {
			found = true
			if m.HostPath != mountDir {
				t.Errorf("HostPath = %q, want %q", m.HostPath, mountDir)
			}
			if !m.Shared {
				t.Errorf("local mount Shared = false, want true (host-owned dir)")
			}
		}
	}
	if !found {
		t.Fatalf("depProbeInputs mounts = %+v, want a /sibling entry from sandbox.local_mounts", mounts)
	}
}

// TestDepProbeInputsThreadsDepStrategyEnv is the regression test for the
// other half of gap 3: dep-strategy-derived Env (and by extension ROMounts)
// never reached ProbeCapabilities before this fix. A vendored Go repo is a
// deterministic, host-independent way to prove Env threading end-to-end
// without requiring a real Go toolchain or module cache on the test host.
func TestDepProbeInputsThreadsDepStrategyEnv(t *testing.T) {
	repoDir := t.TempDir()
	writeTestFile(t, filepath.Join(repoDir, "go.mod"), "module example.com/x\n\ngo 1.22\n")
	writeTestFile(t, filepath.Join(repoDir, "vendor", "modules.txt"), "# example.com/dep v1.0.0\n")

	var cfg config.Config
	sb := sandbox.NewMock(sandbox.MockResponse{})
	_, env := depProbeInputs(cfg, sb, repoDir)

	wantEnv := "GOFLAGS=-mod=vendor"
	found := false
	for _, e := range env {
		if e == wantEnv {
			found = true
		}
	}
	if !found {
		t.Fatalf("depProbeInputs env = %v, want it to contain %q (vendored Go detection)", env, wantEnv)
	}
}

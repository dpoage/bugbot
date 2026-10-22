//go:build integration

package sandboxenv

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/llmkit/sandbox"
)

// TestProbeCapabilitiesIntegration_GoImage verifies that the Go official image
// has the race detector available (cgo + C compiler present), and that a
// minimal Go-only slim image does not.
//
// Requires podman or docker on PATH and network access to pull images.
// Run with: go test -tags=integration ./internal/sandboxenv/...
func TestProbeCapabilitiesIntegration_GoImage(t *testing.T) {
	runtime, ok := sandbox.Detect()
	if !ok {
		t.Skip("no container runtime (podman/docker) on PATH")
	}

	repoDir := t.TempDir()
	ctx := context.Background()

	t.Run("golang_official_race_available", func(t *testing.T) {
		const goImage = "docker.io/library/golang:1.22-bookworm"
		sb, err := sandbox.NewCLI(sandbox.WithRuntime(runtime), sandbox.WithImage(goImage))
		if err != nil {
			t.Fatalf("NewCLI: %v", err)
		}
		cs, err := ProbeCapabilities(ctx, sb, config.Sandbox{Image: goImage}, sandbox.Spec{RepoDir: repoDir})
		if err != nil {
			t.Fatalf("ProbeCapabilities: %v", err)
		}
		if !cs.Available("go", "race") {
			t.Errorf("expected race=true for %s, got caps=%v", goImage, cs)
		}
	})

	t.Run("debian_slim_race_unavailable", func(t *testing.T) {
		const slimImage = "docker.io/library/debian:stable-slim"
		sb, err := sandbox.NewCLI(sandbox.WithRuntime(runtime), sandbox.WithImage(slimImage))
		if err != nil {
			t.Fatalf("NewCLI: %v", err)
		}
		cs, err := ProbeCapabilities(ctx, sb, config.Sandbox{Image: slimImage}, sandbox.Spec{RepoDir: repoDir})
		if err != nil {
			t.Fatalf("ProbeCapabilities: %v", err)
		}
		if cs.Available("go", "race") {
			t.Errorf("expected race=false for %s (no Go toolchain), got caps=%v", slimImage, cs)
		}
	})
}

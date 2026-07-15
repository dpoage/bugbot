//go:build integration

package sandbox

import (
	"context"
	"testing"
)

// TestProbeCapabilitiesIntegration_GoImage verifies that the Go official image
// has the race detector available (cgo + C compiler present), and that a
// minimal Go-only slim image does not.
//
// Requires podman or docker on PATH and network access to pull images.
// Run with: go test -tags=integration ./internal/sandbox/...
func TestProbeCapabilitiesIntegration_GoImage(t *testing.T) {
	runtime, ok := Detect()
	if !ok {
		t.Skip("no container runtime (podman/docker) on PATH")
	}

	repoDir := t.TempDir()
	ctx := context.Background()

	t.Run("golang_official_race_available", func(t *testing.T) {
		const goImage = "docker.io/library/golang:1.22-bookworm"
		InvalidateCapabilityCache(goImage)
		sb, err := NewCLI(runtime, goImage)
		if err != nil {
			t.Fatalf("NewCLI: %v", err)
		}
		cs := ProbeCapabilities(ctx, sb, goImage, repoDir, nil, nil)
		if !cs.Available("go", "race") {
			t.Errorf("expected race=true for %s, got caps=%v", goImage, cs)
		}
	})

	t.Run("debian_slim_race_unavailable", func(t *testing.T) {
		const slimImage = "docker.io/library/debian:stable-slim"
		InvalidateCapabilityCache(slimImage)
		sb, err := NewCLI(runtime, slimImage)
		if err != nil {
			t.Fatalf("NewCLI: %v", err)
		}
		cs := ProbeCapabilities(ctx, sb, slimImage, repoDir, nil, nil)
		if cs.Available("go", "race") {
			t.Errorf("expected race=false for %s (no Go toolchain), got caps=%v", slimImage, cs)
		}
	})
}

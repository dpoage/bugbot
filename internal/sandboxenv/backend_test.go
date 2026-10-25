package sandboxenv

import (
	"strings"
	"testing"

	"github.com/dpoage/llmkit/sandbox"

	"github.com/dpoage/bugbot/internal/config"
)

// Every case fails before any runtime lookup, so the test is hermetic.
func TestNew_RefusesKnobsLlmkitCannotHonor(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Sandbox)
		want   string
	}{
		{"unresolved host toolchain", func(s *config.Sandbox) { s.HostToolchains = []string{"bugbot-no-such-toolchain-xyz"} },
			"sandbox.host_toolchains: bugbot-no-such-toolchain-xyz did not resolve"},
		{"network the backend cannot honor", func(s *config.Sandbox) { s.Network = "slirp4netns" },
			`Spec.Network = "slirp4netns"`},
		{"zero cpus", func(s *config.Sandbox) { s.CPUs = 0 }, "WithCPUs(0)"},
		{"negative growth ceiling", func(s *config.Sandbox) { s.WorkspaceGrowthCeilingMB = -1 }, "WithWorkspaceGrowthCeilingMB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default().Sandbox
			tc.mutate(&cfg)
			sb, err := New(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New() = %T, %v; want an error containing %q", sb, err, tc.want)
			}
			if sb != nil {
				t.Errorf("New() returned a non-nil Sandbox %T alongside its error", sb)
			}
		})
	}
}

func TestRunImage_EmptyOffContainerBackend(t *testing.T) {
	cfg := config.Sandbox{Image: "docker.io/library/debian:stable-slim"}
	if got := RunImage(&sandbox.CLI{}, cfg); got != cfg.Image {
		t.Errorf("RunImage(CLI) = %q, want %q", got, cfg.Image)
	}
	for _, sb := range []sandbox.Sandbox{&sandbox.Bwrap{}, sandbox.NewHostExec(), sandbox.NewMock(sandbox.MockResponse{})} {
		if got := RunImage(sb, cfg); got != "" {
			t.Errorf("RunImage(%T) = %q, want empty (Bwrap/HostExec refuse a Spec.Image)", sb, got)
		}
	}
}

// The unsandboxed backend value is never an isolating backend — New
// refuses it, Available is false, and the label names the mode.
func TestUnsandboxedBackendIsNeverIsolating(t *testing.T) {
	cfg := config.Default().Sandbox
	cfg.Backend = BackendUnsandboxed

	if Available(cfg) {
		t.Error("Available(unsandboxed) = true, want false")
	}
	if got := BackendLabel(cfg); got != "unsandboxed" {
		t.Errorf("BackendLabel(unsandboxed) = %q, want unsandboxed", got)
	}
	sb, err := New(cfg)
	if err == nil || !strings.Contains(err.Error(), "unsandboxed") {
		t.Fatalf("New(unsandboxed) = %T, %v; want an error naming the mode", sb, err)
	}
	if sb != nil {
		t.Errorf("New() returned a non-nil Sandbox %T alongside its error", sb)
	}
}

func TestUnavailableReason(t *testing.T) {
	cfg := config.Default().Sandbox
	cfg.Backend = "podman"
	cfg.Runtime = "bugbot-no-such-runtime-ypei"
	if got := UnavailableReason(cfg); got != "bugbot-no-such-runtime-ypei not on PATH" {
		t.Errorf("UnavailableReason(runtime set) = %q", got)
	}
	cfg.Runtime = ""
	if got := UnavailableReason(cfg); got != "podman/docker not on PATH" {
		t.Errorf("UnavailableReason(autodetect miss) = %q, want podman/docker not on PATH", got)
	}
	// Only assert the bwrap branch when this host genuinely cannot bwrap;
	// a bwrap-capable host correctly yields "".
	if bwrapOK, _ := sandbox.DetectBwrap(); !bwrapOK {
		if got := UnavailableReason(config.Sandbox{Backend: BackendBwrap}); !strings.HasPrefix(got, "bwrap unusable (") {
			t.Errorf("UnavailableReason(bwrap) = %q, want bwrap unusable (<reason>)", got)
		}
	}
}

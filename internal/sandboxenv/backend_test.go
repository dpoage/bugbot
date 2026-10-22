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

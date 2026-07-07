//go:build integration

package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// TestRepro_ResolvesEmptyTargetAgainstRealBacklog is the regression test the
// oracle review of bugbot-pt83 asked for: it seeds one real, eligible T2
// backlog finding and drives Dispatcher.Repro all the way to
// BuildReproducer/repro.New with an EMPTY opts.Target — exactly what the TUI
// dispatch palette sends — against a real container runtime. Before the fix
// this failed every time with "repro: empty repoDir" once execution reached
// repro.New; the plain-unit tests in repro_test.go cannot prove this alone
// because an empty backlog short-circuits before opts.Target is ever
// forwarded downstream. Gated behind -tags integration (real container
// execution) per the repo's existing convention — see
// internal/repro/integration_test.go for the sibling pattern.
func TestRepro_ResolvesEmptyTargetAgainstRealBacklog(t *testing.T) {
	if _, ok := sandbox.Detect(); !ok {
		t.Skip("no container runtime (podman/docker) detected")
	}
	const image = "docker.io/library/golang:1.26-alpine"
	if _, err := sandbox.NewCLI("", image); err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}

	cfg := testConfig(t)
	cfg.Sandbox.Image = image
	cfg.Sandbox.DepStrategy = "off"
	t.Setenv("BUGBOT_TEST_REPRO_KEY", "sk-test-unused")
	cfg.Providers = map[string]config.Provider{
		"test": {Type: config.ProviderAnthropic, APIKeyEnv: "BUGBOT_TEST_REPRO_KEY"},
	}
	cfg.Roles.Reproducer = config.RoleModel{Provider: "test", Model: "claude-test"}

	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Second)
	defer cancel()

	d, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()

	fp := domain.Fingerprint("logic", "internal/engine/repro.go", "bugbot-pt83-seed")
	if _, err := d.store.UpsertFinding(ctx, domain.Finding{
		Fingerprint: fp,
		Title:       "bugbot-pt83 regression seed finding",
		Description: "Seeded solely to populate OpenBacklog for this Dispatcher.Repro integration test.",
		Tier:        domain.TierVerified,
		Status:      domain.StatusOpen,
		Lens:        "logic",
		File:        "internal/engine/repro.go",
		Line:        1,
	}); err != nil {
		t.Fatalf("UpsertFinding() error = %v", err)
	}

	var out strings.Builder
	_, err = d.Repro(ctx, ReproOpts{Target: "", Out: &out})

	t.Logf("Repro(Target=\"\") err=%v out=%q", err, out.String())
	// The LLM client here is a stub with no real credentials/endpoint, so a
	// non-nil error is expected once the reproducer agent actually tries to
	// call it (or from downstream sandbox/dependency work). What matters for
	// this regression test is that the failure — if any — is NOT the
	// bugbot-pt83 symptom: an empty repoDir rejected before BuildReproducer
	// ever got a real target.
	if err != nil && strings.Contains(err.Error(), "empty repoDir") {
		t.Fatalf("Repro(Target=\"\") regressed to the bugbot-pt83 empty-repoDir failure: %v (output=%q)", err, out.String())
	}
	if strings.Contains(out.String(), "empty repoDir") {
		t.Fatalf("Repro(Target=\"\") output mentions empty repoDir: %q", out.String())
	}
}

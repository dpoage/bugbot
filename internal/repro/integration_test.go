//go:build integration

package repro

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// TestIntegration_RealSandboxPromotion exercises the full repro execution path
// against a real container runtime: a tiny seeded-bug Go module is reproduced
// by a HAND-WRITTEN plan (the LLM is bypassed by scripting the fake client with
// the real test content) and run via `go test` in a golang image. We assert a
// real non-zero exit and promotion to Tier-1.
//
// Skips cleanly when no runtime is available or the image pull fails. Kept well
// under 90s with a tight timeout.
func TestIntegration_RealSandboxPromotion(t *testing.T) {
	if _, ok := sandbox.Detect(); !ok {
		t.Skip("no container runtime (podman/docker) detected")
	}

	const image = "docker.io/library/golang:1.26-alpine"

	sb, err := sandbox.NewCLI("", image)
	if err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}

	// Seed a tiny Go module with a real bug: Divide returns 0 on a zero divisor
	// instead of signalling an error, so a test asserting the error fails.
	repoDir := t.TempDir()
	writeFile(t, repoDir, "go.mod", "module bugfixture\n\ngo 1.21\n")
	writeFile(t, repoDir, "calc.go", `package bugfixture

// Divide is buggy: it silently returns 0 (and ok=true) for a zero divisor
// instead of reporting failure.
func Divide(a, b int) (int, bool) {
	if b == 0 {
		return 0, true // BUG: should report failure
	}
	return a / b, true
}
`)

	// Hand-written repro plan: a standard Go test that FAILS on the buggy code
	// (ok should be false for a zero divisor) and PASSES once fixed.
	plan := Plan{
		Files: map[string]string{
			"calc_repro_test.go": `package bugfixture

import "testing"

func TestDivideByZeroReported(t *testing.T) {
	if _, ok := Divide(1, 0); ok {
		t.Fatalf("Divide(1,0) reported ok=true; division by zero must fail")
	}
}
`,
		},
		Cmd:    []string{"go", "test", "-timeout", "60s", "-run", "TestDivideByZeroReported", "./..."},
		Expect: "TestDivideByZeroReported fails because Divide(1,0) returns ok=true",
	}

	client := newScriptedClient(planBody(t, plan))

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	fp := domain.Fingerprint("logic", "calc.go", fmt.Sprintf("%d|%s", 7, "Divide ignores zero divisor"))
	finding, err := st.UpsertFinding(context.Background(), domain.Finding{
		Fingerprint: fp,
		Title:       "Divide ignores zero divisor",
		Description: "Divide returns ok=true for a zero divisor.",
		Severity:    "high",
		Tier:        2,
		Status:      domain.StatusOpen,
		Lens:        "logic",
		File:        "calc.go",
		Line:        7,
		CommitSHA:   "fixture",
		FileHash:    "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}

	artifactDir := t.TempDir()
	r, err := New(client, sb, repoDir, Options{
		ArtifactDir: artifactDir,
		Image:       image,
		Timeout:     80 * time.Second,
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Second)
	defer cancel()

	att, err := r.Attempt(ctx, finding)
	if err != nil {
		// A pull failure surfaces here; skip rather than fail.
		if strings.Contains(err.Error(), "pull") || strings.Contains(err.Error(), "manifest") || strings.Contains(err.Error(), "image") {
			t.Skipf("image pull failed, skipping: %v", err)
		}
		t.Fatalf("Attempt: %v", err)
	}

	if !att.Promoted {
		t.Fatalf("expected real promotion, got reason=%q output=%q", att.Reason, att.Output)
	}
	// Promotion alone is not proof the repro ran: an environment failure also
	// exits non-zero. Require evidence the seeded test itself executed and
	// failed (this test once passed on "failed to initialize build cache").
	if !strings.Contains(att.Output, "TestDivideByZeroReported") {
		t.Fatalf("promotion output does not show the repro test running; output=%q", att.Output)
	}
	if att.ArtifactPath == "" {
		t.Fatal("expected artifact path")
	}
	if _, statErr := os.Stat(filepath.Join(att.ArtifactPath, "README.md")); statErr != nil {
		t.Errorf("missing README: %v", statErr)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

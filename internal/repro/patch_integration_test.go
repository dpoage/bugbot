//go:build integration

package repro

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// TestIntegration_PatchProver exercises the full patch-prover execution path
// against a real container runtime.  The pipeline:
//  1. Seeds a tiny Go module with a real bug (Divide ignores zero divisor).
//  2. Bypasses the LLM repro agent with a hand-written repro plan.
//  3. Runs the repro in the sandbox to confirm the bug (T1 promotion).
//  4. Bypasses the LLM patch agent with a hand-written patch plan.
//  5. Runs the patch-prover to confirm the fix (T0 promotion).
//
// Skips cleanly when no runtime is available or the image pull fails.
func TestIntegration_PatchProver(t *testing.T) {
	if _, ok := sandbox.Detect(); !ok {
		t.Skip("no container runtime (podman/docker) detected")
	}

	const image = "docker.io/library/golang:1.26-alpine"

	sb, err := sandbox.NewCLI("", image)
	if err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}

	// Seed a tiny Go module with a real bug.
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

	// Hand-written repro plan that FAILS on the buggy code.
	reproPlan := Plan{
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
		Cmd:    []string{"go", "test", "-run", "TestDivideByZeroReported", "./..."},
		Expect: "TestDivideByZeroReported fails because Divide(1,0) returns ok=true",
	}

	// Hand-written patch plan that FIXES the bug.
	patchPlan := PatchPlan{
		Files: map[string]string{
			"calc.go": `package bugfixture

// Divide returns ok=false when b is zero.
func Divide(a, b int) (int, bool) {
	if b == 0 {
		return 0, false // fixed
	}
	return a / b, true
}
`,
		},
		Summary: "Add zero-divisor guard to Divide",
	}

	// Open the store and seed a T2 finding.
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	fp := store.Fingerprint("logic", "calc.go", 7, "Divide ignores zero divisor")
	finding, err := st.UpsertFinding(context.Background(), store.Finding{
		Fingerprint: fp,
		Title:       "Divide ignores zero divisor",
		Description: "Divide returns ok=true for a zero divisor.",
		Severity:    "high",
		Tier:        2,
		Status:      store.StatusOpen,
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

	// Script both the repro and patch plans via the fake LLM client.
	client := newScriptedClient(
		planBody(t, reproPlan),
		patchPlanBody(t, patchPlan),
	)

	r, err := New(client, sb, repoDir, Options{
		ArtifactDir:      artifactDir,
		Image:            image,
		Timeout:          80 * time.Second,
		MaxAttempts:      1,
		PatchProver:      true,
		PatchMaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	summary, err := r.PromoteAll(ctx, st, []store.Finding{finding})
	if err != nil {
		// Image pull failure: skip rather than fail.
		if strings.Contains(err.Error(), "pull") || strings.Contains(err.Error(), "manifest") || strings.Contains(err.Error(), "image") {
			t.Skipf("image pull failed, skipping: %v", err)
		}
		t.Fatalf("PromoteAll: %v", err)
	}

	if summary.Promoted != 1 {
		t.Errorf("Promoted = %d, want 1", summary.Promoted)
	}
	if summary.FixWitnessed != 1 {
		t.Errorf("FixWitnessed = %d, want 1", summary.FixWitnessed)
	}

	// Finding must be promoted to T0 in the store.
	got, err := st.GetFindingByFingerprint(context.Background(), finding.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if got.Tier != 0 {
		t.Errorf("tier = %d, want 0", got.Tier)
	}

	// Artifact patch.diff must exist in the bundle directory.
	bundleDir := filepath.Join(artifactDir, finding.ID)
	diffPath := filepath.Join(bundleDir, "patch.diff")
	if _, err := os.Stat(diffPath); err != nil {
		t.Errorf("patch.diff missing from artifact bundle: %v", err)
	}
}

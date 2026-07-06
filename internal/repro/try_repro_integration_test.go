//go:build integration

package repro

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// tryReproIntegrationImage matches patch_integration_test.go's image so both
// integration suites share the same warm local pull.
const tryReproIntegrationImage = "docker.io/library/golang:1.26-alpine"

// newTryReproFinding builds a minimal Tier-2 finding for the try_repro
// integration tests below; they call Attempt directly (no store) so only the
// fields Attempt/buildTask actually read need to be populated.
func newTryReproFinding() domain.Finding {
	return domain.Finding{
		ID:          "tryrepro-fixture",
		Fingerprint: domain.Fingerprint("logic", "calc.go", fmt.Sprintf("%d|%s", 7, "Divide ignores zero divisor")),
		Title:       "Divide ignores zero divisor",
		Description: "Divide returns 0 for a zero divisor instead of an error.",
		Severity:    "high",
		Tier:        2,
		Status:      domain.StatusOpen,
		Lens:        "logic",
		File:        "calc.go",
		Line:        7,
		CommitSHA:   "fixture",
		FileHash:    "fixture",
	}
}

// TestIntegration_TryReproFixesCompileErrorWithinOneAttempt exercises the
// core bugbot-bkz1 claim against a real container runtime: a candidate with a
// compile error is fixed via two try_repro iterations WITHIN A SINGLE outer
// Attempt round (MaxAttempts: 1 — a second outer round would mean the fix
// happened via the old blind-revision path, not the interactive loop).
func TestIntegration_TryReproFixesCompileErrorWithinOneAttempt(t *testing.T) {
	if _, ok := sandbox.Detect(); !ok {
		t.Skip("no container runtime (podman/docker) detected")
	}
	sb, err := sandbox.NewCLI("", tryReproIntegrationImage)
	if err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}
	defer func() { _ = sb.Close() }()

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

	cmd := []string{"go", "test", "-timeout", "60s", "-run", "TestDivideByZeroReported", "./..."}

	// Candidate 1 (via try_repro): a SYNTAX ERROR, so the sandbox never even
	// reaches the test runner (a real compile failure).
	brokenFiles := map[string]string{
		"calc_repro_test.go": `package bugfixture

import "testing"

func TestDivideByZeroReported(t *testing.T) {
	if _, ok := Divide(1, 0; ok {
		t.Fatalf("Divide(1,0) reported ok=true; division by zero must fail")
	}
}
`,
	}
	// Candidate 2 (via try_repro, same iteration workspace): the syntax fixed
	// and genuinely failing on the current buggy code.
	fixedFiles := map[string]string{
		"calc_repro_test.go": `package bugfixture

import "testing"

func TestDivideByZeroReported(t *testing.T) {
	if _, ok := Divide(1, 0); ok {
		t.Fatalf("Divide(1,0) reported ok=true; division by zero must fail")
	}
}
`,
	}
	finalPlan := Plan{
		Files:  fixedFiles,
		Cmd:    cmd,
		Expect: "TestDivideByZeroReported fails because Divide(1,0) returns ok=true",
	}

	client := newToolScriptedClient(
		toolCallStep("c1", "try_repro", string(mustArgs(t, brokenFiles, cmd))),
		toolCallStep("c2", "try_repro", string(mustArgs(t, fixedFiles, cmd))),
		textStep(planBody(t, finalPlan)),
	)

	r, err := New(client, sb, repoDir, Options{
		Image:       tryReproIntegrationImage,
		Timeout:     80 * time.Second,
		MaxAttempts: 1,
		ArtifactDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	att, err := r.Attempt(ctx, newTryReproFinding())
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if !att.Promoted {
		t.Fatalf("Attempt not promoted: %+v", att)
	}
	if att.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (the fix must land within try_repro iteration, not an outer revision round)", att.Attempts)
	}
}

// TestIntegration_TryReproCleanRoomIndependence proves the clean-room
// guarantee against a real container runtime: a file planted via try_repro
// into the ATTEMPT's iteration workspace is NOT visible to the official
// verdict run, which always executes against a brand-new workspace. The
// final plan's test asserts the marker is ABSENT; if isolation ever broke
// (the official run reused the iteration workspace) the marker would be
// present, the assertion would genuinely FAIL, and the finding would be
// wrongly promoted — so Promoted == false here is exactly the property under
// test.
func TestIntegration_TryReproCleanRoomIndependence(t *testing.T) {
	if _, ok := sandbox.Detect(); !ok {
		t.Skip("no container runtime (podman/docker) detected")
	}
	sb, err := sandbox.NewCLI("", tryReproIntegrationImage)
	if err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}
	defer func() { _ = sb.Close() }()

	repoDir := t.TempDir()
	writeFile(t, repoDir, "go.mod", "module bugfixture\n\ngo 1.21\n")

	markerFiles := map[string]string{"marker.txt": "leaked-if-you-can-see-this"}
	markerCmd := []string{"cat", "marker.txt"}

	canaryFiles := map[string]string{
		"marker_canary_test.go": `package bugfixture

import (
	"os"
	"testing"
)

// TestMarkerAbsent must PASS in a genuinely clean workspace: marker.txt was
// only ever written into the (now-discarded) try_repro iteration workspace,
// never into the repo the official verdict copies from.
func TestMarkerAbsent(t *testing.T) {
	if _, err := os.Stat("marker.txt"); err == nil {
		t.Fatal("marker.txt leaked from the try_repro iteration workspace into the clean-room run")
	}
}
`,
	}
	canaryCmd := []string{"go", "test", "-timeout", "60s", "-run", "TestMarkerAbsent", "./..."}
	finalPlan := Plan{
		Files:  canaryFiles,
		Cmd:    canaryCmd,
		Expect: "canary: marker.txt must not exist in the official verdict's workspace",
	}

	client := newToolScriptedClient(
		toolCallStep("c1", "try_repro", string(mustArgs(t, markerFiles, markerCmd))),
		textStep(planBody(t, finalPlan)),
	)

	r, err := New(client, sb, repoDir, Options{
		Image:       tryReproIntegrationImage,
		Timeout:     80 * time.Second,
		MaxAttempts: 1,
		ArtifactDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	att, err := r.Attempt(ctx, newTryReproFinding())
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if att.Promoted {
		t.Fatalf("Attempt was promoted, meaning marker.txt leaked from the iteration workspace into the official clean-room run: %+v", att)
	}
	if att.Reason != string(VerdictReasonExitZero) {
		t.Errorf("Reason = %q, want %q (the canary test should have PASSED in a genuinely clean workspace)", att.Reason, VerdictReasonExitZero)
	}
}

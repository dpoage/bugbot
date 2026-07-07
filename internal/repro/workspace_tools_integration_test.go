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

// workspaceIntegrationImage matches patch_integration_test.go's image so both
// integration suites share the same warm local pull.
const workspaceIntegrationImage = "docker.io/library/golang:1.26-alpine"

// newWorkspaceFinding builds a minimal Tier-2 finding for the workspace-tool
// integration tests below; they call Attempt directly (no store) so only the
// fields Attempt/buildTask actually read need to be populated.
func newWorkspaceFinding() domain.Finding {
	return domain.Finding{
		ID:          "workspace-fixture",
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

// TestIntegration_WorkspaceFixesCompileErrorWithinOneAttempt exercises the
// core bugbot-bkz1/bugbot-hu59 claim against a real container runtime: a
// candidate with a compile error is fixed by OVERWRITING the file via
// write_repro_file and re-running — two run_repro iterations WITHIN A SINGLE
// outer Attempt round (MaxAttempts: 1 — a second outer round would mean the
// fix happened via the old blind-revision path, not the interactive loop).
// The final plan carries cmd ONLY: the workspace registry supplies the file,
// proving the workspace-as-proof submission end-to-end.
func TestIntegration_WorkspaceFixesCompileErrorWithinOneAttempt(t *testing.T) {
	if _, ok := sandbox.Detect(); !ok {
		t.Skip("no container runtime (podman/docker) detected")
	}
	sb, err := sandbox.NewCLI("", workspaceIntegrationImage)
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

	// Candidate 1: a SYNTAX ERROR, so the sandbox never even reaches the test
	// runner (a real compile failure).
	brokenTest := `package bugfixture

import "testing"

func TestDivideByZeroReported(t *testing.T) {
	if _, ok := Divide(1, 0; ok {
		t.Fatalf("Divide(1,0) reported ok=true; division by zero must fail")
	}
}
`
	// Candidate 2 (same path — write_repro_file overwrite IS the edit): the
	// syntax fixed and genuinely failing on the current buggy code.
	fixedTest := `package bugfixture

import "testing"

func TestDivideByZeroReported(t *testing.T) {
	if _, ok := Divide(1, 0); ok {
		t.Fatalf("Divide(1,0) reported ok=true; division by zero must fail")
	}
}
`

	client := newToolScriptedClient(
		toolCallStep("c1", "write_repro_file", string(mustWriteArgs(t, "calc_repro_test.go", brokenTest))),
		toolCallStep("c2", "run_repro", string(mustCmdArgs(t, cmd))),
		toolCallStep("c3", "write_repro_file", string(mustWriteArgs(t, "calc_repro_test.go", fixedTest))),
		toolCallStep("c4", "run_repro", string(mustCmdArgs(t, cmd))),
		// cmd-only final plan: the workspace registry is the proof.
		textStep(`{"cmd":["go","test","-timeout","60s","-run","TestDivideByZeroReported","./..."],"expect":"TestDivideByZeroReported fails because Divide(1,0) returns ok=true"}`),
	)

	r, err := New(client, sb, repoDir, Options{
		Image:       workspaceIntegrationImage,
		Timeout:     80 * time.Second,
		MaxAttempts: 1,
		ArtifactDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	att, err := r.Attempt(ctx, newWorkspaceFinding())
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if !att.Promoted {
		t.Fatalf("Attempt not promoted: %+v", att)
	}
	if att.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (the fix must land within run_repro iteration, not an outer revision round)", att.Attempts)
	}
	if got := att.Plan.Files["calc_repro_test.go"]; got != fixedTest {
		t.Errorf("promoted plan carries the wrong file contents (want the FIXED candidate from the workspace registry); got:\n%s", got)
	}
}

// TestIntegration_WorkspaceCleanRoomIndependence proves the clean-room
// guarantee against a real container runtime under workspace-as-proof
// semantics: TRACKED files (write_repro_file) carry into the official verdict
// by design, but COMMAND SIDE EFFECTS do not — a marker file created by
// run_repro's cmd inside the iteration workspace must NOT be visible to the
// official run, which executes against a brand-new workspace containing the
// repo plus only the tracked files. The final plan's canary test asserts the
// marker is ABSENT; if isolation ever broke (the official run reused the
// iteration workspace) the marker would be present, the canary would
// genuinely FAIL, and the finding would be wrongly promoted — so
// Promoted == false here is exactly the property under test.
func TestIntegration_WorkspaceCleanRoomIndependence(t *testing.T) {
	if _, ok := sandbox.Detect(); !ok {
		t.Skip("no container runtime (podman/docker) detected")
	}
	sb, err := sandbox.NewCLI("", workspaceIntegrationImage)
	if err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}
	defer func() { _ = sb.Close() }()

	repoDir := t.TempDir()
	writeFile(t, repoDir, "go.mod", "module bugfixture\n\ngo 1.21\n")

	canaryTest := `package bugfixture

import (
	"os"
	"testing"
)

// TestMarkerAbsent must PASS in a genuinely clean workspace: marker.txt was
// only ever created as a side effect of a run_repro cmd inside the
// (now-discarded) iteration workspace, never tracked via write_repro_file and
// never present in the repo the official verdict copies from.
func TestMarkerAbsent(t *testing.T) {
	if _, err := os.Stat("marker.txt"); err == nil {
		t.Fatal("marker.txt leaked from the iteration workspace into the clean-room run")
	}
}
`

	client := newToolScriptedClient(
		// Plant the marker as a COMMAND SIDE EFFECT (not a tracked write).
		toolCallStep("c1", "run_repro", string(mustCmdArgs(t, []string{"bash", "-c", "echo leaked-if-you-can-see-this > marker.txt && cat marker.txt"}))),
		// Track the canary; it becomes the submitted proof.
		toolCallStep("c2", "write_repro_file", string(mustWriteArgs(t, "marker_canary_test.go", canaryTest))),
		textStep(`{"cmd":["go","test","-timeout","60s","-run","TestMarkerAbsent","./..."],"expect":"canary: marker.txt must not exist in the official verdict's workspace"}`),
	)

	r, err := New(client, sb, repoDir, Options{
		Image:       workspaceIntegrationImage,
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

	att, err := r.Attempt(ctx, newWorkspaceFinding())
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

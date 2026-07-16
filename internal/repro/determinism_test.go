package repro

import (
	"context"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// bugbot-c49s: interpret()==demonstrated used to promote off a SINGLE
// sandbox run. Concurrency/race findings are this stage's core target
// class, and are exactly the ones prone to a single lucky failure — one
// non-deterministic pass would otherwise mint a Tier-1 artifact that may
// not reproduce for a human. These tests cover the acceptance criteria
// verbatim: promotion requires two CONSECUTIVE demonstrating runs; an
// alternating fail/pass sandbox yields no promotion and a flake-specific
// reason; a deterministic repro pays exactly one extra sandbox run, and a
// non-demonstrating first run pays zero extra runs.

// TestAttempt_DeterministicRepro_PromotesWithOneExtraRun asserts that a
// repro that demonstrates on both the official run and the identical
// confirmation re-run promotes, and pays exactly ONE extra sandbox call to
// do so (the mock's default response repeats indefinitely, so both calls
// see the same demonstrating result — this is the "deterministic repro"
// case).
func TestAttempt_DeterministicRepro_PromotesWithOneExtraRun(t *testing.T) {
	repoDir := newRepoDir(t)
	client := newScriptedClient(planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\n    bug_test.go:1: Divide(1,0) = 0, want error\nFAIL",
	}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir(), MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}

	finding := domain.Finding{ID: "f-det", Title: "deterministic race", File: "calc.go", Line: 12}
	att, err := r.Attempt(context.Background(), finding)
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if !att.Promoted {
		t.Fatalf("Attempt = %+v, want Promoted (both runs demonstrate)", att)
	}
	if got := sb.CallCount(); got != 2 {
		t.Errorf("sandbox calls = %d, want 2 (official run + exactly one determinism confirmation)", got)
	}
}

// TestAttempt_NonDemonstratingFirstRun_PaysZeroExtraRuns asserts the other
// half of the cost contract: a first run that does NOT demonstrate never
// pays for a confirmation run at all.
func TestAttempt_NonDemonstratingFirstRun_PaysZeroExtraRuns(t *testing.T) {
	repoDir := newRepoDir(t)
	client := newScriptedClient(planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok\nPASS"}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir(), MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}

	finding := domain.Finding{ID: "f-clean", Title: "no bug here", File: "calc.go", Line: 12}
	att, err := r.Attempt(context.Background(), finding)
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if att.Promoted {
		t.Fatalf("Attempt = %+v, want not promoted (exit 0 never demonstrates)", att)
	}
	if got := sb.CallCount(); got != 1 {
		t.Errorf("sandbox calls = %d, want 1 (a non-demonstrating first run pays zero extra runs)", got)
	}
}

// TestAttempt_FlakyRepro_AlternatingFailPass_NoPromotion is the bead's
// central acceptance criterion: a sandbox mock alternating
// fail(demonstrate)/pass(does not demonstrate) never promotes, on any
// round, and the verdict carries the dedicated flake-specific reason
// (VerdictReasonFlaky) whose feedback text is actionable (mentions
// tightening synchronization / iterations) rather than the generic
// not-demonstrated message.
func TestAttempt_FlakyRepro_AlternatingFailPass_NoPromotion(t *testing.T) {
	repoDir := newRepoDir(t)
	// Two revision rounds so we can also assert the round-2 agent request
	// carries the flake-specific corrective feedback.
	client := newScriptedClient(planBody(t, goodPlan()), planBody(t, goodPlan()))

	sb := sandbox.NewMock(sandbox.MockResponse{})
	sb.ResponseFunc = func(n int, _ sandbox.Spec) (sandbox.Result, error) {
		if n%2 == 0 {
			// Official run (round 1's call 0, round 2's call 2): demonstrates.
			return sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestBug\nFAIL"}, nil
		}
		// Determinism confirmation run: passes, i.e. does NOT demonstrate.
		return sandbox.Result{ExitCode: 0, Stdout: "ok\nPASS"}, nil
	}

	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir(), MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}

	finding := domain.Finding{ID: "f-flaky", Title: "racy assertion", File: "calc.go", Line: 12}
	att, err := r.Attempt(context.Background(), finding)
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if att.Promoted {
		t.Fatalf("Attempt = %+v, want NOT promoted (demonstrate-then-not is not a reliable repro)", att)
	}
	if att.Reason != string(VerdictReasonFlaky) {
		t.Errorf("Reason = %q, want %q", att.Reason, VerdictReasonFlaky)
	}
	if got := sb.CallCount(); got != 4 {
		t.Errorf("sandbox calls = %d, want 4 (2 rounds x [official + confirmation])", got)
	}

	// Round 2's agent request must carry the flake-specific, actionable
	// feedback — not the generic not-demonstrated message.
	second := client.taskText(1)
	for _, want := range []string{"synchronization", "iterations", "did NOT demonstrate it again"} {
		if !strings.Contains(second, want) {
			t.Errorf("round-2 feedback missing %q; got:\n%s", want, second)
		}
	}
}

// TestAttempt_FlakyRepro_WritesNoArtifact ensures a flaky (demonstrate-
// then-not) sequence never writes a repro bundle: promotion and artifact
// writing are gated together.
func TestAttempt_FlakyRepro_WritesNoArtifact(t *testing.T) {
	repoDir := newRepoDir(t)
	client := newScriptedClient(planBody(t, goodPlan()))

	sb := sandbox.NewMock(sandbox.MockResponse{})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestBug\nFAIL"}})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok\nPASS"}})

	artifactDir := t.TempDir()
	r, err := New(client, sb, repoDir, Options{ArtifactDir: artifactDir, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}

	finding := domain.Finding{ID: "f-flaky-2", Title: "racy assertion", File: "calc.go", Line: 12}
	att, err := r.Attempt(context.Background(), finding)
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if att.Promoted || att.ArtifactPath != "" {
		t.Fatalf("Attempt = %+v, want not promoted and no artifact path", att)
	}
}

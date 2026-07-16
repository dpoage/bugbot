package repro

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// reproRecordingSink captures progress events for assertions. Safe for the
// concurrent emission a reproduce run may produce.
type reproRecordingSink struct {
	mu     sync.Mutex
	events []progress.Event
}

func (s *reproRecordingSink) Handle(ev progress.Event) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
}

func (s *reproRecordingSink) byKind(k progress.Kind) []progress.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []progress.Event
	for _, e := range s.events {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

// TestAttempt_EmitsAgentBracket verifies a reproduce run surfaces in
// `bugbot status` via the shared AgentScope seam: exactly one agent-started and
// one agent-finished event, tagged role=reproducer with the finding title as
// label, and the finished event carrying the run's token usage.
func TestAttempt_EmitsAgentBracket(t *testing.T) {
	repoDir := newRepoDir(t)
	client := newScriptedClient(planBody(t, goodPlan()))
	// Genuine assertion failure → demonstrated → one planFor call.
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\n    bug_test.go:1: Divide(1,0) = 0, want error\nFAIL",
	}})

	var sink reproRecordingSink
	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir(), Progress: &sink})
	if err != nil {
		t.Fatal(err)
	}

	finding := domain.Finding{ID: "f-1", Title: "Divide ignores zero divisor", File: "calc.go", Line: 12}
	if _, err := r.Attempt(context.Background(), finding); err != nil {
		t.Fatalf("Attempt: %v", err)
	}

	started := sink.byKind(progress.KindAgentStarted)
	finished := sink.byKind(progress.KindAgentFinished)
	if len(started) != 1 || len(finished) != 1 {
		t.Fatalf("started=%d finished=%d, want 1 each", len(started), len(finished))
	}
	if started[0].Role != progress.RoleReproducer || started[0].Label != finding.Title {
		t.Errorf("started = %+v, want role=%q label=%q", started[0], progress.RoleReproducer, finding.Title)
	}
	if finished[0].Role != progress.RoleReproducer || finished[0].Label != finding.Title {
		t.Errorf("finished role/label = %+v", finished[0])
	}
	// scriptedClient bills 15 tokens/call; the bracket must report the run total.
	if finished[0].Tokens == 0 {
		t.Errorf("finished Tokens = 0, want the run's accumulated usage")
	}
}

// TestReproducer_StatusNoteToolGating verifies the status_note tool is offered
// to the reproducer agent only when Options.StatusNotes is set — the opt-in
// half of the shared observability interface.
func TestReproducer_StatusNoteToolGating(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   bool
	}{{"off", false}, {"on", true}} {
		t.Run(tc.name, func(t *testing.T) {
			repoDir := newRepoDir(t)
			client := newScriptedClient(planBody(t, goodPlan()))
			sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
				ExitCode: 1,
				Stdout:   "--- FAIL: TestBug\nFAIL",
			}})

			r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir(), StatusNotes: tc.on})
			if err != nil {
				t.Fatal(err)
			}

			finding := domain.Finding{ID: "f-1", Title: "t", File: "calc.go", Line: 1}
			if _, err := r.Attempt(context.Background(), finding); err != nil {
				t.Fatalf("Attempt: %v", err)
			}

			reqs := client.allRequests()
			if len(reqs) == 0 {
				t.Fatal("no requests recorded")
			}
			has := false
			for _, td := range reqs[0].Tools {
				if td.Name == "status_note" {
					has = true
					break
				}
			}
			if has != tc.on {
				t.Errorf("status_note tool present=%v, want %v", has, tc.on)
			}
		})
	}
}

// TestProve_EmitsAgentBracket verifies the patch-prover run surfaces through
// the same shared seam: one started + one finished event tagged
// role=patch-prover with the finding title, and (with statusNotes on) the
// status_note tool offered to the agent. Models TestPatchProver_SuccessPath.
func TestProve_EmitsAgentBracket(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	repoDir := newRepoDirWithCalc(t)
	artifactDir := t.TempDir()

	finding, att := buildT1Finding(t, st)
	if err := os.MkdirAll(filepath.Join(artifactDir, finding.ID), 0o755); err != nil {
		t.Fatal(err)
	}

	client := newScriptedClient(patchPlanBody(t, goodPatchPlan()))
	// Two sandbox calls both green: targeted pass + suite pass → fix witnessed.
	sb := sandbox.NewMock(sandbox.MockResponse{})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok"}})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok"}})

	var sink reproRecordingSink
	prover := &PatchProver{
		client:      client,
		sb:          sb,
		repoDir:     repoDir,
		maxAttempts: 3,
		artifactDir: artifactDir,
		suiteCmd:    []string{"go", "test", "./..."}, // explicit so Prove never skips
		progress:    &sink,
		statusNotes: true,
	}

	if _, err := prover.Prove(ctx, st, finding, att); err != nil {
		t.Fatalf("Prove: %v", err)
	}

	started := sink.byKind(progress.KindAgentStarted)
	finished := sink.byKind(progress.KindAgentFinished)
	if len(started) != 1 || len(finished) != 1 {
		t.Fatalf("started=%d finished=%d, want 1 each", len(started), len(finished))
	}
	if started[0].Role != progress.RolePatchProver || started[0].Label != finding.Title {
		t.Errorf("started = %+v, want role=%q label=%q", started[0], progress.RolePatchProver, finding.Title)
	}
	if finished[0].Role != progress.RolePatchProver {
		t.Errorf("finished role = %q, want %q", finished[0].Role, progress.RolePatchProver)
	}

	reqs := client.allRequests()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	has := false
	for _, td := range reqs[0].Tools {
		if td.Name == "status_note" {
			has = true
			break
		}
	}
	if !has {
		t.Error("status_note tool absent from patch-prover despite statusNotes=true")
	}
}

// TestAttempt_EmitsReproAttemptPerRound verifies Attempt emits exactly one
// KindReproAttempt per round, with the round's 1-based Attempt number, the
// resolved MaxAttempts, and the round's verdict token: round 1 exits zero
// (not_demonstrated verdict, since interpret classifies a clean exit as
// VerdictReasonExitZero) so it revises; round 2 demonstrates the bug and
// promotes, closing the loop after exactly 2 rounds.
func TestAttempt_EmitsReproAttemptPerRound(t *testing.T) {
	repoDir := newRepoDir(t)
	client := newScriptedClient(planBody(t, goodPlan()), planBody(t, goodPlan()))

	sb := sandbox.NewMock(sandbox.MockResponse{})
	// Round 1: clean exit, does not demonstrate.
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok\nPASS"}})
	// Round 2: genuine assertion failure, demonstrates the bug.
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\n    bug_test.go:1: Divide(1,0) = 0, want error\nFAIL",
	}})
	// bugbot-c49s determinism gate: round 2's demonstration must be confirmed
	// by an identical re-run before it promotes.
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\n    bug_test.go:1: Divide(1,0) = 0, want error\nFAIL",
	}})

	var sink reproRecordingSink
	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir(), Progress: &sink, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}

	finding := domain.Finding{ID: "f-2", Title: "Divide ignores zero divisor", File: "calc.go", Line: 12}
	att, err := r.Attempt(context.Background(), finding)
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if !att.Promoted {
		t.Fatalf("Attempt = %+v, want Promoted (round 2 demonstrates)", att)
	}

	events := sink.byKind(progress.KindReproAttempt)
	if len(events) != 2 {
		t.Fatalf("KindReproAttempt events = %d, want 2 (one per round); got %+v", len(events), events)
	}
	for _, ev := range events {
		if ev.Role != progress.RoleReproducer || ev.Label != finding.Title {
			t.Errorf("event role/label = %+v, want role=%q label=%q", ev, progress.RoleReproducer, finding.Title)
		}
		if ev.MaxAttempts != 2 {
			t.Errorf("event MaxAttempts = %d, want 2", ev.MaxAttempts)
		}
		if ev.Duration < 0 {
			t.Errorf("event Duration = %v, want >= 0", ev.Duration)
		}
	}
	if events[0].Attempt != 1 {
		t.Errorf("first event Attempt = %d, want 1", events[0].Attempt)
	}
	if events[0].Verdict == "" || events[0].Verdict == "demonstrated" {
		t.Errorf("first round Verdict = %q, want a non-demonstrating VerdictReason", events[0].Verdict)
	}
	if events[1].Attempt != 2 {
		t.Errorf("second event Attempt = %d, want 2", events[1].Attempt)
	}
	if events[1].Verdict != "demonstrated" {
		t.Errorf("second round Verdict = %q, want %q", events[1].Verdict, "demonstrated")
	}
}

// TestAttempt_EmitsReproAttempt_UnparseableAndInvalidPlan verifies the two
// early-exit branches (unparseable plan output, structurally invalid plan)
// each emit a KindReproAttempt with the matching sentinel Verdict token
// instead of a sandbox-derived VerdictReason, before continuing to the next
// round.
func TestAttempt_EmitsReproAttempt_UnparseableAndInvalidPlan(t *testing.T) {
	repoDir := newRepoDir(t)
	// Round 1: garbage output → ErrUnparseableOutput (round emits "unparseable_plan").
	// Round 2: a schema-valid but structurally invalid plan (an absolute file
	// path, rejected by validatePlan's workspace-relative check) → round
	// emits "invalid_plan". MaxAttempts=2 means the loop ends there.
	invalidPlan := `{"files":{"/tmp/bug_test.go":"package bug"},"cmd":["go","test"]}`
	client := newScriptedClient(
		"]<]minimax[>[ broken tool call, not json",
		"still not json",
		invalidPlan,
	)
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok"}})

	var sink reproRecordingSink
	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir(), Progress: &sink, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}

	finding := domain.Finding{ID: "f-3", Title: "unparseable then invalid", File: "calc.go", Line: 1}
	att, err := r.Attempt(context.Background(), finding)
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if att.Promoted {
		t.Fatalf("Attempt = %+v, want not promoted", att)
	}

	events := sink.byKind(progress.KindReproAttempt)
	if len(events) != 2 {
		t.Fatalf("KindReproAttempt events = %d, want 2; got %+v", len(events), events)
	}
	if events[0].Attempt != 1 || events[0].Verdict != "unparseable_plan" {
		t.Errorf("round 1 event = %+v, want Attempt=1 Verdict=unparseable_plan", events[0])
	}
	if events[1].Attempt != 2 || events[1].Verdict != "invalid_plan" {
		t.Errorf("round 2 event = %+v, want Attempt=2 Verdict=invalid_plan", events[1])
	}
	// Neither round reached the sandbox: both branches short-circuit before execute().
	if n := len(sb.Calls()); n != 0 {
		t.Errorf("sandbox calls = %d, want 0 (both rounds fail before execute)", n)
	}
}

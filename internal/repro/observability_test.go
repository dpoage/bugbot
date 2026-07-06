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

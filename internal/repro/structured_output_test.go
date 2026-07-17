package repro

import (
	"context"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// End-to-end boundary integration tests for bugbot-jwd on the repro
// side. The mechanism that threads each boundary's JSON schema through
// llm.Request.ResponseSchema (capability-gated) is implemented by
// gd3 + w88; these tests PROVE that the reproducer boundary's
// planSchema and the patch-prover boundary's patchSchema reach the
// wire as ResponseSchema when the client reports
// StructuredOutput=true, and are absent when the cap is off. The
// repair path is verified for both boundaries: a wrong-shape first
// answer triggers a tools-less, schema-bearing repair completion.
//
// What is intentionally NOT in scope: parser-correctness of the JSON
// itself (covered by the unit tests for typed unmarshal). The point
// of this file is the WIRE contract at the two repro boundaries:
// Reproducer.planFor (planSchema) and PatchProver.planFor
// (patchSchema).

// demonstratingSandbox is the canonical "test ran and failed" output
// the reproducer's interpreter accepts. Bare "FAIL" or just a non-
// zero exit is rejected as "not a real test failure", which would
// trigger a revision request and an extra completion — that would
// pollute the wire-contract assertions below. Reused by every
// reproducer test in this file.
func demonstratingSandbox() *sandbox.Mock {
	return sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\n    bug_test.go:1: Divide(1,0) = 0, want error\nFAIL",
	}})
}

// TestStructuredOutput_ReproducerCarriesPlanSchema asserts that the
// reproducer boundary's RunJSON completion carries planSchema as
// ResponseSchema when the client reports StructuredOutput=true, and
// the parsed plan round-trips end-to-end through a real Reproducer
// Attempt.
func TestStructuredOutput_ReproducerCarriesPlanSchema(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)
	artifactDir := t.TempDir()

	client := newScriptedClient(planBody(t, goodPlan()))
	client.caps = llm.Capabilities{StructuredOutput: true}

	r, err := New(client, demonstratingSandbox(), repoDir, Options{ArtifactDir: artifactDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := r.Attempt(ctx, finding); err != nil {
		t.Fatalf("Attempt: %v", err)
	}

	reqs := client.allRequests()
	if len(reqs) == 0 {
		t.Fatalf("reproducer client saw no completions")
	}
	for i, r := range reqs {
		if got := string(r.ResponseSchema); got != string(planSchema) {
			t.Errorf("req %d: ResponseSchema = %s\nwant (planSchema)", i, got)
		}
	}
}

// TestStructuredOutput_ReproducerNoCapPassthrough is the negative
// control: StructuredOutput=false (the zero value) must drop the
// schema on the wire while still producing a correct plan.
func TestStructuredOutput_ReproducerNoCapPassthrough(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)
	artifactDir := t.TempDir()

	// caps is the zero value: StructuredOutput=false.
	client := newScriptedClient(planBody(t, goodPlan()))

	r, err := New(client, demonstratingSandbox(), repoDir, Options{ArtifactDir: artifactDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := r.Attempt(ctx, finding); err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	for i, r := range client.allRequests() {
		if len(r.ResponseSchema) != 0 {
			t.Errorf("req %d: ResponseSchema = %s, want empty (no-cap passthrough)", i, string(r.ResponseSchema))
		}
	}
}

// TestStructuredOutput_ReproducerRepairCarriesSchema drives a
// wrong-shape first answer (a bare array where planSchema requires
// an object) followed by a correct plan on the repair, and asserts
// that the repair completion (a) carries planSchema on the wire, and
// (b) is tools-less. This locks in the w88 constrained-repair
// contract at the reproducer boundary specifically.
func TestStructuredOutput_ReproducerRepairCarriesSchema(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)
	artifactDir := t.TempDir()

	// First body: a bare array (wrong root shape) whose inner object is ALSO
	// schema-invalid (missing required "cmd") so the rescue scan
	// (agent.rescueBody, bugbot-9fac) cannot salvage it and the repair
	// round-trip genuinely fires. Second body: a well-shaped plan.
	client := newScriptedClient(
		`[{"files":{},"expect":"y"}]`,
		planBody(t, goodPlan()),
	)
	client.caps = llm.Capabilities{StructuredOutput: true}

	r, err := New(client, demonstratingSandbox(), repoDir, Options{ArtifactDir: artifactDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := r.Attempt(ctx, finding); err != nil {
		t.Fatalf("Attempt: %v", err)
	}

	reqs := client.allRequests()
	if len(reqs) < 2 {
		t.Fatalf("reproducer client saw %d completions, want ≥ 2 (main + repair)", len(reqs))
	}
	for i, r := range reqs {
		if got := string(r.ResponseSchema); got != string(planSchema) {
			t.Errorf("req %d: ResponseSchema = %s\nwant (planSchema)", i, got)
		}
	}
	repair := reqs[len(reqs)-1]
	if len(repair.Tools) != 0 {
		t.Errorf("repair request carried %d tool(s), want 0 (tools-less constrained completion)", len(repair.Tools))
	}
}

// TestStructuredOutput_PatchProverCarriesPatchSchema asserts that the
// patch-prover boundary's RunJSON completion carries patchSchema as
// ResponseSchema when the client reports StructuredOutput=true, and
// the parsed PatchPlan round-trips end-to-end through a real
// PatchProver.
func TestStructuredOutput_PatchProverCarriesPatchSchema(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding, att := buildT1Finding(t, st)
	repoDir := newRepoDirWithCalc(t)
	artifactDir := t.TempDir()

	client := newScriptedClient(patchPlanBody(t, goodPatchPlan()))
	client.caps = llm.Capabilities{StructuredOutput: true}
	// Two sandbox calls: targeted (exit 0) then suite (exit 0).
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0}})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0}})

	prover := &PatchProver{
		client:      client,
		sb:          sb,
		repoDir:     repoDir,
		artifactDir: artifactDir,
		maxAttempts: 1,
		timeout:     80_000_000_000, // 80s
		suiteCmd:    []string{"go", "test", "./..."},
	}
	if _, err := prover.Prove(ctx, st, finding, att); err != nil {
		t.Fatalf("Prove: %v", err)
	}

	reqs := client.allRequests()
	if len(reqs) == 0 {
		t.Fatalf("patch-prover client saw no completions")
	}
	for i, r := range reqs {
		if got := string(r.ResponseSchema); got != string(patchSchema) {
			t.Errorf("req %d: ResponseSchema = %s\nwant (patchSchema)", i, got)
		}
	}
}

// TestStructuredOutput_PatchProverNoCapPassthrough is the negative
// control: StructuredOutput=false (the zero value) must drop the
// schema on the wire while still producing a correct patch plan.
func TestStructuredOutput_PatchProverNoCapPassthrough(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding, att := buildT1Finding(t, st)
	repoDir := newRepoDirWithCalc(t)
	artifactDir := t.TempDir()

	// caps is the zero value: StructuredOutput=false.
	client := newScriptedClient(patchPlanBody(t, goodPatchPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0}})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0}})

	prover := &PatchProver{
		client:      client,
		sb:          sb,
		repoDir:     repoDir,
		artifactDir: artifactDir,
		maxAttempts: 1,
		timeout:     80_000_000_000,
		suiteCmd:    []string{"go", "test", "./..."},
	}
	if _, err := prover.Prove(ctx, st, finding, att); err != nil {
		t.Fatalf("Prove: %v", err)
	}
	for i, r := range client.allRequests() {
		if len(r.ResponseSchema) != 0 {
			t.Errorf("req %d: ResponseSchema = %s, want empty (no-cap passthrough)", i, string(r.ResponseSchema))
		}
	}
}

// TestStructuredOutput_PatchProverRepairCarriesSchema drives a
// wrong-shape first answer (an array) followed by a correct
// patchPlan on the repair, and asserts that the repair completion
// (a) carries patchSchema on the wire, and (b) is tools-less.
func TestStructuredOutput_PatchProverRepairCarriesSchema(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding, att := buildT1Finding(t, st)
	repoDir := newRepoDirWithCalc(t)
	artifactDir := t.TempDir()

	client := newScriptedClient(
		`[{"files":{},"summary":"x"}]`,
		patchPlanBody(t, goodPatchPlan()),
	)
	client.caps = llm.Capabilities{StructuredOutput: true}
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0}})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0}})

	prover := &PatchProver{
		client:      client,
		sb:          sb,
		repoDir:     repoDir,
		artifactDir: artifactDir,
		maxAttempts: 1,
		timeout:     80_000_000_000,
		suiteCmd:    []string{"go", "test", "./..."},
	}
	if _, err := prover.Prove(ctx, st, finding, att); err != nil {
		t.Fatalf("Prove: %v", err)
	}

	reqs := client.allRequests()
	if len(reqs) < 2 {
		t.Fatalf("patch-prover client saw %d completions, want ≥ 2 (main + repair)", len(reqs))
	}
	for i, r := range reqs {
		if got := string(r.ResponseSchema); got != string(patchSchema) {
			t.Errorf("req %d: ResponseSchema = %s\nwant (patchSchema)", i, got)
		}
	}
	repair := reqs[len(reqs)-1]
	if len(repair.Tools) != 0 {
		t.Errorf("repair request carried %d tool(s), want 0 (tools-less constrained completion)", len(repair.Tools))
	}
	// Sanity: at least one user message contains the patch-task
	// task header.
	if !anyMessageMentions(reqs, "MINIMAL fix") {
		t.Errorf("no user message contained the patch-prover task header ('MINIMAL fix')")
	}
}

// anyMessageMentions reports whether any of the user-role messages
// across reqs contains sub. Tiny helper kept here to avoid touching
// the existing fake_test.go.
func anyMessageMentions(reqs []llm.Request, sub string) bool {
	for _, r := range reqs {
		for _, m := range r.Messages {
			if m.Role == llm.RoleUser && strings.Contains(m.Content, sub) {
				return true
			}
		}
	}
	return false
}

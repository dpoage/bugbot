package repro

import (
	"context"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// TestNewRunner_RunTestsToolWiredWithGoMod verifies that when the repo
// directory contains a go.mod (a recognized build system), newRunner wires the
// run_tests tool so it appears in the first LLM request's tool set.
func TestNewRunner_RunTestsToolWiredWithGoMod(t *testing.T) {
	repoDir := newRepoDir(t) // creates go.mod via newRepoDir helper
	client := newScriptedClient(planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\nFAIL",
	}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	finding := store.Finding{ID: "rt-1", Title: "run_tests wiring", File: "calc.go", Line: 1}
	if _, err := r.Attempt(context.Background(), finding); err != nil {
		t.Fatalf("Attempt: %v", err)
	}

	reqs := client.allRequests()
	if len(reqs) == 0 {
		t.Fatal("no LLM requests recorded")
	}
	var names []string
	for _, td := range reqs[0].Tools {
		names = append(names, td.Name)
	}
	found := false
	for _, n := range names {
		if n == "run_tests" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("run_tests tool not present in LLM request; got tools: %v", names)
	}
}

// TestNewRunner_RunTestsToolAbsentWithoutBuildSystem verifies that when the
// repo directory contains no recognized build-system markers, the run_tests
// tool is NOT wired (detectSuiteCmd returns empty).
func TestNewRunner_RunTestsToolAbsentWithoutBuildSystem(t *testing.T) {
	// Bare temp dir — no go.mod, Cargo.toml, package.json, etc.
	repoDir := t.TempDir()
	client := newScriptedClient(planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\nFAIL",
	}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	finding := store.Finding{ID: "rt-2", Title: "no build system", File: "calc.go", Line: 1}
	if _, err := r.Attempt(context.Background(), finding); err != nil {
		t.Fatalf("Attempt: %v", err)
	}

	reqs := client.allRequests()
	if len(reqs) == 0 {
		t.Fatal("no LLM requests recorded")
	}
	for _, td := range reqs[0].Tools {
		if td.Name == "run_tests" {
			t.Errorf("run_tests tool unexpectedly present when no build system marker exists")
		}
	}
}

// TestNewRunner_RunTestsGuidanceInPrompt verifies that the run_tests guidance
// section (including the per-attempt budget) appears in the system prompt when
// a build system is detected, and is absent when no build system is present.
func TestNewRunner_RunTestsGuidanceInPrompt(t *testing.T) {
	t.Run("present_with_go_mod", func(t *testing.T) {
		repoDir := newRepoDir(t)
		client := newScriptedClient(planBody(t, goodPlan()))
		sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1}})

		// Use a non-default budget to confirm the number is threaded through.
		r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir(), SandboxMaxExecs: 5})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()

		finding := store.Finding{ID: "rt-3", Title: "prompt check", File: "calc.go", Line: 1}
		if _, err := r.Attempt(context.Background(), finding); err != nil {
			t.Fatalf("Attempt: %v", err)
		}

		reqs := client.allRequests()
		if len(reqs) == 0 {
			t.Fatal("no LLM requests recorded")
		}
		prompt := reqs[0].System
		if !strings.Contains(prompt, "run_tests") {
			t.Errorf("system prompt missing run_tests guidance; prompt[:300]=%q", truncateStr(prompt, 300))
		}
		// The budget (5) must be embedded by runTestsGuidance(5).
		if !strings.Contains(prompt, "5 time") {
			t.Errorf("system prompt missing budget in run_tests guidance; prompt[:500]=%q", truncateStr(prompt, 500))
		}
	})

	t.Run("absent_without_build_system", func(t *testing.T) {
		repoDir := t.TempDir()
		client := newScriptedClient(planBody(t, goodPlan()))
		sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1}})

		r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()

		finding := store.Finding{ID: "rt-4", Title: "no build system prompt check", File: "calc.go", Line: 1}
		if _, err := r.Attempt(context.Background(), finding); err != nil {
			t.Fatalf("Attempt: %v", err)
		}

		reqs := client.allRequests()
		if len(reqs) == 0 {
			t.Fatal("no LLM requests recorded")
		}
		prompt := reqs[0].System
		if strings.Contains(prompt, "run_tests") {
			t.Errorf("run_tests guidance unexpectedly present with no build system")
		}
	})
}

// TestSandboxMaxExecs_DefaultApplied verifies that resolve() fills in
// DefaultSandboxMaxExecs when Options.SandboxMaxExecs is zero.
func TestSandboxMaxExecs_DefaultApplied(t *testing.T) {
	o := Options{}
	got := o.resolve()
	if got.SandboxMaxExecs != DefaultSandboxMaxExecs {
		t.Errorf("resolve().SandboxMaxExecs = %d, want %d (DefaultSandboxMaxExecs)",
			got.SandboxMaxExecs, DefaultSandboxMaxExecs)
	}
}

// TestSandboxMaxExecs_ExplicitPreserved verifies that an explicit non-zero
// value in Options is not overwritten by resolve().
func TestSandboxMaxExecs_ExplicitPreserved(t *testing.T) {
	o := Options{SandboxMaxExecs: 7}
	got := o.resolve()
	if got.SandboxMaxExecs != 7 {
		t.Errorf("resolve().SandboxMaxExecs = %d, want 7", got.SandboxMaxExecs)
	}
}

// truncateStr returns up to n bytes of s for use in error messages.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

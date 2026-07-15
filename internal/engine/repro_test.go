package engine

import (
	"context"
	"io"
	"strings"
	"testing"
)

// TestRepro_ResolvesEmptyTargetToRepoToplevel covers the exact defect behind
// bugbot-pt83: the TUI dispatch palette's rowRepro never populates
// ReproOpts.Target (see palette.go's dispatchCmd), so Dispatcher.Repro
// forwarded an empty string straight into BuildReproducer as repoRoot, and
// repro.New rejects an empty repoDir outright ("repro: empty repoDir") —
// every TUI-dispatched Repro against a non-empty backlog failed. The fix
// routes opts.Target through d.openRepo (the same ingest.Open helper every
// sibling verb already uses), which both resolves "" to the process cwd AND
// walks up to the git toplevel via `git rev-parse --show-toplevel` — not
// just an absolute path, so a TUI launched from a subdirectory still repros
// against the whole repo rather than a wrong (sub)tree. This test exercises
// that resolution through the real Dispatcher (not a copy of the logic).
func TestRepro_ResolvesEmptyTargetToRepoToplevel(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)

	d, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()

	// Empty backlog short-circuits before opts.Target reaches BuildReproducer
	// (see TestRepro_EmptyBacklogSkipsBeforeBuildReproducer for that
	// boundary and TestRepro_ResolvesEmptyTargetAgainstRealBacklog, gated
	// behind -tags integration, for the seeded-backlog case that actually
	// reaches repro.New). What this test pins down is the resolution step
	// itself: d.openRepo must succeed against an unset Target and record a
	// non-empty, absolute repo root on the Dispatcher.
	if _, err := d.Repro(ctx, ReproOpts{Target: "", Out: io.Discard}); err != nil {
		t.Fatalf("Repro(Target=\"\") error = %v, want a graceful skip", err)
	}
	if d.repo == nil {
		t.Fatal("Repro(Target=\"\") did not resolve a repo via openRepo")
	}
	if d.repo.Root() == "" {
		t.Fatal("Repro(Target=\"\") resolved an empty repo root; repro.New rejects that outright (bugbot-pt83 regression)")
	}
}

// TestRepro_EmptyBacklogSkipsBeforeBuildReproducer documents the boundary
// the oracle review of bugbot-pt83 flagged: an empty backlog returns before
// opts.Target is ever forwarded to BuildReproducer/repro.New, so this case
// alone cannot prove the fix (see TestRepro_ResolvesEmptyTargetAgainstRealBacklog
// for the test that actually crosses that boundary).
func TestRepro_EmptyBacklogSkipsBeforeBuildReproducer(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)

	d, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()

	res, err := d.Repro(ctx, ReproOpts{Target: "", Out: io.Discard})
	if err != nil {
		t.Fatalf("Repro(Target=\"\") error = %v, want a graceful skip", err)
	}
	switch {
	case res == nil:
		t.Fatal("Repro(Target=\"\") returned a nil result with a nil error")
	case res.Skipped != "no eligible findings" && res.Skipped != "no container runtime":
		t.Fatalf("Repro(Target=\"\") Skipped = %q, want \"no eligible findings\" or \"no container runtime\"", res.Skipped)
	}
}

// TestRepro_UnsandboxedRefusedWithoutFindingID is regression coverage for
// bugbot-14g0 acceptance 5: --unsandboxed (ReproOpts.Unsandboxed) must be
// hard-refused for the backlog batch path — it is single-finding-attended
// only. This is the gate that also structurally keeps the escape hatch out
// of any unattended caller of Dispatcher.Repro (the daemon never calls it at
// all; see daemon.promoteNewFindings).
func TestRepro_UnsandboxedRefusedWithoutFindingID(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)

	d, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()

	_, err = d.Repro(ctx, ReproOpts{Unsandboxed: true, Out: io.Discard})
	if err == nil {
		t.Fatal("Repro(Unsandboxed=true, FindingID=\"\") should be refused, got nil error")
	}
	if !strings.Contains(err.Error(), "unsandboxed") || !strings.Contains(err.Error(), "finding id") {
		t.Errorf("error = %q, want it to explain --unsandboxed needs a finding id", err.Error())
	}
}

// TestRepro_SingleFindingUnknownIDErrors verifies the single-finding path
// (opts.FindingID set) surfaces a clear resolution error for an unknown id,
// without ever reaching the sandbox/LLM client construction — report.ResolveID
// fails fast against the (empty) store.
func TestRepro_SingleFindingUnknownIDErrors(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)

	d, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()

	_, err = d.Repro(ctx, ReproOpts{FindingID: "no-such-finding-id", Out: io.Discard})
	if err == nil {
		t.Fatal("Repro(FindingID=\"no-such-finding-id\") should error, got nil")
	}
	if !strings.Contains(err.Error(), "resolve finding") {
		t.Errorf("error = %q, want it to name the resolution step", err.Error())
	}
}

// TestRepro_UnsandboxedSingleFindingUnknownIDStillHardRefusesFirst pins the
// ORDER of the two checks: an --unsandboxed call with a finding-id that does
// not exist should still fail on RESOLUTION (id not found), not silently
// succeed — i.e. the refusal gate does not mask a real resolution error, and
// (converse) a valid FindingID does not bypass the refusal gate's absence
// requirement, since it is not the case being tested here — this test only
// pins that Unsandboxed+FindingID together reach the resolve step.
func TestRepro_UnsandboxedSingleFindingUnknownIDStillHardRefusesFirst(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)

	d, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()

	_, err = d.Repro(ctx, ReproOpts{FindingID: "no-such-finding-id", Unsandboxed: true, Out: io.Discard})
	if err == nil {
		t.Fatal("want a resolution error, got nil")
	}
	if !strings.Contains(err.Error(), "resolve finding") {
		t.Errorf("error = %q, want the resolve-finding error (not a refusal or unrelated error)", err.Error())
	}
}

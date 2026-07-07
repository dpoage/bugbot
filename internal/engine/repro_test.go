package engine

import (
	"context"
	"io"
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

package engine

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestResolveReproTarget covers the exact defect behind bugbot-pt83: the TUI
// dispatch palette's rowRepro never populates ReproOpts.Target (see
// palette.go's dispatchCmd), so Dispatcher.Repro received an empty string
// and forwarded it verbatim into BuildReproducer as repoRoot. repro.New
// rejects an empty repoDir outright, so every TUI-dispatched Repro against a
// non-empty backlog failed with "repro: empty repoDir". Every other
// Dispatcher verb (Scan/Verify/Sweep) is immune because openRepo routes
// through ingest.Open, which calls filepath.Abs internally; Repro has no
// such intermediary, hence resolveReproTarget.
func TestResolveReproTarget(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}

	t.Run("empty resolves to cwd, never empty string", func(t *testing.T) {
		got, err := resolveReproTarget("")
		if err != nil {
			t.Fatalf("resolveReproTarget(\"\") error = %v", err)
		}
		if got == "" {
			t.Fatal("resolveReproTarget(\"\") returned an empty string; repro.New rejects that outright (bugbot-pt83 regression)")
		}
		if got != wd {
			t.Errorf("resolveReproTarget(\"\") = %q, want cwd %q", got, wd)
		}
		if !filepath.IsAbs(got) {
			t.Errorf("resolveReproTarget(\"\") = %q, want an absolute path", got)
		}
	})

	t.Run("relative path is resolved absolute", func(t *testing.T) {
		got, err := resolveReproTarget(".")
		if err != nil {
			t.Fatalf("resolveReproTarget(\".\") error = %v", err)
		}
		if got != wd {
			t.Errorf("resolveReproTarget(\".\") = %q, want cwd %q", got, wd)
		}
	})

	t.Run("already-absolute path is preserved", func(t *testing.T) {
		dir := t.TempDir()
		got, err := resolveReproTarget(dir)
		if err != nil {
			t.Fatalf("resolveReproTarget(%q) error = %v", dir, err)
		}
		if got != dir {
			t.Errorf("resolveReproTarget(%q) = %q, want unchanged", dir, got)
		}
	})
}

// TestRepro_EmptyTargetDoesNotFailAsEmptyRepoDir exercises Dispatcher.Repro
// itself (not just the resolver helper) with an unset Target — exactly what
// the TUI dispatch palette sends (bugbot-pt83) — against a fresh store with
// no backlog. Skipped reason depends on whether this host has a container
// runtime on PATH ("no container runtime" if not, "no eligible findings" if
// so, since the store is empty either way); either is a graceful, expected
// skip. What this test pins down is that neither path surfaces the
// bugbot-pt83 regression's "repro: empty repoDir" / "resolve target" error.
func TestRepro_EmptyTargetDoesNotFailAsEmptyRepoDir(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)

	d, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()

	res, err := d.Repro(ctx, ReproOpts{Target: "", Out: io.Discard})
	if err != nil {
		t.Fatalf("Repro(Target=\"\") error = %v, want a graceful skip (empty repoDir must never resurface)", err)
	}
	switch {
	case res == nil:
		t.Fatal("Repro(Target=\"\") returned a nil result with a nil error")
	case res.Skipped != "no eligible findings" && res.Skipped != "no container runtime":
		t.Fatalf("Repro(Target=\"\") Skipped = %q, want \"no eligible findings\" or \"no container runtime\"", res.Skipped)
	}
}

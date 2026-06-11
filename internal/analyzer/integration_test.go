//go:build integration

package analyzer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// ruffImage is the public ruff container image used for integration tests.
// It is small (~10 MB compressed) and purpose-built for ruff. The image
// uses ruff as its ENTRYPOINT, so the Cmd passed to the sandbox should be the
// ruff sub-command arguments only (not including "ruff" as the first element).
const ruffImage = "ghcr.io/astral-sh/ruff:latest"

// TestSeed_ruff_integration exercises the full end-to-end pipeline: container
// execution → SARIF stdout → parseSARIF → postLeads → store.PendingLeads.
//
// Rather than going through Seed (which uses the package-level registry keyed
// on the general `ruff check ...` command for images with ruff on PATH), this
// test constructs a dedicated analyzerSpec for the ghcr.io/astral-sh/ruff
// image whose ENTRYPOINT is ruff and invokes runAnalyzer directly. This lets
// us verify the real container path without conflating the registry command
// convention with this image's entrypoint convention.
//
// Requires podman or docker on PATH. Skips gracefully when none is found.
func TestSeed_ruff_integration(t *testing.T) {
	runtime, ok := sandbox.Detect()
	if !ok {
		t.Skip("no container runtime (podman/docker) found on PATH; skipping ruff integration test")
	}

	// Tiny Python fixture with a known security rule hit.
	//   S301: use of pickle — detected by ruff's bandit-derived S rules.
	fixture := `import pickle

def load_data(path):
    with open(path, "rb") as f:
        return pickle.load(f)  # S301: use of pickle
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte(fixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// pyproject.toml is the Python project marker for ruff detection.
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[build-system]\n"), 0o644); err != nil {
		t.Fatalf("write pyproject.toml: %v", err)
	}

	sb, err := sandbox.NewCLI(runtime, ruffImage)
	if err != nil {
		t.Fatalf("build sandbox: %v", err)
	}

	// ruffEntrypointSpec uses the ghcr.io/astral-sh/ruff image where ruff is
	// the ENTRYPOINT. The Cmd here is the ruff sub-command args only (the
	// entrypoint provides the "ruff" binary). --select S targets the bandit
	// security rules so the fixture produces a known S301 hit without noise.
	ruffEntrypointSpec := analyzerSpec{
		name:     "ruff",
		detect:   hasPythonProject,
		cmd:      []string{"check", "--output-format=sarif", "--select", "S", "."},
		ruleLens: ruffRuleLens,
		timeout:  defaultAnalyzerTimeout,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	arun := runAnalyzer(ctx, ruffEntrypointSpec, sb, dir, ruffImage)

	t.Logf("ruff integration run: ran=%v hits=%d skipped=%q duration=%s",
		arun.Ran, arun.Hits, arun.SkippedReason, arun.Duration)

	if !arun.Ran {
		t.Fatalf("ruff did not run (skipped: %s)", arun.SkippedReason)
	}
	if arun.SkippedReason != "" {
		t.Fatalf("ruff was skipped unexpectedly: %s", arun.SkippedReason)
	}
	if arun.Hits == 0 {
		t.Fatal("expected at least one ruff hit from S301 (pickle), got 0")
	}

	// Post the leads to an in-memory store and assert the injection lens was populated.
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	posted, err := postLeads(ctx, arun.results, "ruff", st)
	if err != nil {
		t.Fatalf("postLeads: %v", err)
	}
	t.Logf("posted %d lead(s)", posted)
	if posted == 0 {
		t.Fatal("expected at least one lead to be posted")
	}

	leads, err := st.PendingLeads(ctx, lensInjection)
	if err != nil {
		t.Fatalf("PendingLeads(injection): %v", err)
	}
	if len(leads) == 0 {
		t.Error("expected at least one injection lead from S301 (pickle use), got 0")
	}
	for _, l := range leads {
		t.Logf("  lead: poster=%s target=%s file=%s line=%d note=%q",
			l.PosterLens, l.TargetLens, l.File, l.Line, l.Note)
		if l.PosterLens != "analyzer:ruff" {
			t.Errorf("PosterLens = %q, want 'analyzer:ruff'", l.PosterLens)
		}
		if l.TargetLens != lensInjection {
			t.Errorf("TargetLens = %q, want %q", l.TargetLens, lensInjection)
		}
	}
}

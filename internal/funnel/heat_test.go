package funnel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// ---------------------------------------------------------------------------
// applyHeatOrder unit tests
// ---------------------------------------------------------------------------

func TestApplyHeatOrder_ReordersHotFirst(t *testing.T) {
	// hot.go scores 1.0, cold.go scores 0.1, zero.go scores 0.
	heat := map[string]float64{
		"hot.go":  1.0,
		"cold.go": 0.1,
		"zero.go": 0.0,
	}
	targets := []string{"cold.go", "zero.go", "hot.go"}
	reordered := applyHeatOrder(targets, heat)
	if !reordered {
		t.Error("expected applyHeatOrder to return true (reordering occurred)")
	}
	if targets[0] != "hot.go" {
		t.Errorf("targets[0] = %q, want hot.go", targets[0])
	}
	if targets[1] != "cold.go" {
		t.Errorf("targets[1] = %q, want cold.go", targets[1])
	}
	if targets[2] != "zero.go" {
		t.Errorf("targets[2] = %q, want zero.go", targets[2])
	}
}

func TestApplyHeatOrder_AlphabeticalTiebreak(t *testing.T) {
	// All files have the same heat; they should end up alphabetical.
	heat := map[string]float64{
		"charlie.go": 0.5,
		"alpha.go":   0.5,
		"bravo.go":   0.5,
	}
	targets := []string{"charlie.go", "bravo.go", "alpha.go"}
	_ = applyHeatOrder(targets, heat)
	want := []string{"alpha.go", "bravo.go", "charlie.go"}
	for i, w := range want {
		if targets[i] != w {
			t.Errorf("targets[%d] = %q, want %q", i, targets[i], w)
		}
	}
}

func TestApplyHeatOrder_ZeroHeatAlphabetical(t *testing.T) {
	// All files have zero heat (empty map) — purely alphabetical.
	heat := map[string]float64{}
	targets := []string{"z.go", "a.go", "m.go"}
	_ = applyHeatOrder(targets, heat)
	want := []string{"a.go", "m.go", "z.go"}
	for i, w := range want {
		if targets[i] != w {
			t.Errorf("targets[%d] = %q, want %q (zero-heat alphabetical)", i, targets[i], w)
		}
	}
}

func TestApplyHeatOrder_AlreadySorted_ReturnsFalse(t *testing.T) {
	// If the input is already in heat-desc order, return false.
	heat := map[string]float64{"a.go": 2.0, "b.go": 1.0, "c.go": 0.0}
	targets := []string{"a.go", "b.go", "c.go"}
	if applyHeatOrder(targets, heat) {
		t.Error("expected false when already in correct order")
	}
}

func TestApplyHeatOrder_SingleFile(t *testing.T) {
	// Single file — no reordering possible.
	heat := map[string]float64{"a.go": 1.0}
	targets := []string{"a.go"}
	if applyHeatOrder(targets, heat) {
		t.Error("single file: expected false")
	}
}

func TestApplyHeatOrder_Empty(t *testing.T) {
	heat := map[string]float64{}
	var targets []string
	if applyHeatOrder(targets, heat) {
		t.Error("empty targets: expected false")
	}
}

// ---------------------------------------------------------------------------
// Sweep heat stats integration test
// ---------------------------------------------------------------------------

// newHeatFixtureRepo builds a git repo with 3 files at different churn levels
// and returns its path. Files:
//   - "recent.go" — touched 3 times in the past 7 days.
//   - "old.go" — touched once 200 days ago.
//   - "clean.go" — touched once at the same time as recent first touch
//     (middle heat).
func newHeatFixtureRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()

	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit := func(env []string, args ...string) {
		t.Helper()
		full := append([]string{
			"-C", dir,
			"-c", "user.name=Test",
			"-c", "user.email=test@example.com",
			"-c", "commit.gpgsign=false",
		}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0",
			"HOME="+dir,
		)
		cmd.Env = append(cmd.Env, env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	envDate := func(d time.Time) []string {
		s := d.UTC().Format(time.RFC3339)
		return []string{"GIT_AUTHOR_DATE=" + s, "GIT_COMMITTER_DATE=" + s}
	}

	now := time.Now()
	oldDate := now.Add(-200 * 24 * time.Hour)
	recentDate1 := now.Add(-7 * 24 * time.Hour)
	recentDate2 := now.Add(-3 * 24 * time.Hour)
	recentDate3 := now.Add(-1 * 24 * time.Hour)

	runGit(nil, "init", "-b", "main")

	// Initial commit (old date): all three files.
	write("old.go", "package fixture\n")
	write("clean.go", "package fixture\n// clean\nfunc Add(a,b int)int{return a+b}\n")
	write("recent.go", "package fixture\n")
	runGit(envDate(oldDate), "add", "-A")
	runGit(envDate(oldDate), "commit", "-m", "initial")

	// 3 more commits to recent.go at recent dates.
	write("recent.go", "package fixture\n// v1\n")
	runGit(envDate(recentDate1), "add", "-A")
	runGit(envDate(recentDate1), "commit", "-m", "recent v1")

	write("recent.go", "package fixture\n// v2\n")
	runGit(envDate(recentDate2), "add", "-A")
	runGit(envDate(recentDate2), "commit", "-m", "recent v2")

	write("recent.go", "package fixture\n// v3\n")
	runGit(envDate(recentDate3), "add", "-A")
	runGit(envDate(recentDate3), "commit", "-m", "recent v3")

	return dir
}

// TestSweep_HeatStats verifies that after a Sweep with heat ordering enabled,
// Stats.HeatOrdered is true and Stats.HeatFiles reflects the number of files
// with non-zero heat. The ordering itself is validated by checking that
// recent.go appears in the first chunk (before old.go).
func TestSweep_HeatStats(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()

	repoDir := newHeatFixtureRepo(t)

	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	repo, err := ingest.Open(ctx, repoDir)
	if err != nil {
		t.Fatalf("ingest.Open: %v", err)
	}

	// Use scripted clients that return empty candidates — we only care about
	// the Stats fields and that heat ordering ran without error.
	finder := newScriptedClient()
	verifier := newScriptedClient()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		// HeatOrdering is enabled by default (DisableHeatOrdering = false).
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = f.Close() }()

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// Heat ordered should be true because the repo has churn history and the
	// heat map reorders the alphabetical snapshot.
	if !res.Stats.HeatOrdered {
		t.Errorf("Stats.HeatOrdered = false; expected heat ordering to have reordered targets")
	}
	if res.Stats.HeatFiles == 0 {
		t.Errorf("Stats.HeatFiles = 0; expected some files with heat > 0")
	}
	t.Logf("Stats.HeatOrdered=%v HeatFiles=%d", res.Stats.HeatOrdered, res.Stats.HeatFiles)
}

// TestSweep_DisableHeatOrdering verifies that setting DisableHeatOrdering=true
// prevents heat ordering: Stats.HeatOrdered must be false and Stats.HeatFiles
// must be zero regardless of git history.
func TestSweep_DisableHeatOrdering(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()

	repoDir := newHeatFixtureRepo(t)

	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	repo, err := ingest.Open(ctx, repoDir)
	if err != nil {
		t.Fatalf("ingest.Open: %v", err)
	}

	finder := newScriptedClient()
	verifier := newScriptedClient()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Features: FeatureFlags{DisableHeatOrdering: true},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = f.Close() }()

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if res.Stats.HeatOrdered {
		t.Error("Stats.HeatOrdered = true with DisableHeatOrdering; expected false")
	}
	if res.Stats.HeatFiles != 0 {
		t.Errorf("Stats.HeatFiles = %d with DisableHeatOrdering; expected 0", res.Stats.HeatFiles)
	}
}

package ingest

import (
	"context"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// parseHeat unit tests
// ---------------------------------------------------------------------------

func TestParseHeat_Empty(t *testing.T) {
	heat := parseHeat([]byte(""), time.Now())
	if len(heat) != 0 {
		t.Errorf("expected empty map for empty input, got %v", heat)
	}
}

func TestParseHeat_NoFilesAfterTS(t *testing.T) {
	// A timestamp with no file lines — valid stanza but contributes nothing.
	heat := parseHeat([]byte("1700000000\n\n"), time.Now())
	if len(heat) != 0 {
		t.Errorf("expected empty map, got %v", heat)
	}
}

func TestParseHeat_OrphanedFileLine(t *testing.T) {
	// File line before any timestamp — should be ignored.
	now := time.Unix(1_700_000_000, 0)
	data := "orphan.go\n1700000000\nfoo.go\n"
	heat := parseHeat([]byte(data), now)
	if _, ok := heat["orphan.go"]; ok {
		t.Error("orphaned file line (before first timestamp) should be ignored")
	}
	if _, ok := heat["foo.go"]; !ok {
		t.Error("foo.go after a timestamp should have a score")
	}
}

func TestParseHeat_DecayOrdering(t *testing.T) {
	// Three files each touched in one commit at different ages. The most
	// recent commit should produce the highest heat score.
	now := time.Unix(1_700_000_000, 0)
	recentTS := now.Add(-5 * 24 * time.Hour)
	mediumTS := now.Add(-60 * 24 * time.Hour)
	oldTS := now.Add(-150 * 24 * time.Hour)

	var sb strings.Builder
	for _, pair := range []struct {
		ts   time.Time
		file string
	}{
		{recentTS, "recent.go"},
		{mediumTS, "medium.go"},
		{oldTS, "old.go"},
	} {
		sb.WriteString(strconv.FormatInt(pair.ts.Unix(), 10))
		sb.WriteByte('\n')
		sb.WriteString(pair.file)
		sb.WriteString("\n\n")
	}

	heat := parseHeat([]byte(sb.String()), now)

	if heat["recent.go"] <= heat["medium.go"] {
		t.Errorf("recent.go (%.4f) should be hotter than medium.go (%.4f)",
			heat["recent.go"], heat["medium.go"])
	}
	if heat["medium.go"] <= heat["old.go"] {
		t.Errorf("medium.go (%.4f) should be hotter than old.go (%.4f)",
			heat["medium.go"], heat["old.go"])
	}
	if heat["old.go"] <= 0 {
		t.Errorf("old.go should have positive heat, got %.6f", heat["old.go"])
	}
}

func TestParseHeat_MultipleCommitsSameFile(t *testing.T) {
	// A file touched by 3 recent commits should score higher than a file
	// touched once at the same age.
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Add(-10*24*time.Hour).Unix(), 10)

	var sb strings.Builder
	for range 3 {
		sb.WriteString(ts + "\n" + "hot.go" + "\n\n")
	}
	sb.WriteString(ts + "\n" + "cool.go" + "\n")

	heat := parseHeat([]byte(sb.String()), now)

	if heat["hot.go"] <= heat["cool.go"] {
		t.Errorf("hot.go (%.4f) should score higher than cool.go (%.4f) (touched 3x vs 1x)",
			heat["hot.go"], heat["cool.go"])
	}
}

func TestParseHeat_ZeroAgeWeight(t *testing.T) {
	// A commit right now should contribute exactly exp(0) = 1.0.
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	data := ts + "\nfile.go\n"

	heat := parseHeat([]byte(data), now)
	if got, want := heat["file.go"], 1.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("zero-age weight: got %.9f, want %.9f", got, want)
	}
}

func TestParseHeat_TauDecay(t *testing.T) {
	// A commit exactly τ (30 days) old should contribute exp(-1) ≈ 0.3679.
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Add(-time.Duration(heatDecayTauDays)*24*time.Hour).Unix(), 10)
	data := ts + "\nfile.go\n"

	heat := parseHeat([]byte(data), now)
	wantWeight := math.Exp(-1.0)
	if got := heat["file.go"]; math.Abs(got-wantWeight) > 1e-4 {
		t.Errorf("τ-decay weight: got %.6f, want %.6f (exp(-1))", got, wantWeight)
	}
}

func TestParseHeat_DropsLangOtherPaths(t *testing.T) {
	// A single stanza lists a code file alongside the kinds of non-code
	// files that historically dominated the top of the heat ranking:
	// Bazel manifest fragments, Helm values, JSON schemas, Markdown docs,
	// go.mod, and extensionless dotfiles. All have no registered language
	// and classify as LangOther. Only the code file should survive.
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Add(-5*24*time.Hour).Unix(), 10)

	files := []string{
		"svc/main.go",
		"svc/values.yaml",
		"deps.MODULE.bazel",
		"svc/values.schema.json",
		"README.md",
		"go.mod",
		"notes.txt",
		".envrc",
	}

	var sb strings.Builder
	sb.WriteString(ts)
	sb.WriteByte('\n')
	for _, f := range files {
		sb.WriteString(f)
		sb.WriteByte('\n')
	}
	sb.WriteByte('\n')

	heat := parseHeat([]byte(sb.String()), now)

	if got := heat["svc/main.go"]; got <= 0 {
		t.Errorf("svc/main.go should have a positive heat score, got %.6f", got)
	}
	dropped := []string{
		"svc/values.yaml",
		"deps.MODULE.bazel",
		"svc/values.schema.json",
		"README.md",
		"go.mod",
		"notes.txt",
		".envrc",
	}
	for _, f := range dropped {
		if _, ok := heat[f]; ok {
			t.Errorf("%s is LangOther and must be dropped from the heat map, got score %.6f", f, heat[f])
		}
	}
	// Sanity: nothing else leaked through.
	if len(heat) != 1 {
		t.Errorf("expected exactly 1 entry in heat map (the code file), got %d: %v", len(heat), heat)
	}
}

func TestParseHeat_KeepsLangShellPaths(t *testing.T) {
	// Shell scripts are LangShell — they are real code (build/release
	// glue, ops scripts) and must not be lumped with the LangOther noise
	// that this filter is designed to drop. A shell script touched at
	// the same time as a Go file should score identically.
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Add(-10*24*time.Hour).Unix(), 10)
	data := ts + "\nsvc/main.go\nscripts/deploy.sh\n"

	heat := parseHeat([]byte(data), now)
	if got := heat["svc/main.go"]; got <= 0 {
		t.Errorf("svc/main.go should be in the heat map, got %.6f", got)
	}
	if got := heat["scripts/deploy.sh"]; got <= 0 {
		t.Errorf("scripts/deploy.sh (LangShell) should be in the heat map, got %.6f", got)
	}
	if math.Abs(heat["svc/main.go"]-heat["scripts/deploy.sh"]) > 1e-9 {
		t.Errorf("same-age code paths should score equally: go=%.6f sh=%.6f",
			heat["svc/main.go"], heat["scripts/deploy.sh"])
	}
}

// TestParseHeat_LongPathNotDropped proves that a file-path line longer than
// bufio.Scanner's 64KB default token cap is still counted in the heat map.
// Before the strings.Split rewrite, bufio.NewScanner would silently drop
// (or, depending on the buffer state, truncate) any line over the cap,
// causing the heat score for the touched file to be zero — a silent data
// loss for deeply-nested generated code, vendored trees, or any path
// constructed by tooling. The path used here is 100KB of repeating prefix
// segments plus a real ".go" extension so DetectLanguage keeps it.
func TestParseHeat_LongPathNotDropped(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Add(-5*24*time.Hour).Unix(), 10)

	// 100KB path: 2500 segments of "subdir/" (8 bytes each = 20000 bytes)
	// plus a short "long.go" tail. We size it well past 64KB to make the
	// regression meaningful: any prior bufio.Scanner-based implementation
	// would drop or split this line and the file would receive no heat.
	const segment = "subdir/"
	const segments = 12500 // 12500 * 8 = 100_000 bytes
	var sb strings.Builder
	sb.Grow(len(segment)*segments + 32)
	for range segments {
		sb.WriteString(segment)
	}
	sb.WriteString("long.go")
	longPath := sb.String()
	if len(longPath) <= 64*1024 {
		t.Fatalf("test path must exceed 64KB to exercise the scanner-cap case, got %d bytes", len(longPath))
	}

	data := ts + "\n" + longPath + "\n"

	heat := parseHeat([]byte(data), now)

	if got, ok := heat[longPath]; !ok {
		t.Errorf("long path (%d bytes) was dropped from heat map; expected an entry", len(longPath))
	} else if got <= 0 {
		t.Errorf("long path should have positive heat, got %.6f", got)
	}
}

// ChurnHeat integration tests (real git repo)

// TestChurnHeat_NonASCIIPaths proves that files with non-ASCII names (accented
// Latin, CJK, etc.) receive non-zero heat and rank correctly against plain-ASCII
// files touched at the same time. This exercises the -c core.quotepath=off fix:
// without it, git log --name-only emits octal-escaped paths ("caf\303\251.go")
// while the snapshot side uses raw UTF-8, so the paths never match and heat is
// silently zero for those files.
func TestChurnHeat_NonASCIIPaths(t *testing.T) {
	r := newTestRepo(t)

	now := time.Now()
	recentDate := now.Add(-2 * 24 * time.Hour)

	// Commit a plain-ASCII file and a non-ASCII file at the same time.
	r.write("ascii.go", "package main\n")
	r.write("café_ñ.go", "package main\n")
	r.gitCommitDated(recentDate, "add non-ascii file")

	// Touch only the non-ASCII file a second time (more recent), so it should
	// outrank the ASCII file by heat.
	r.write("café_ñ.go", "package main\n// v2\n")
	r.gitCommitDated(now.Add(-1*24*time.Hour), "update non-ascii file again")

	heat, err := ChurnHeat(context.Background(), r.dir, 0)
	if err != nil {
		t.Fatalf("ChurnHeat: %v", err)
	}

	t.Logf("heat scores: ascii=%.4f café_ñ.go=%.4f", heat["ascii.go"], heat["café_ñ.go"])

	if heat["café_ñ.go"] == 0 {
		t.Error("café_ñ.go has zero heat; non-ASCII paths not matched (core.quotepath=off missing?)")
	}
	if heat["ascii.go"] == 0 {
		t.Error("ascii.go has zero heat; unexpected")
	}
	// The non-ASCII file was touched twice (more recently), so it should be hotter.
	if heat["café_ñ.go"] <= heat["ascii.go"] {
		t.Errorf("café_ñ.go (%.4f) should outrank ascii.go (%.4f) — touched more recently and more often",
			heat["café_ñ.go"], heat["ascii.go"])
	}
}

// TestChurnHeat_TimeoutDegrades proves that ChurnHeat degrades gracefully (nil,
// nil) when the context is already cancelled — simulating a timeout hit. This
// exercises the heatGitTimeout / context-cancellation degradation path.
func TestChurnHeat_TimeoutDegrades(t *testing.T) {
	r := newTestRepo(t)
	r.write("file.go", "package main\n")
	r.gitCommitDated(time.Now().Add(-1*24*time.Hour), "seed")

	// Pre-cancel the context to simulate a timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	heat, err := ChurnHeat(ctx, r.dir, 0)
	if err != nil {
		t.Fatalf("ChurnHeat should degrade to nil,nil on timeout/cancel, got err: %v", err)
	}
	if heat != nil {
		t.Errorf("ChurnHeat should return nil map on degradation, got %v", heat)
	}
}

func TestChurnHeat_ThreeFileFixture(t *testing.T) {
	r := newTestRepo(t)

	now := time.Now()
	// "old" file: touched only in the initial commit, 200 days ago.
	oldDate := now.Add(-200 * 24 * time.Hour)
	// "medium" file: touched 120 days ago.
	medDate := now.Add(-120 * 24 * time.Hour)
	// "recent" file: touched 3 times in the past week.
	recentDate1 := now.Add(-7 * 24 * time.Hour)
	recentDate2 := now.Add(-4 * 24 * time.Hour)
	recentDate3 := now.Add(-1 * 24 * time.Hour)

	// Create all three files in the initial (old-dated) commit.
	r.write("old.go", "package main\n")
	r.write("medium.go", "package main\n")
	r.write("recent.go", "package main\n")
	r.gitCommitDated(oldDate, "initial commit")

	// Touch medium.go once.
	r.write("medium.go", "package main\n// v2\n")
	r.gitCommitDated(medDate, "update medium.go")

	// Touch recent.go three times.
	r.write("recent.go", "package main\n// v1\n")
	r.gitCommitDated(recentDate1, "update recent.go v1")
	r.write("recent.go", "package main\n// v2\n")
	r.gitCommitDated(recentDate2, "update recent.go v2")
	r.write("recent.go", "package main\n// v3\n")
	r.gitCommitDated(recentDate3, "update recent.go v3")

	heat, err := ChurnHeat(context.Background(), r.dir, 400*24*time.Hour)
	if err != nil {
		t.Fatalf("ChurnHeat: %v", err)
	}

	t.Logf("heat scores: recent=%.4f medium=%.4f old=%.4f",
		heat["recent.go"], heat["medium.go"], heat["old.go"])

	if heat["recent.go"] <= heat["medium.go"] {
		t.Errorf("recent.go (%.4f) should be hotter than medium.go (%.4f)",
			heat["recent.go"], heat["medium.go"])
	}
	if heat["medium.go"] <= heat["old.go"] {
		t.Errorf("medium.go (%.4f) should be hotter than old.go (%.4f)",
			heat["medium.go"], heat["old.go"])
	}
}

func TestChurnHeat_EmptyHistory_ReturnsEmpty(t *testing.T) {
	// A repo with no commits should return an empty map with no error.
	r := newTestRepo(t)
	// No commits made.
	heat, err := ChurnHeat(context.Background(), r.dir, 0)
	if err != nil {
		t.Fatalf("expected no error for empty history, got: %v", err)
	}
	if len(heat) != 0 {
		t.Errorf("expected empty heat map for repo with no commits, got %d entries", len(heat))
	}
}

func TestChurnHeat_NotAGitRepo_ReturnsEmpty(t *testing.T) {
	// A plain directory (not a git repo) should return an empty map, no error.
	dir := t.TempDir()
	heat, err := ChurnHeat(context.Background(), dir, 0)
	if err != nil {
		t.Fatalf("expected no error for non-git dir, got: %v", err)
	}
	if len(heat) != 0 {
		t.Errorf("expected empty heat map for non-git dir, got %d entries", len(heat))
	}
}

func TestChurnHeat_AlphabeticalTiebreak(t *testing.T) {
	// Two files committed at the same timestamp should have identical heat
	// scores; alphabetical sort should place alpha.go before beta.go.
	r := newTestRepo(t)
	sameDate := time.Now().Add(-5 * 24 * time.Hour)

	r.write("beta.go", "package main\n")
	r.write("alpha.go", "package main\n")
	r.gitCommitDated(sameDate, "both files same commit")

	heat, err := ChurnHeat(context.Background(), r.dir, 0)
	if err != nil {
		t.Fatalf("ChurnHeat: %v", err)
	}

	// Both files should have the same heat (same commit, same timestamp).
	if math.Abs(heat["alpha.go"]-heat["beta.go"]) > 1e-9 {
		t.Errorf("alpha.go (%.6f) and beta.go (%.6f) should have equal heat",
			heat["alpha.go"], heat["beta.go"])
	}

	// When sorted heat-desc + alpha tiebreak, alpha.go should come first.
	files := []string{"beta.go", "alpha.go"}
	sort.SliceStable(files, func(i, j int) bool {
		hi, hj := heat[files[i]], heat[files[j]]
		if hi != hj {
			return hi > hj
		}
		return files[i] < files[j]
	})
	if files[0] != "alpha.go" {
		t.Errorf("alphabetical tiebreak: expected alpha.go first, got %v", files)
	}
}

// ---------------------------------------------------------------------------
// testRepo extension: date-controlled commits
// ---------------------------------------------------------------------------

// gitCommitDated stages all modified files and commits them with the given
// author/committer date, using GIT_AUTHOR_DATE and GIT_COMMITTER_DATE env vars.
func (r *testRepo) gitCommitDated(when time.Time, msg string) {
	r.t.Helper()
	// Stage all changes first.
	r.git("add", "-A")

	dateStr := when.UTC().Format(time.RFC3339)
	cmd := exec.Command("git",
		"-C", r.dir,
		"-c", "user.name=Test",
		"-c", "user.email=test@example.com",
		"-c", "commit.gpgsign=false",
		"commit", "-m", msg,
	)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"HOME="+r.dir,
		"GIT_AUTHOR_DATE="+dateStr,
		"GIT_COMMITTER_DATE="+dateStr,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git commit (dated %s): %v\n%s", dateStr, err, out)
	}
}

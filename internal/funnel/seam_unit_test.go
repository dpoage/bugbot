package funnel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// TestBuildSeamTask_NamesBothSides verifies the cross-language-boundary custom
// task names the seam key and EVERY participating side (file + language), so a
// finder can read both ends of the contract. This is acceptance criterion (2):
// the boundary unit receives one seam with both sides named.
func TestBuildSeamTask_NamesBothSides(t *testing.T) {
	seam := ingest.Seam{
		Kind: ingest.SeamDataFile,
		Key:  "metrics.json",
		Sides: []ingest.SeamSide{
			{File: "producer.py", Language: ingest.LangPython, Line: 7},
			{File: "consumer.go", Language: ingest.LangGo, Line: 12},
		},
	}
	task := buildSeamTask(seam)
	for _, want := range []string{
		"metrics.json",
		"producer.py", string(ingest.LangPython),
		"consumer.go", string(ingest.LangGo),
		"BOTH sides",
	} {
		if !strings.Contains(task, want) {
			t.Errorf("seam task missing %q\ntask:\n%s", want, task)
		}
	}
}

// TestBuildSeamTask_EnvVarKind frames an env-var seam distinctly from a data
// file so the finder knows what surface it is auditing.
func TestBuildSeamTask_EnvVarKind(t *testing.T) {
	seam := ingest.Seam{
		Kind:  ingest.SeamEnvVar,
		Key:   "API_TOKEN",
		Sides: []ingest.SeamSide{{File: "a.go", Language: ingest.LangGo}, {File: "b.py", Language: ingest.LangPython}},
	}
	task := buildSeamTask(seam)
	if !strings.Contains(task, "ENVIRONMENT VARIABLE") || !strings.Contains(task, "API_TOKEN") {
		t.Errorf("env-var seam task missing env framing or key:\n%s", task)
	}
}

// TestBuildUnits_ExcludesBoundaryFromChunkPath asserts the cross-language-boundary
// lens never emits per-chunk units (it is custom-unit only, like diff-intent):
// buildUnits must skip it even though it is a builtin lens.
func TestBuildUnits_ExcludesBoundaryFromChunkPath(t *testing.T) {
	langs := []ingest.Language{ingest.LangGo}
	chunks := []fileChunk{{files: []string{"x.go"}, langs: langs}}
	units := buildUnits(lensesByYield(BuiltinLenses(), langs), builtinStrategies(), chunks, nil)
	for _, u := range units {
		if u.lens.Name == "cross-language-boundary" {
			t.Fatalf("cross-language-boundary emitted a chunk unit; it must be custom-unit only")
		}
	}
}

// TestSeam_SweepPopulatesStats runs a sweep over a polyglot fixture (a Python
// writer and a Go reader that share metrics.json with a seeded field-type
// mismatch) and asserts the seam is discovered and a boundary unit runs to a
// terminal state. This is the deterministic half of acceptance (3) and all of
// (4): EnumerateSeams surfaces the seam, the boundary custom unit fires, and
// Stats.SeamsFound/SeamsCovered report it. The seeded type mismatch (string vs
// int) is what a live finder would flag; here a scripted client returns no
// candidates, so the test exercises discovery + emission + stats, not the LLM.
func TestSeam_SweepPopulatesStats(t *testing.T) {
	st, repo := openPolyglotSeamFixture(t)

	finder := newScriptedClient()
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{Limits: StageLimits{MaxParallel: 4}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Stats.SeamsFound < 1 {
		t.Fatalf("SeamsFound = %d, want >= 1 (metrics.json shared by producer.py and consumer.go)", res.Stats.SeamsFound)
	}
	if res.Stats.SeamsCovered < 1 {
		t.Errorf("SeamsCovered = %d, want >= 1 (the boundary unit should run to a terminal state)", res.Stats.SeamsCovered)
	}
	if res.Stats.SeamsCovered > res.Stats.SeamsFound {
		t.Errorf("SeamsCovered (%d) > SeamsFound (%d): coverage cannot exceed discovery", res.Stats.SeamsCovered, res.Stats.SeamsFound)
	}
}

// openPolyglotSeamFixture builds a git repo whose Python and Go files share a
// serialized data file (metrics.json) so EnumerateSeams reports one data-file
// seam with both sides. Mirrors newFixtureRepo's git seed sequence.
func openPolyglotSeamFixture(t *testing.T) (*store.Store, *ingest.Repo) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("producer.py", `import json


def write_metrics(count):
    # Seeded mismatch: count is serialized as a string, but the Go reader
    # below unmarshals "count" into an int field.
    with open("metrics.json", "w") as fh:
        json.dump({"count": str(count)}, fh)
`)
	write("consumer.go", `package main

import (
	"encoding/json"
	"os"
)

type Metrics struct {
	Count int `+"`"+`json:"count"`+"`"+`
}

func readMetrics() (Metrics, error) {
	var m Metrics
	b, err := os.ReadFile("metrics.json")
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}
`)
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	runGit("init", "-q")
	runGit("add", ".")
	runGit("commit", "-q", "-m", "seed")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo, err := ingest.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	return st, repo
}

package report

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGolden, when set via `go test -run TestMarkdown -update`, rewrites the
// golden file instead of comparing against it.
var updateGolden = flag.Bool("update", false, "update golden files")

func TestMarkdownGolden(t *testing.T) {
	r := New(fixtureFindings(), fixtureMeta())
	got := Markdown(r)

	goldenPath := filepath.Join("testdata", "report.golden.md")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote golden %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("markdown mismatch.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestMarkdownDeterministicOrdering(t *testing.T) {
	// Reverse the fixture order; New must re-sort to the same canonical output.
	fs := fixtureFindings()
	for i, j := 0, len(fs)-1; i < j; i, j = i+1, j-1 {
		fs[i], fs[j] = fs[j], fs[i]
	}
	a := Markdown(New(fixtureFindings(), fixtureMeta()))
	b := Markdown(New(fs, fixtureMeta()))
	if a != b {
		t.Fatal("markdown output is not order-independent after New sorts")
	}
}

func TestMarkdownContainsRequiredElements(t *testing.T) {
	r := New(fixtureFindings(), fixtureMeta())
	got := Markdown(r)

	// critical (T1) must appear before high before low.
	posCrit := strings.Index(got, "data race on shared counter")
	posHigh := strings.Index(got, "nil pointer dereference")
	posLow := strings.Index(got, "ignored error from Close")
	if !(posCrit < posHigh && posHigh < posLow) {
		t.Fatalf("findings not ordered by severity desc: crit=%d high=%d low=%d", posCrit, posHigh, posLow)
	}

	for _, want := range []string{
		"T1 Reproduced",
		"T2 Verified",
		"internal/worker/pool.go:108", // file:line
		"Reasoning (verification trace)",
		".bugbot/repros/fp-t1-race/race_test.go", // repro link
		"deadbeef",                               // commit in header
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
}

func TestMarkdownEmpty(t *testing.T) {
	got := Markdown(New(nil, fixtureMeta()))
	if !strings.Contains(got, "No open findings.") {
		t.Errorf("empty report should note no findings, got:\n%s", got)
	}
}

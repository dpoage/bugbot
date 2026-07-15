package repro

// corpus_test.go is the static corpus regression suite (bugbot-ecm8
// acceptance 2): every fixture under testdata/corpus/ is a real bundle
// directory (manifest.json + its repro files, exactly the shape writeArtifacts
// produces and LoadBundle consumes) paired with an expected.json recording the
// classification the static target-execution gate (Audit, backed by
// Qb4rImpl's ClassifyTargetExecution) must produce.
//
// This suite runs under plain `go test` — no sandbox, no LLM, no target repo
// checkout — because Audit is a pure function over the bundle's own files.
// The 4 the_cloud false-T1 specimens (python_source_grep,
// python_import_absence_lint, python_no_try_finally, python_transliteration)
// pin bugbot-qb4r's acceptance criterion 4 with production evidence: any
// future change to the gate that would re-promote one of these fails here
// immediately, before it ever reaches a real target repo.
// go_genuine_behavioral is the acceptance-(d) counterpart: a test that
// actually reaches its target must NOT be flagged.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// corpusExpectation is the expected.json contract: the classification a
// corpus fixture's bundle must produce. description is documentation only
// (not asserted) — it exists so a human reading the fixture understands why
// it is in the corpus without cross-referencing this file.
type corpusExpectation struct {
	Reason      string `json:"reason"`
	Description string `json:"description"`
}

// TestCorpus runs Audit (the static, sandbox-free target-execution gate)
// over every fixture under testdata/corpus and asserts it matches that
// fixture's expected.json.
func TestCorpus(t *testing.T) {
	entries, err := os.ReadDir("testdata/corpus")
	if err != nil {
		t.Fatalf("read testdata/corpus: %v", err)
	}
	found := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		found++
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("testdata", "corpus", name)

			b, err := LoadBundle(dir)
			if err != nil {
				t.Fatalf("LoadBundle(%s): %v", dir, err)
			}

			raw, err := os.ReadFile(filepath.Join(dir, "expected.json"))
			if err != nil {
				t.Fatalf("read expected.json: %v", err)
			}
			var want corpusExpectation
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("parse expected.json: %v", err)
			}

			got := Audit(b)
			if string(got.Reason) != want.Reason {
				t.Errorf("Audit(%s).Reason = %q, want %q (detail=%q)", name, got.Reason, want.Reason, got.Detail)
			}
		})
	}
	if found == 0 {
		t.Fatal("testdata/corpus contains no fixture directories")
	}
}

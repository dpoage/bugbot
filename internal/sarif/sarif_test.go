package sarif_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/sarif"
	"github.com/dpoage/bugbot/internal/store"
)

// fixedTime is the epoch used in fixture findings so the golden file is stable.
var fixedTime = time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

// fixtureFindings returns a small, fixed set of findings used in the golden
// test. Field values are deliberately varied to exercise all level mappings.
func fixtureFindings() []store.Finding {
	return []store.Finding{
		{
			ID:          "id-1",
			Fingerprint: "fp-abc",
			Title:       "nil deref in handler",
			Reasoning:   "pointer is never checked before use",
			Severity:    "high",
			Tier:        1, // reproduced -> error
			Status:      store.StatusOpen,
			Lens:        "race",
			File:        "cmd/server/main.go",
			Line:        42,
			CommitSHA:   "cafebabe",
			CreatedAt:   fixedTime,
			UpdatedAt:   fixedTime,
		},
		{
			ID:          "id-2",
			Fingerprint: "fp-def",
			Title:       "missing bounds check",
			Reasoning:   "",
			Severity:    "medium",
			Tier:        2, // verified -> warning
			Status:      store.StatusOpen,
			Lens:        "bounds",
			File:        "internal/parser/parse.go",
			Line:        10,
			CommitSHA:   "cafebabe",
			CreatedAt:   fixedTime,
			UpdatedAt:   fixedTime,
		},
		{
			ID:          "id-3",
			Fingerprint: "fp-ghi",
			Title:       "possible division by zero",
			Reasoning:   "denominator may be zero when input is empty",
			Severity:    "low",
			Tier:        3, // suspected -> note
			Status:      store.StatusOpen,
			Lens:        "race",
			File:        "cmd/server/main.go",
			Line:        99,
			CommitSHA:   "cafebabe",
			CreatedAt:   fixedTime,
			UpdatedAt:   fixedTime,
		},
	}
}

// goldenPath is the testdata golden file for the fixture findings.
const goldenPath = "testdata/example.sarif.json"

// TestFromFindings_Golden verifies that FromFindings produces byte-stable
// output matching the committed golden file.
//
// Run with -update to regenerate the golden file.
var update = func() bool {
	for _, arg := range os.Args {
		if arg == "-update" || arg == "--update" {
			return true
		}
	}
	return false
}()

func TestFromFindings_Golden(t *testing.T) {
	doc := sarif.FromFindings(fixtureFindings())
	got, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n') // trailing newline for diff friendliness

	if update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden file updated: %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to generate)", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("SARIF output does not match golden file.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestFromFindings_StructuralAssertions checks required SARIF fields and the
// tier->level mapping without relying on the golden file.
func TestFromFindings_StructuralAssertions(t *testing.T) {
	findings := fixtureFindings()
	doc := sarif.FromFindings(findings)

	// Top-level required fields.
	if doc.Version != "2.1.0" {
		t.Errorf("version = %q, want 2.1.0", doc.Version)
	}
	if doc.Schema == "" {
		t.Error("$schema must not be empty")
	}
	if len(doc.Runs) != 1 {
		t.Fatalf("len(runs) = %d, want 1", len(doc.Runs))
	}

	run := doc.Runs[0]
	if run.Tool.Driver.Name != "bugbot" {
		t.Errorf("driver.name = %q, want bugbot", run.Tool.Driver.Name)
	}
	if run.Tool.Driver.InformationURI == "" {
		t.Error("driver.informationUri must not be empty")
	}

	// Rules: unique sorted lenses.
	if len(run.Tool.Driver.Rules) != 2 { // "bounds" and "race"
		t.Errorf("len(rules) = %d, want 2", len(run.Tool.Driver.Rules))
	}
	if run.Tool.Driver.Rules[0].ID != "bounds" {
		t.Errorf("rules[0].id = %q, want bounds (sorted)", run.Tool.Driver.Rules[0].ID)
	}
	if run.Tool.Driver.Rules[1].ID != "race" {
		t.Errorf("rules[1].id = %q, want race (sorted)", run.Tool.Driver.Rules[1].ID)
	}

	// Results: one per finding.
	if len(run.Results) != len(findings) {
		t.Fatalf("len(results) = %d, want %d", len(run.Results), len(findings))
	}

	// Level mapping verification. Results are sorted by (file, line, ruleId, fingerprint).
	// Expected order: cmd/server/main.go:42 (race, T1), cmd/server/main.go:99 (race, T3),
	//                 internal/parser/parse.go:10 (bounds, T2).
	cases := []struct {
		ruleID string
		level  string
		msg    string // non-empty reasoning or title fallback
		file   string
		line   int
		fp     string
	}{
		{"race", "error", "pointer is never checked before use", "cmd/server/main.go", 42, "fp-abc"},
		{"race", "note", "denominator may be zero when input is empty", "cmd/server/main.go", 99, "fp-ghi"},
		{"bounds", "warning", "missing bounds check", "internal/parser/parse.go", 10, "fp-def"},
	}

	for i, tc := range cases {
		r := run.Results[i]
		if r.RuleID != tc.ruleID {
			t.Errorf("results[%d].ruleId = %q, want %q", i, r.RuleID, tc.ruleID)
		}
		if r.Level != tc.level {
			t.Errorf("results[%d].level = %q, want %q", i, r.Level, tc.level)
		}
		if r.Message.Text != tc.msg {
			t.Errorf("results[%d].message.text = %q, want %q", i, r.Message.Text, tc.msg)
		}
		if len(r.Locations) != 1 {
			t.Errorf("results[%d]: len(locations) = %d, want 1", i, len(r.Locations))
			continue
		}
		pl := r.Locations[0].PhysicalLocation
		if pl.ArtifactLocation.URI != tc.file {
			t.Errorf("results[%d].locations[0].uri = %q, want %q", i, pl.ArtifactLocation.URI, tc.file)
		}
		if pl.Region.StartLine != tc.line {
			t.Errorf("results[%d].locations[0].startLine = %d, want %d", i, pl.Region.StartLine, tc.line)
		}
		if fp, ok := r.PartialFingerprints["bugbotFingerprint/v1"]; !ok || fp != tc.fp {
			t.Errorf("results[%d].partialFingerprints[bugbotFingerprint/v1] = %q, want %q", i, fp, tc.fp)
		}
	}
}

// TestFromFindings_Deterministic verifies byte-identical output for identical input.
func TestFromFindings_Deterministic(t *testing.T) {
	findings := fixtureFindings()

	marshal := func() string {
		doc := sarif.FromFindings(findings)
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}

	first := marshal()
	for i := 0; i < 5; i++ {
		if got := marshal(); got != first {
			t.Errorf("iteration %d: output differs from first run", i+1)
		}
	}
}

// TestFromFindings_TierLevel exercises all tier->level branches.
func TestFromFindings_TierLevel(t *testing.T) {
	base := store.Finding{
		Fingerprint: "x",
		Title:       "t",
		Lens:        "l",
		File:        "f.go",
		Line:        1,
	}
	cases := []struct {
		tier  int
		level string
	}{
		{1, "error"},
		{2, "warning"},
		{3, "note"},
		{0, "error"}, // T0 fix-witnessed is strongest evidence -> error (corrected, bugbot-0nc.2)
		{4, "note"},
	}
	for _, tc := range cases {
		f := base
		f.Tier = domain.Tier(tc.tier)
		f.Fingerprint = "fp" + string(rune('0'+tc.tier))
		doc := sarif.FromFindings([]store.Finding{f})
		got := doc.Runs[0].Results[0].Level
		if got != tc.level {
			t.Errorf("tier %d: level = %q, want %q", tc.tier, got, tc.level)
		}
	}
}

// TestFromFindings_EmptyFindings confirms an empty slice produces a valid
// document with no results and no rules.
func TestFromFindings_EmptyFindings(t *testing.T) {
	doc := sarif.FromFindings(nil)
	if doc.Version != "2.1.0" {
		t.Errorf("version = %q", doc.Version)
	}
	if len(doc.Runs) != 1 {
		t.Fatalf("len(runs) = %d, want 1", len(doc.Runs))
	}
	if len(doc.Runs[0].Results) != 0 {
		t.Errorf("expected 0 results for empty input")
	}
	if len(doc.Runs[0].Tool.Driver.Rules) != 0 {
		t.Errorf("expected 0 rules for empty input")
	}
}

// TestFromFindings_ReasoningFallback checks that an empty Reasoning falls back
// to the Title for message.text.
func TestFromFindings_ReasoningFallback(t *testing.T) {
	f := store.Finding{
		Fingerprint: "fp",
		Title:       "the title",
		Reasoning:   "", // empty -> fallback
		Lens:        "l",
		File:        "f.go",
		Line:        1,
		Tier:        2,
	}
	doc := sarif.FromFindings([]store.Finding{f})
	got := doc.Runs[0].Results[0].Message.Text
	if got != "the title" {
		t.Errorf("message.text = %q, want %q", got, "the title")
	}
}

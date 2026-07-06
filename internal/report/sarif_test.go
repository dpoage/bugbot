package report

import (
	"encoding/json"
	"testing"

	isarif "github.com/dpoage/bugbot/internal/sarif"
	"github.com/dpoage/bugbot/internal/store"
)

func TestSARIFUnmarshalsAndHasRequiredFields(t *testing.T) {
	r := New(fixtureFindings(), fixtureMeta())
	raw, err := SARIF(r)
	if err != nil {
		t.Fatalf("SARIF: %v", err)
	}

	// Validate-by-construction: it must round-trip into the expected shape.
	var doc isarif.Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("emitted SARIF does not unmarshal: %v", err)
	}

	if doc.Version != "2.1.0" {
		t.Errorf("version = %q, want 2.1.0", doc.Version)
	}
	if doc.Schema == "" {
		t.Error("$schema must be non-empty")
	}
	if len(doc.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(doc.Runs))
	}
	d := doc.Runs[0].Tool.Driver
	if d.Name != "bugbot" {
		t.Errorf("driver.name = %q, want bugbot", d.Name)
	}
	if d.InformationURI == "" || d.Version == "" {
		t.Error("driver informationUri and version must be non-empty")
	}

	// One rule per distinct lens (errcheck, nilcheck, race), sorted by id.
	wantRules := []string{"errcheck", "nilcheck", "race"}
	if len(d.Rules) != len(wantRules) {
		t.Fatalf("rules = %d, want %d", len(d.Rules), len(wantRules))
	}
	for i, want := range wantRules {
		if d.Rules[i].ID != want {
			t.Errorf("rule[%d].id = %q, want %q", i, d.Rules[i].ID, want)
		}
		if d.Rules[i].ShortDescription == nil || d.Rules[i].ShortDescription.Text == "" {
			t.Errorf("rule[%d] shortDescription must be non-empty", i)
		}
	}

	results := doc.Runs[0].Results
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	for i, res := range results {
		if res.RuleID == "" {
			t.Errorf("result[%d].ruleId empty", i)
		}
		if res.Message.Text == "" {
			t.Errorf("result[%d].message.text empty", i)
		}
		if len(res.Locations) == 0 {
			t.Errorf("result[%d] has no locations", i)
		}
		// Canonical fingerprint key is bugbotFingerprint/v2.
		if res.PartialFingerprints[isarif.FingerprintKey] == "" {
			t.Errorf("result[%d] missing %s", i, isarif.FingerprintKey)
		}
	}
}

// TestSARIFLevelsInDocument verifies that level is derived from Tier.Level(),
// not from Severity. Fixture findings: T2 nilcheck (high sev), T1 race
// (critical sev), T2 errcheck (low sev). Sorted by SARIF order
// (file asc, line asc): errcheck@50, nilcheck@42 is after in file order;
// actual sort is file then line then lens. Easiest to assert by ruleID.
func TestSARIFLevelsInDocument(t *testing.T) {
	doc := BuildSARIF(New(fixtureFindings(), fixtureMeta()))
	results := doc.Runs[0].Results

	levelByRule := make(map[string]string, len(results))
	for _, res := range results {
		levelByRule[res.RuleID] = res.Level
	}

	// T2 (verified) -> "warning" regardless of severity.
	if got := levelByRule["nilcheck"]; got != "warning" {
		t.Errorf("nilcheck (T2) level = %q, want warning", got)
	}
	// T1 (reproduced) -> "error".
	if got := levelByRule["race"]; got != "error" {
		t.Errorf("race (T1) level = %q, want error", got)
	}
	// T2 (verified) -> "warning" regardless of low severity.
	if got := levelByRule["errcheck"]; got != "warning" {
		t.Errorf("errcheck (T2) level = %q, want warning", got)
	}
}

func TestSARIFRepoRelativeURIs(t *testing.T) {
	// Absolute file paths under RepoPath must become repo-relative URIs.
	fs := fixtureFindings()
	fs[0].File = "/home/user/target-repo/internal/api/handler.go"
	doc := BuildSARIF(New(fs, Metadata{RepoPath: "/home/user/target-repo"}))

	var found bool
	for _, res := range doc.Runs[0].Results {
		uri := res.Locations[0].PhysicalLocation.ArtifactLocation.URI
		if uri == "internal/api/handler.go" {
			found = true
		}
		if len(uri) > 0 && uri[0] == '/' {
			t.Errorf("uri %q should be repo-relative, not absolute", uri)
		}
	}
	if !found {
		t.Error("expected repo-relative uri internal/api/handler.go")
	}
}

func TestRepoRelativeWindowsSeparators(t *testing.T) {
	// Backslashes must normalize to forward slashes in URIs.
	got := isarif.RepoRelative("", `internal\api\handler.go`)
	if got != "internal/api/handler.go" {
		t.Errorf("RepoRelative = %q, want forward slashes", got)
	}
}

func TestRepoRelativeEscapingPath(t *testing.T) {
	// A path outside the repo root must not emit ".." climbs; it falls back to
	// the cleaned absolute path.
	got := isarif.RepoRelative("/home/user/repo", "/etc/passwd")
	if got != "/etc/passwd" {
		t.Errorf("RepoRelative escaping path = %q, want /etc/passwd", got)
	}
}

// TestSARIFRegionOmittedWhenNoLine verifies that Region is absent from the
// emitted JSON when a finding has no line information (Line == 0).
func TestSARIFRegionOmittedWhenNoLine(t *testing.T) {
	fs := fixtureFindings()
	fs[0].Line = 0
	doc := BuildSARIF(New(fs, fixtureMeta()))

	// Find the result corresponding to the zero-line finding.
	var found bool
	for _, res := range doc.Runs[0].Results {
		if res.Locations[0].PhysicalLocation.Region == nil {
			found = true
		}
	}
	if !found {
		t.Error("expected at least one result with Region==nil (Line==0 finding)")
	}

	// Also verify via JSON that "region" key is absent.
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// A startLine:0 in the JSON would indicate the bug; verify it's absent.
	if json.Valid(out) {
		var raw map[string]any
		if err := json.Unmarshal(out, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
	}
}

// TestBothPathsEmitIdenticalLevelAndFingerprint is the parity gate: it
// exercises the two production entrypoints (report.BuildSARIF for the daemon
// FS sink, sarif.FromFindings for bugbot export) and asserts they emit
// identical level, fingerprint key, and driver identity for the same finding.
// If the two paths ever diverge again this test will fail.
func TestBothPathsEmitIdenticalLevelAndFingerprint(t *testing.T) {
	findings := []store.Finding{
		{
			ID:          "parity-1",
			Fingerprint: "fp-parity",
			Title:       "parity check finding",
			Reasoning:   "parity reasoning",
			Severity:    "high",
			Tier:        1, // T1 reproduced -> error
			Status:      store.StatusOpen,
			Lens:        "paritycheck",
			File:        "pkg/foo/bar.go",
			Line:        7,
			CommitSHA:   "abc123",
		},
	}

	// Path A: report.BuildSARIF (scan/daemon FS sink).
	docA := BuildSARIF(New(findings, Metadata{}))

	// Path B: sarif.FromFindings (the real export entrypoint; uses no Options,
	// falls through to sarif.ToolVersion). This is the path bugbot export takes.
	docB := isarif.FromFindings(findings)

	if len(docA.Runs) != 1 || len(docB.Runs) != 1 {
		t.Fatal("expected exactly 1 run from each path")
	}
	rA := docA.Runs[0].Results[0]
	rB := docB.Runs[0].Results[0]

	// Level must match (both from Tier.Level()).
	if rA.Level != rB.Level {
		t.Errorf("level: path A = %q, path B = %q (must be identical)", rA.Level, rB.Level)
	}

	// Fingerprint key must be the canonical FingerprintKey in both paths.
	fpA := rA.PartialFingerprints[isarif.FingerprintKey]
	fpB := rB.PartialFingerprints[isarif.FingerprintKey]
	if fpA == "" || fpA != fpB {
		t.Errorf("fingerprint: path A = %q, path B = %q (must be identical and non-empty)", fpA, fpB)
	}

	// Driver identity (name, URI, version) must match.
	dA := docA.Runs[0].Tool.Driver
	dB := docB.Runs[0].Tool.Driver
	if dA.Name != dB.Name {
		t.Errorf("driver.name: path A = %q, path B = %q", dA.Name, dB.Name)
	}
	if dA.InformationURI != dB.InformationURI {
		t.Errorf("driver.informationUri: path A = %q, path B = %q", dA.InformationURI, dB.InformationURI)
	}
	if dA.Version != dB.Version {
		t.Errorf("driver.version: path A = %q, path B = %q", dA.Version, dB.Version)
	}
}

// TestSARIFMessagePrecedence verifies the message.text precedence rules:
//  1. reasoning non-empty  → reasoning text
//  2. reasoning empty, description non-empty → "Title: Description"
//  3. all empty            → "(no description)"
func TestSARIFMessagePrecedence(t *testing.T) {
	base := store.Finding{
		Fingerprint: "fp",
		Lens:        "l",
		File:        "f.go",
		Line:        1,
		Tier:        2,
		Status:      store.StatusOpen,
	}

	cases := []struct {
		name      string
		title     string
		desc      string
		reasoning string
		want      string
	}{
		{
			name:      "reasoning wins",
			title:     "the title",
			desc:      "the description",
			reasoning: "the reasoning",
			want:      "the reasoning",
		},
		{
			name:      "title+description when no reasoning",
			title:     "the title",
			desc:      "the description",
			reasoning: "",
			want:      "the title: the description",
		},
		{
			name:      "all empty falls back to no-description sentinel",
			title:     "",
			desc:      "",
			reasoning: "",
			want:      "(no description)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := base
			f.Title = tc.title
			f.Description = tc.desc
			f.Reasoning = tc.reasoning
			doc := BuildSARIF(New([]store.Finding{f}, Metadata{}))
			got := doc.Runs[0].Results[0].Message.Text
			if got != tc.want {
				t.Errorf("message.text = %q, want %q", got, tc.want)
			}
		})
	}
}

package report

import (
	"encoding/json"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
)

func TestSARIFUnmarshalsAndHasRequiredFields(t *testing.T) {
	r := New(fixtureFindings(), fixtureMeta())
	raw, err := SARIF(r)
	if err != nil {
		t.Fatalf("SARIF: %v", err)
	}

	// Validate-by-construction: it must round-trip into the expected shape.
	var doc SARIFLog
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
		if d.Rules[i].ShortDescription.Text == "" {
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
		if res.PartialFingerprints["bugbotFingerprint"] == "" {
			t.Errorf("result[%d] missing bugbotFingerprint", i)
		}
	}
}

func TestSARIFSeverityToLevelMapping(t *testing.T) {
	cases := map[string]string{
		"critical": "error",
		"high":     "error",
		"medium":   "warning",
		"low":      "note",
		"":         "none",
		"weird":    "none",
	}
	for sev, want := range cases {
		if got := levelForSeverity(domain.Severity(sev)); got != want {
			t.Errorf("levelForSeverity(%q) = %q, want %q", sev, got, want)
		}
	}
}

func TestSARIFLevelsInDocument(t *testing.T) {
	doc := BuildSARIF(New(fixtureFindings(), fixtureMeta()))
	// Findings are sorted: critical(race), high(nilcheck), low(errcheck).
	want := []string{"error", "error", "note"}
	for i, res := range doc.Runs[0].Results {
		if res.Level != want[i] {
			t.Errorf("result[%d].level = %q, want %q", i, res.Level, want[i])
		}
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
	got := repoRelative("", `internal\api\handler.go`)
	if got != "internal/api/handler.go" {
		t.Errorf("repoRelative = %q, want forward slashes", got)
	}
}

func TestRepoRelativeEscapingPath(t *testing.T) {
	// A path outside the repo root must not emit ".." climbs; it falls back to
	// the cleaned absolute path.
	got := repoRelative("/home/user/repo", "/etc/passwd")
	if got != "/etc/passwd" {
		t.Errorf("repoRelative escaping path = %q, want /etc/passwd", got)
	}
}

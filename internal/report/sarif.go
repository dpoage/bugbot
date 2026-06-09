package report

import (
	"encoding/json"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dpoage/bugbot/internal/store"
)

// SARIF 2.1.0 emitter.
//
// These structs are a hand-rolled subset of the SARIF 2.1.0 schema sufficient
// for Bugbot's findings; we use encoding/json rather than an external SARIF
// library (stdlib-only constraint). The emitted document is validated by
// construction in sarif_test.go: it must round-trip through these structs with
// the required fields (version, $schema, runs[].tool.driver.name, each result's
// ruleId/message/locations) non-empty. Full JSON-schema validation against the
// upstream SARIF 2.1.0 schema can be wired into CI later if stricter conformance
// is wanted; it is intentionally out of scope here.
//
// Severity -> SARIF level mapping:
//
//	critical, high -> "error"
//	medium         -> "warning"
//	low            -> "note"
//	(anything else)-> "none"
const (
	sarifVersion = "2.1.0"
	// sarifSchema is the canonical schema URL for SARIF 2.1.0.
	sarifSchema = "https://raw.githubusercontent.com/oasis-tcs/sarif-spec/master/Schemata/sarif-schema-2.1.0.json"
)

// SARIFLog is the root SARIF document.
type SARIFLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []SARIFRun `json:"runs"`
}

// SARIFRun is a single analysis run.
type SARIFRun struct {
	Tool    SARIFTool     `json:"tool"`
	Results []SARIFResult `json:"results"`
}

// SARIFTool wraps the driver component.
type SARIFTool struct {
	Driver SARIFDriver `json:"driver"`
}

// SARIFDriver identifies the analysis tool and declares its rules.
type SARIFDriver struct {
	Name           string      `json:"name"`
	InformationURI string      `json:"informationUri"`
	Version        string      `json:"version"`
	Rules          []SARIFRule `json:"rules"`
}

// SARIFRule describes one rule (here, one per Bugbot lens).
type SARIFRule struct {
	ID               string             `json:"id"`
	Name             string             `json:"name,omitempty"`
	ShortDescription SARIFMessageString `json:"shortDescription"`
}

// SARIFResult is a single reported finding.
type SARIFResult struct {
	RuleID              string             `json:"ruleId"`
	Level               string             `json:"level"`
	Message             SARIFMessageString `json:"message"`
	Locations           []SARIFLocation    `json:"locations"`
	PartialFingerprints map[string]string  `json:"partialFingerprints,omitempty"`
	Properties          map[string]any     `json:"properties,omitempty"`
}

// SARIFMessageString is the {text: ...} shape used for messages and short
// descriptions.
type SARIFMessageString struct {
	Text string `json:"text"`
}

// SARIFLocation locates a result in source.
type SARIFLocation struct {
	PhysicalLocation SARIFPhysicalLocation `json:"physicalLocation"`
}

// SARIFPhysicalLocation pins an artifact and a region within it.
type SARIFPhysicalLocation struct {
	ArtifactLocation SARIFArtifactLocation `json:"artifactLocation"`
	Region           *SARIFRegion          `json:"region,omitempty"`
}

// SARIFArtifactLocation names the file (repo-relative URI).
type SARIFArtifactLocation struct {
	URI string `json:"uri"`
}

// SARIFRegion is a 1-based line region.
type SARIFRegion struct {
	StartLine int `json:"startLine"`
}

// levelForSeverity maps a Bugbot severity to a SARIF result level.
func levelForSeverity(sev string) string {
	switch strings.ToLower(strings.TrimSpace(sev)) {
	case "critical", "high":
		return "error"
	case "medium":
		return "warning"
	case "low":
		return "note"
	default:
		return "none"
	}
}

// repoRelative returns file made relative to repoPath when possible, with
// backslashes normalized to forward slashes (SARIF URIs use "/"). When file is
// already relative, or relativization fails, the cleaned slash form of file is
// returned unchanged. Absolute paths that escape repoPath are returned as-is
// (slash-normalized) rather than emitting "../" climbs.
func repoRelative(repoPath, file string) string {
	if file == "" {
		return ""
	}
	// Normalize Windows-style separators up front so URIs always use "/",
	// regardless of the host OS running the report (mirrors store.Fingerprint).
	norm := func(p string) string {
		return path.Clean(strings.ReplaceAll(filepath.ToSlash(p), "\\", "/"))
	}
	file = strings.ReplaceAll(file, "\\", "/")
	repoPath = strings.ReplaceAll(repoPath, "\\", "/")

	if repoPath == "" || !path.IsAbs(file) {
		return norm(file)
	}
	rel, err := filepath.Rel(repoPath, file)
	if err != nil || strings.HasPrefix(rel, "..") {
		return norm(file)
	}
	return filepath.ToSlash(rel)
}

// BuildSARIF constructs the SARIF document for a report. It is exported so tests
// (and callers wanting the typed form) can inspect the structure before
// serialization.
func BuildSARIF(r Report) SARIFLog {
	driver := SARIFDriver{
		Name:           toolName,
		InformationURI: informationURI,
		Version:        Version,
		Rules:          rulesForFindings(r.Findings),
	}

	results := make([]SARIFResult, 0, len(r.Findings))
	for _, f := range r.Findings {
		results = append(results, resultForFinding(r.Meta, f))
	}

	return SARIFLog{
		Schema:  sarifSchema,
		Version: sarifVersion,
		Runs: []SARIFRun{{
			Tool:    SARIFTool{Driver: driver},
			Results: results,
		}},
	}
}

// rulesForFindings derives one rule per distinct lens, sorted by id for
// deterministic output. A lens with no findings produces no rule.
func rulesForFindings(fs []store.Finding) []SARIFRule {
	seen := map[string]string{} // lens -> a representative title for the description
	for _, f := range fs {
		lens := f.Lens
		if lens == "" {
			lens = "unknown"
		}
		if _, ok := seen[lens]; !ok {
			seen[lens] = "Findings from the " + lens + " lens."
		}
	}
	rules := make([]SARIFRule, 0, len(seen))
	for lens, desc := range seen {
		rules = append(rules, SARIFRule{
			ID:               lens,
			Name:             lens,
			ShortDescription: SARIFMessageString{Text: desc},
		})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	return rules
}

// resultForFinding maps one finding to a SARIF result.
func resultForFinding(meta Metadata, f store.Finding) SARIFResult {
	ruleID := f.Lens
	if ruleID == "" {
		ruleID = "unknown"
	}

	msg := f.Title
	if f.Description != "" {
		if msg != "" {
			msg += ": " + f.Description
		} else {
			msg = f.Description
		}
	}
	if msg == "" {
		msg = "(no description)"
	}

	uri := repoRelative(meta.RepoPath, f.File)

	var region *SARIFRegion
	if f.Line > 0 {
		region = &SARIFRegion{StartLine: f.Line}
	}

	props := map[string]any{
		"tier":     f.Tier,
		"tierName": tierName(f.Tier),
		"status":   string(f.Status),
		"severity": f.Severity,
	}
	if f.Reasoning != "" {
		props["reasoning"] = f.Reasoning
	}
	if f.ReproPath != "" {
		props["reproPath"] = f.ReproPath
	}
	if f.CommitSHA != "" {
		props["commitSha"] = f.CommitSHA
	}

	return SARIFResult{
		RuleID:    ruleID,
		Level:     levelForSeverity(f.Severity),
		Message:   SARIFMessageString{Text: msg},
		Locations: []SARIFLocation{{PhysicalLocation: SARIFPhysicalLocation{ArtifactLocation: SARIFArtifactLocation{URI: uri}, Region: region}}},
		PartialFingerprints: map[string]string{
			"bugbotFingerprint": f.Fingerprint,
		},
		Properties: props,
	}
}

// SARIF renders the report as pretty-printed SARIF 2.1.0 JSON with a trailing
// newline. Marshaling of these fixed structs cannot fail, but the error is
// returned to keep the signature honest and future-proof.
func SARIF(r Report) ([]byte, error) {
	doc := BuildSARIF(r)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

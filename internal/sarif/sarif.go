// Package sarif provides SARIF 2.1.0 export for Bugbot findings.
//
// Only the minimal subset required for GitHub Code Scanning upload is
// implemented. The design is intentionally dependency-light: hand-rolled
// structs marshaled via encoding/json.
package sarif

import (
	"sort"

	"github.com/dpoage/bugbot/internal/store"
)

const (
	// Schema is the canonical SARIF 2.1.0 JSON Schema URI.
	Schema = "https://raw.githubusercontent.com/oasis-tcs/sarif-spec/master/Schemata/sarif-schema-2.1.0.json"
	// Version is the SARIF format version.
	Version = "2.1.0"

	// InformationURI points at the Bugbot project.
	InformationURI = "https://github.com/dpoage/bugbot"
)

// Document is the top-level SARIF log.
type Document struct {
	Schema  string `json:"$schema"`
	Version string `json:"version"`
	Runs    []Run  `json:"runs"`
}

// Run describes one analysis execution.
type Run struct {
	Tool    Tool     `json:"tool"`
	Results []Result `json:"results"`
}

// Tool identifies the analysis tool that produced the results.
type Tool struct {
	Driver Driver `json:"driver"`
}

// Driver is the primary tool component.
type Driver struct {
	Name           string `json:"name"`
	InformationURI string `json:"informationUri"`
	Version        string `json:"version,omitempty"`
	Rules          []Rule `json:"rules"`
}

// Rule describes one analysis rule (one per distinct lens).
type Rule struct {
	ID               string         `json:"id"`
	Name             string         `json:"name,omitempty"`
	ShortDescription *MessageString `json:"shortDescription,omitempty"`
}

// Result is a single finding.
type Result struct {
	RuleID              string            `json:"ruleId"`
	Level               string            `json:"level"`
	Message             MessageString     `json:"message"`
	Locations           []Location        `json:"locations"`
	PartialFingerprints map[string]string `json:"partialFingerprints,omitempty"`
	Properties          map[string]any    `json:"properties,omitempty"`
}

// MessageString holds a human-readable text value used for messages and
// short descriptions.
type MessageString struct {
	Text string `json:"text"`
}

// Location anchors a result to a source artifact.
type Location struct {
	PhysicalLocation PhysicalLocation `json:"physicalLocation"`
}

// PhysicalLocation identifies the artifact and region within it.
type PhysicalLocation struct {
	ArtifactLocation ArtifactLocation `json:"artifactLocation"`
	// Region is omitted (nil) when the finding has no line information (Line<=0).
	Region *Region `json:"region,omitempty"`
}

// ArtifactLocation is the URI of the source file.
type ArtifactLocation struct {
	URI string `json:"uri"`
}

// Region is the line range within the artifact.
type Region struct {
	StartLine int `json:"startLine"`
}

// messageText returns the reasoning text when non-empty, falling back to the
// title. SARIF requires a non-empty message.text.
func messageText(f store.Finding) string {
	if f.Reasoning != "" {
		return f.Reasoning
	}
	return f.Title
}

// FromFindings converts a slice of store findings into a SARIF Document.
//
// Rules are collected from the unique set of finding.Lens values and sorted for
// determinism. Results are sorted by (File, Line, RuleID, Fingerprint) so the
// output is byte-stable across runs.
//
// Level is derived from the finding's Tier via domain.Tier.Level().
// Region is omitted when Line <= 0.
func FromFindings(findings []store.Finding) Document {
	// Collect unique lenses preserving insertion order for rule dedup, then sort.
	seen := make(map[string]struct{}, len(findings))
	var lenses []string
	for _, f := range findings {
		if _, ok := seen[f.Lens]; !ok {
			seen[f.Lens] = struct{}{}
			lenses = append(lenses, f.Lens)
		}
	}
	sort.Strings(lenses)

	rules := make([]Rule, 0, len(lenses))
	for _, l := range lenses {
		rules = append(rules, Rule{ID: l, Name: l})
	}

	// Sort findings deterministically before building results.
	sorted := make([]store.Finding, len(findings))
	copy(sorted, findings)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Lens != b.Lens {
			return a.Lens < b.Lens
		}
		return a.Fingerprint < b.Fingerprint
	})

	results := make([]Result, 0, len(sorted))
	for _, f := range sorted {
		var region *Region
		if f.Line > 0 {
			region = &Region{StartLine: f.Line}
		}
		r := Result{
			RuleID: f.Lens,
			Level:  f.Tier.Level(),
			Message: MessageString{
				Text: messageText(f),
			},
			Locations: []Location{
				{
					PhysicalLocation: PhysicalLocation{
						ArtifactLocation: ArtifactLocation{URI: f.File},
						Region:           region,
					},
				},
			},
		}
		if f.Fingerprint != "" {
			r.PartialFingerprints = map[string]string{
				"bugbotFingerprint/v1": f.Fingerprint,
			}
		}
		results = append(results, r)
	}

	return Document{
		Schema:  Schema,
		Version: Version,
		Runs: []Run{
			{
				Tool: Tool{
					Driver: Driver{
						Name:           "bugbot",
						InformationURI: InformationURI,
						Rules:          rules,
					},
				},
				Results: results,
			},
		},
	}
}

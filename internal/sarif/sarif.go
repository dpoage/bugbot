// Package sarif provides SARIF 2.1.0 export for Bugbot findings.
//
// Only the minimal subset required for GitHub Code Scanning upload is
// implemented. The design is intentionally dependency-light: hand-rolled
// structs marshaled via encoding/json.
package sarif

import (
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dpoage/bugbot/internal/store"
)

const (
	// Schema is the canonical SARIF 2.1.0 JSON Schema URI.
	Schema = "https://raw.githubusercontent.com/oasis-tcs/sarif-spec/master/Schemata/sarif-schema-2.1.0.json"
	// Version is the SARIF format version.
	Version = "2.1.0"

	// DriverName is the canonical tool name stamped into every SARIF driver block.
	DriverName = "bugbot"
	// DriverInfoURI is the canonical home-page URI stamped into every SARIF driver block.
	DriverInfoURI = "https://github.com/dpoage/bugbot"

	// FingerprintKey is the canonical partialFingerprints key for Code Scanning deduplication.
	FingerprintKey = "bugbotFingerprint/v2"
)

// ToolVersion is the Bugbot tool version stamped into every emitted SARIF
// driver block. It is a var so a build can inject a release version via
// -ldflags "-X github.com/dpoage/bugbot/internal/sarif.ToolVersion=v1.2.3".
var ToolVersion = "0.1.0"

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

// Options controls how FromFindingsWithOptions builds a SARIF document.
type Options struct {
	// RepoPath is the absolute path of the scanned repository. When set, file
	// URIs in results are made relative to this path (SARIF best practice).
	RepoPath string
}

// RepoRelative returns file made relative to repoPath when possible, with
// backslashes normalized to forward slashes (SARIF URIs use "/"). When file is
// already relative, or relativization fails, the cleaned slash form of file is
// returned unchanged. Absolute paths that escape repoPath are returned as-is
// (slash-normalized) rather than emitting "../" climbs.
func RepoRelative(repoPath, file string) string {
	if file == "" {
		return ""
	}
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

// messageText returns the reasoning text when non-empty, falling back to the
// title + description. SARIF requires a non-empty message.text.
func messageText(f store.Finding) string {
	if f.Reasoning != "" {
		return f.Reasoning
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
	return msg
}

// FromFindings converts a slice of store findings into a SARIF Document using
// the package-level defaults (no repo-relative URI rewriting, ToolVersion for
// driver.version). Equivalent to FromFindingsWithOptions(findings, Options{}).
func FromFindings(findings []store.Finding) Document {
	return FromFindingsWithOptions(findings, Options{})
}

// FromFindingsWithOptions is the canonical finding→SARIF mapping used by both
// the scan/daemon FS sink (internal/report) and the bugbot export path
// (internal/cli export). It produces byte-stable output for identical inputs.
//
// Level is derived from Tier.Level() (domain logic; NOT from Severity).
// Fingerprint key is FingerprintKey ("bugbotFingerprint/v2").
// Rules include a ShortDescription per lens.
// Results carry a properties bag with tier, status, severity, and optional fields.
// File URIs are made repo-relative when opts.RepoPath is non-empty.
func FromFindingsWithOptions(findings []store.Finding, opts Options) Document {
	// Collect unique lenses preserving insertion order for rule dedup, then sort.
	seen := make(map[string]struct{}, len(findings))
	var lenses []string
	for _, f := range findings {
		lens := f.Lens
		if lens == "" {
			lens = "unknown"
		}
		if _, ok := seen[lens]; !ok {
			seen[lens] = struct{}{}
			lenses = append(lenses, lens)
		}
	}
	sort.Strings(lenses)

	rules := make([]Rule, 0, len(lenses))
	for _, l := range lenses {
		rules = append(rules, Rule{
			ID:               l,
			Name:             l,
			ShortDescription: &MessageString{Text: "Findings from the " + l + " lens."},
		})
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
		ruleID := f.Lens
		if ruleID == "" {
			ruleID = "unknown"
		}

		var region *Region
		if f.Line > 0 {
			region = &Region{StartLine: f.Line}
		}

		uri := RepoRelative(opts.RepoPath, f.File)

		props := map[string]any{
			"tier":     f.Tier,
			"tierName": f.Tier.Label(),
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
		if len(f.CorroboratingLenses) > 0 {
			props["corroboratingLenses"] = f.CorroboratingLenses
		}
		if f.FixPatch != "" {
			props["hasFixPatch"] = true
		}
		if f.NeedsHuman {
			props["needsHuman"] = true
		}

		r := Result{
			RuleID:  ruleID,
			Level:   f.Tier.Level(),
			Message: MessageString{Text: messageText(f)},
			Locations: []Location{{
				PhysicalLocation: PhysicalLocation{
					ArtifactLocation: ArtifactLocation{URI: uri},
					Region:           region,
				},
			}},
			Properties: props,
		}
		if f.Fingerprint != "" {
			r.PartialFingerprints = map[string]string{
				FingerprintKey: f.Fingerprint,
			}
		}
		results = append(results, r)
	}

	return Document{
		Schema:  Schema,
		Version: Version,
		Runs: []Run{{
			Tool: Tool{
				Driver: Driver{
					Name:           DriverName,
					InformationURI: DriverInfoURI,
					Version:        ToolVersion,
					Rules:          rules,
				},
			},
			Results: results,
		}},
	}
}

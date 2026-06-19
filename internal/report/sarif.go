package report

import (
	"encoding/json"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dpoage/bugbot/internal/domain"
	isarif "github.com/dpoage/bugbot/internal/sarif"
	"github.com/dpoage/bugbot/internal/store"
)

// levelForSeverity maps a Bugbot severity to a SARIF result level.
//
// Severity -> SARIF level mapping:
//
//	critical, high -> "error"
//	medium         -> "warning"
//	low            -> "note"
//	(anything else)-> "none"
func levelForSeverity(sev domain.Severity) string {
	switch sev {
	case domain.SeverityCritical, domain.SeverityHigh:
		return "error"
	case domain.SeverityMedium:
		return "warning"
	case domain.SeverityLow:
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
func BuildSARIF(r Report) isarif.Document {
	driver := isarif.Driver{
		Name:           toolName,
		InformationURI: informationURI,
		Version:        Version,
		Rules:          rulesForFindings(r.Findings),
	}

	results := make([]isarif.Result, 0, len(r.Findings))
	for _, f := range r.Findings {
		results = append(results, resultForFinding(r.Meta, f))
	}

	return isarif.Document{
		Schema:  isarif.Schema,
		Version: isarif.Version,
		Runs: []isarif.Run{{
			Tool:    isarif.Tool{Driver: driver},
			Results: results,
		}},
	}
}

// rulesForFindings derives one rule per distinct lens, sorted by id for
// deterministic output. A lens with no findings produces no rule.
func rulesForFindings(fs []store.Finding) []isarif.Rule {
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
	rules := make([]isarif.Rule, 0, len(seen))
	for lens, desc := range seen {
		d := desc
		rules = append(rules, isarif.Rule{
			ID:               lens,
			Name:             lens,
			ShortDescription: &isarif.MessageString{Text: d},
		})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	return rules
}

// resultForFinding maps one finding to a SARIF result.
func resultForFinding(meta Metadata, f store.Finding) isarif.Result {
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

	var region *isarif.Region
	if f.Line > 0 {
		region = &isarif.Region{StartLine: f.Line}
	}

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

	return isarif.Result{
		RuleID:  ruleID,
		Level:   levelForSeverity(f.Severity),
		Message: isarif.MessageString{Text: msg},
		Locations: []isarif.Location{{PhysicalLocation: isarif.PhysicalLocation{
			ArtifactLocation: isarif.ArtifactLocation{URI: uri},
			Region:           region,
		}}},
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

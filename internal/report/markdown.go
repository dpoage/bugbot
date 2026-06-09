package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dpoage/bugbot/internal/store"
)

// Markdown renders the report as a readable Markdown document. The output is
// deterministic given the same Report (findings are pre-sorted by New, and the
// only time source is Meta.GeneratedAt). The layout is:
//
//	# Bugbot Report
//	  repo / commit / generated-at / optional scan-run + stats
//	  counts by tier and by severity
//	## Findings
//	  one ### section per finding: title, tier, severity, file:line,
//	  description, the reasoning/verification trace, and a repro-artifact link
//	  when ReproPath is set.
//
// When there are no findings it renders a short "No open findings." body so the
// document is still well-formed.
func Markdown(r Report) string {
	var b strings.Builder

	b.WriteString("# Bugbot Report\n\n")

	writeMetaLine(&b, "Repository", orUnknown(r.Meta.RepoPath))
	writeMetaLine(&b, "Commit", orUnknown(r.Meta.Commit))
	if r.Meta.GeneratedAt.IsZero() {
		writeMetaLine(&b, "Generated", "unknown")
	} else {
		writeMetaLine(&b, "Generated", r.Meta.GeneratedAt.UTC().Format("2006-01-02 15:04:05 MST"))
	}
	if r.Meta.ScanRunID != "" {
		writeMetaLine(&b, "Scan run", r.Meta.ScanRunID)
	}
	writeMetaLine(&b, "Findings", fmt.Sprintf("%d", len(r.Findings)))
	if r.Meta.Stats != "" {
		writeMetaLine(&b, "Stats", r.Meta.Stats)
	}
	b.WriteString("\n")

	writeCounts(&b, r.Findings)

	b.WriteString("## Findings\n\n")
	if len(r.Findings) == 0 {
		b.WriteString("No open findings.\n")
		return b.String()
	}

	for i, f := range r.Findings {
		writeFinding(&b, i+1, f)
	}

	return b.String()
}

// writeMetaLine writes a "- **Key:** value" bullet.
func writeMetaLine(b *strings.Builder, key, value string) {
	fmt.Fprintf(b, "- **%s:** %s\n", key, value)
}

// writeCounts renders the by-tier and by-severity tallies as a compact section.
func writeCounts(b *strings.Builder, fs []store.Finding) {
	byTier := map[int]int{}
	bySev := map[string]int{}
	for _, f := range fs {
		byTier[f.Tier]++
		bySev[strings.ToLower(strings.TrimSpace(f.Severity))]++
	}

	b.WriteString("## Summary\n\n")

	b.WriteString("By tier:\n")
	for _, t := range []int{1, 2, 3} {
		if byTier[t] > 0 {
			fmt.Fprintf(b, "- %s: %d\n", tierName(t), byTier[t])
		}
	}
	// Surface any out-of-range tiers deterministically.
	var otherTiers []int
	for t := range byTier {
		if t < 1 || t > 3 {
			otherTiers = append(otherTiers, t)
		}
	}
	sort.Ints(otherTiers)
	for _, t := range otherTiers {
		fmt.Fprintf(b, "- %s: %d\n", tierName(t), byTier[t])
	}
	b.WriteString("\n")

	b.WriteString("By severity:\n")
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if bySev[sev] > 0 {
			fmt.Fprintf(b, "- %s: %d\n", sev, bySev[sev])
		}
	}
	// Unknown / empty severities, sorted for determinism.
	var others []string
	for sev := range bySev {
		switch sev {
		case "critical", "high", "medium", "low":
		default:
			label := sev
			if label == "" {
				label = "(unspecified)"
			}
			others = append(others, label)
		}
	}
	sort.Strings(others)
	for _, label := range others {
		key := label
		if label == "(unspecified)" {
			key = ""
		}
		fmt.Fprintf(b, "- %s: %d\n", label, bySev[key])
	}
	b.WriteString("\n")
}

// writeFinding renders a single finding section.
func writeFinding(b *strings.Builder, n int, f store.Finding) {
	fmt.Fprintf(b, "### %d. %s\n\n", n, orUnknown(f.Title))

	writeMetaLine(b, "ID", orUnknown(f.ID))
	writeMetaLine(b, "Tier", tierName(f.Tier))
	writeMetaLine(b, "Severity", orUnknown(f.Severity))
	writeMetaLine(b, "Lens", orUnknown(f.Lens))
	writeMetaLine(b, "Location", fmt.Sprintf("%s:%d", orUnknown(f.File), f.Line))
	writeMetaLine(b, "Status", orUnknown(string(f.Status)))
	b.WriteString("\n")

	if f.Description != "" {
		b.WriteString("**Description**\n\n")
		b.WriteString(f.Description)
		b.WriteString("\n\n")
	}

	if f.Reasoning != "" {
		b.WriteString("**Reasoning (verification trace)**\n\n")
		b.WriteString(f.Reasoning)
		b.WriteString("\n\n")
	}

	if f.ReproPath != "" {
		fmt.Fprintf(b, "**Reproduction:** [`%s`](%s)\n\n", f.ReproPath, f.ReproPath)
	}
}

// orUnknown returns s, or a placeholder when s is empty, so the rendered
// document never has dangling labels.
func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unknown)"
	}
	return s
}

// Package report renders Bugbot findings into human- and machine-readable
// forms (Markdown and SARIF 2.1.0) and delivers them through pluggable sinks
// (filesystem, stdout, and—later—issue trackers or webhooks).
//
// The package operates purely on github.com/dpoage/bugbot/internal/store types
// plus a small Metadata struct. It deliberately does NOT import internal/funnel
// or any pipeline package, so the daemon, the scan command, and the report CLI
// can all reuse it without creating an import cycle. Callers gather findings
// from the store, build a Report, and hand it to one or more Sinks.
//
// # Determinism
//
// Rendering is deterministic given the same inputs: findings are sorted by
// severity (descending) then file/line/id, and timestamps flow only from
// Metadata.GeneratedAt so tests can pin them. This makes golden-file testing of
// the Markdown output and structural testing of the SARIF output reliable.
package report

import (
	"sort"
	"strings"
	"time"

	"github.com/dpoage/bugbot/internal/store"
)

// Version is the Bugbot version stamped into emitted reports. It is a var (not
// a const) so a build can override it via -ldflags if desired.
var Version = "0.1.0"

// informationURI is advertised in SARIF as the tool's home page.
const informationURI = "https://github.com/dpoage/bugbot"

// toolName is the SARIF driver name and the Markdown report attribution.
const toolName = "bugbot"

// Metadata carries the scan context that frames a set of findings. All fields
// are optional; emitters degrade gracefully when a field is empty.
type Metadata struct {
	// RepoPath is the absolute or working path of the scanned repository. It is
	// shown in the Markdown header; SARIF URIs are made relative to it.
	RepoPath string
	// Commit is the git commit the scan ran against, if known.
	Commit string
	// GeneratedAt is the report timestamp. Callers SHOULD set this explicitly
	// (tests pin it for determinism); a zero value renders as "unknown".
	GeneratedAt time.Time
	// ScanRunID and stats are optional; when ScanRunID is set the Markdown
	// header notes it. Stats is an opaque, already-rendered string (e.g. the
	// store's stats_json) shown verbatim if non-empty.
	ScanRunID string
	Stats     string
}

// Report bundles the findings and the metadata that describe one rendering.
// Rendered Markdown/SARIF are produced on demand by the emitters; a Report does
// not cache them, keeping the type a plain value the daemon can pass around.
type Report struct {
	Findings []store.Finding
	Meta     Metadata
}

// New builds a Report, copying and sorting the findings into the canonical
// order (severity desc, then file, line, id) so every emitter and sink sees the
// same ordering. The input slice is not mutated.
func New(findings []store.Finding, meta Metadata) Report {
	sorted := make([]store.Finding, len(findings))
	copy(sorted, findings)
	SortFindings(sorted)
	return Report{Findings: sorted, Meta: meta}
}

// severityRank orders severities for display: higher number sorts first. Unknown
// severities sort last (rank 0) but before nothing, keeping output stable.
func severityRank(sev string) int {
	switch strings.ToLower(strings.TrimSpace(sev)) {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

// SortFindings sorts findings in place into the canonical report order:
// severity descending, then file ascending, then line ascending, then id. The
// id tiebreaker guarantees a total order so rendering is fully deterministic.
func SortFindings(fs []store.Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		ri, rj := severityRank(fs[i].Severity), severityRank(fs[j].Severity)
		if ri != rj {
			return ri > rj
		}
		if fs[i].File != fs[j].File {
			return fs[i].File < fs[j].File
		}
		if fs[i].Line != fs[j].Line {
			return fs[i].Line < fs[j].Line
		}
		return fs[i].ID < fs[j].ID
	})
}

// tierName maps the integer tier to its human label (see ARCHITECTURE.md).
func tierName(tier int) string {
	switch tier {
	case 0:
		return "T0 Fix-witnessed"
	case 1:
		return "T1 Reproduced"
	case 2:
		return "T2 Verified"
	case 3:
		return "T3 Suspected"
	default:
		return "T? Unknown"
	}
}

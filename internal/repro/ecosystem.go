package repro

// ecosystem.go bridges the repro package to the shared internal/ecosystem
// registry. The actual interpretation data (tables, markers, detect logic)
// lives in internal/ecosystem; this file provides the package-private aliases
// and wrappers that the rest of the repro package uses unchanged.
//
// Callers inside repro (interpret.go, patch.go, verify_sandbox.go) continue to
// use the unexported names (ecosystemRules, ecosystemTable, detectEcosystem,
// hasAnyMarker, etc.) — zero changes to those files.

import (
	"strings"

	eco "github.com/dpoage/bugbot/internal/ecosystem"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// ecosystemRules is the package-private alias for the interpretation data for a
// single ecosystem. Fields are lowercase to preserve internal usage without
// modification. The data is populated by converting ecosystem.InterpRules values
// from the shared registry — see ecosystemTable below.
type ecosystemRules struct {
	name                         sandbox.Ecosystem
	ranMarkers                   []string
	notRanMarkers                []string
	buildMarkers                 []string
	toolchainMarkers             []string
	lineAnchoredToolchainMarkers []string
}

// fromInterpRules converts an ecosystem.InterpRules (exported, from the shared
// registry) to the package-private ecosystemRules used within repro.
func fromInterpRules(r eco.InterpRules) ecosystemRules {
	return ecosystemRules{
		name:                         r.Name,
		ranMarkers:                   r.RanMarkers,
		notRanMarkers:                r.NotRanMarkers,
		buildMarkers:                 r.BuildMarkers,
		toolchainMarkers:             r.ToolchainMarkers,
		lineAnchoredToolchainMarkers: r.LineAnchoredToolchainMarkers,
	}
}

// defaultEnvMarkers delegates to the shared registry.
var defaultEnvMarkers = eco.DefaultEnvMarkers

// bazelEnvMarkers delegates to the shared registry.
var bazelEnvMarkers = eco.BazelEnvMarkers

// sanitizerReportMarkers delegates to the shared registry.
var sanitizerReportMarkers = eco.SanitizerReportMarkers

// runtimeFailureMarkers delegates to the shared registry.
var runtimeFailureMarkers = eco.RuntimeFailureMarkers

// reproSentinelDemonstrated delegates to the shared registry.
const reproSentinelDemonstrated = eco.ReproSentinelDemonstrated

// reproSentinelMarkers delegates to the shared registry.
var reproSentinelMarkers = eco.ReproSentinelMarkers

// ecosystemTable is the ordered registry of supported ecosystems, populated
// from the shared internal/ecosystem.InterpTable. Adding a new ecosystem means
// adding it to internal/ecosystem/interp.go (single source of truth); this
// table is derived automatically.
var ecosystemTable = func() []ecosystemRules {
	src := eco.InterpTable
	out := make([]ecosystemRules, len(src))
	for i, r := range src {
		out[i] = fromInterpRules(r)
	}
	return out
}()

// detectEcosystem picks the first ecosystemRules whose launcher regex matches
// the plan's argv. Delegates to the shared ecosystem.DetectEcosystem for the
// actual argv analysis; converts the result back to the package-private type.
func detectEcosystem(argv []string) ecosystemRules {
	return fromInterpRules(eco.DetectEcosystem(argv))
}

// ecosystemIndex returns the position of the named entry in ecosystemTable,
// or 0 if the name is unknown.
func ecosystemIndex(name sandbox.Ecosystem) int {
	for i, e := range ecosystemTable {
		if e.name == name {
			return i
		}
	}
	return 0
}

// unwrapShell delegates to the shared registry.
func unwrapShell(argv []string) []string {
	return eco.UnwrapShell(argv)
}

// hasRanEvidence reports whether out contains at least one of the ecosystem's
// positive ran-markers.
func (e *ecosystemRules) hasRanEvidence(out string) bool {
	if e == nil {
		return false
	}
	low := strings.ToLower(out)
	for _, m := range e.ranMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// hasNotRanEvidence reports whether out contains any of the ecosystem's
// notRanMarkers.
func (e *ecosystemRules) hasNotRanEvidence(out string) bool {
	if e == nil {
		return false
	}
	low := strings.ToLower(out)
	for _, m := range e.notRanMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// hasAnyMarker delegates to the shared registry.
func hasAnyMarker(out string, markers []string) bool {
	return eco.HasAnyMarker(out, markers)
}

// hasAnyMarkerAtLineStart delegates to the shared registry.
func hasAnyMarkerAtLineStart(out string, markers []string) bool {
	return eco.HasAnyMarkerAtLineStart(out, markers)
}

// hasLineAnchoredToolchainMarker checks both the free toolchainMarkers
// (substring) AND the lineAnchoredToolchainMarkers (line-start anchored).
func (e *ecosystemRules) hasLineAnchoredToolchainMarker(out string) bool {
	if hasAnyMarker(out, e.toolchainMarkers) {
		return true
	}
	return hasAnyMarkerAtLineStart(out, e.lineAnchoredToolchainMarkers)
}

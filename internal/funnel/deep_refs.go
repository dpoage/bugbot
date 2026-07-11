package funnel

import (
	"context"
	"sort"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/treesitter"
)

// deepRef is one precomputed cross-reference site: a load-bearing symbol
// declared in a seed file and a specific location in another file that
// references it. Used to prime deep-strategy finder units so the agent
// confirms a known cross-site set instead of rediscovering it.
type deepRef struct {
	Symbol string
	File   string
	Line   int
}

// deepRefMaxSymbols is the maximum number of public top-level symbols
// selected from the seed files for reference lookup. Eight matches the cap
// the deep-strategy system prompt suggests to the agent.
const deepRefMaxSymbols = 8

// deepRefMaxRefs is the maximum total cross-file reference sites returned.
// Keeps the PRECOMPUTED CROSS-REFERENCES section from dominating the task
// context window.
const deepRefMaxRefs = 24

// deepRefMaxRelatedFiles is the maximum number of distinct referenced files
// added to the deep unit's file set.
const deepRefMaxRelatedFiles = 8

// refClosureNav is the slice of *agent.CodeNav consumed by deepRefClosureWith.
// An interface so unit tests can inject scripted outline+references without a
// real language server. Production uses *agent.CodeNav directly.
type refClosureNav interface {
	Outline(file string) ([]treesitter.OutlineEntry, error)
	References(ctx context.Context, file string, line int, sym string) ([]agent.RefLocation, error)
}

// isLoadBearing reports whether a symbol kind is considered load-bearing for
// the contract-trace and state-trace strategies: functions, methods, types,
// interfaces, and classes carry contracts and state.
func isLoadBearing(k treesitter.Kind) bool {
	switch k {
	case treesitter.KindFunction, treesitter.KindMethod,
		treesitter.KindType, treesitter.KindInterface, treesitter.KindClass:
		return true
	}
	return false
}

// isPublicSymbol reports whether name looks like part of the file's public
// API surface under lang's conventions. Go uses its export rule (upper-case
// ASCII first letter). Every other language has no syntactic export marker
// tree-sitter can surface, so the leading-underscore private-by-convention
// rule applies (Python _helper/__init__, JS/TS _internal, C _static): a
// leading underscore is private, anything else is public. The symbol caps
// (deepRefMaxSymbols/deepRefMaxRefs) bound the looser non-Go filter.
func isPublicSymbol(lang ingest.Language, name string) bool {
	if name == "" {
		return false
	}
	if lang == ingest.LangGo {
		r := name[0]
		return r >= 'A' && r <= 'Z'
	}
	return name[0] != '_'
}

// deepRefClosure precomputes the cross-reference closure of the load-bearing
// public symbols declared in seedFiles. Returns (nil, nil) when the
// code-nav bundle is nil or errors — byte-identical to today's behavior.
func (f *Funnel) deepRefClosure(ctx context.Context, seedFiles []string) (refs []deepRef, relatedFiles []string) {
	nav, err := f.codeNav()
	if err != nil || nav == nil {
		return nil, nil
	}
	return deepRefClosureWith(ctx, nav, seedFiles)
}

// deepRefClosureWith is the core implementation, separated so tests can inject
// a fake refClosureNav without a real language server.
//
// Steps (all NIL-SAFE):
//  1. Calls nav.Outline on each seed file to enumerate top-level declarations.
//  2. Selects up to deepRefMaxSymbols load-bearing public symbols in
//     deterministic order (alphabetical by name, then by file, then by line).
//  3. For each selected symbol, calls nav.References to find cross-file sites
//     (excluding the seed files themselves).
//  4. Caps total refs at deepRefMaxRefs and related files at
//     deepRefMaxRelatedFiles; returns both sorted deterministically.
//
// Any error from Outline or References is silently ignored (best-effort):
// partial results are returned; an all-error run returns (nil, nil).
func deepRefClosureWith(ctx context.Context, nav refClosureNav, seedFiles []string) (refs []deepRef, relatedFiles []string) {
	seedSet := make(map[string]bool, len(seedFiles))
	for _, sf := range seedFiles {
		seedSet[sf] = true
	}

	// Candidate symbols: (name, file, startLine) triples.
	type candidate struct {
		sym       string
		file      string
		startLine int
	}
	var candidates []candidate

	for _, sf := range seedFiles {
		entries, err := nav.Outline(sf)
		if err != nil || len(entries) == 0 {
			continue
		}
		lang := ingest.DetectLanguage(sf)
		for _, e := range entries {
			if !isPublicSymbol(lang, e.Name) || !isLoadBearing(e.Kind) {
				continue
			}
			candidates = append(candidates, candidate{
				sym:       e.Name,
				file:      sf,
				startLine: e.StartLine,
			})
		}
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	// Deterministic selection: alphabetical by name, then file, then line.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].sym != candidates[j].sym {
			return candidates[i].sym < candidates[j].sym
		}
		if candidates[i].file != candidates[j].file {
			return candidates[i].file < candidates[j].file
		}
		return candidates[i].startLine < candidates[j].startLine
	})
	if len(candidates) > deepRefMaxSymbols {
		candidates = candidates[:deepRefMaxSymbols]
	}

	// Collect cross-file reference sites, deduplicated.
	type refKey struct {
		file string
		line int
	}
	seen := make(map[refKey]bool)
	relatedSet := make(map[string]bool)
	var collected []deepRef

	for _, c := range candidates {
		locs, err := nav.References(ctx, c.file, c.startLine, c.sym)
		if err != nil || len(locs) == 0 {
			continue
		}
		for _, loc := range locs {
			if seedSet[loc.File] {
				continue // exclude the seed files themselves
			}
			k := refKey{loc.File, loc.Line}
			if seen[k] {
				continue
			}
			seen[k] = true
			collected = append(collected, deepRef{Symbol: c.sym, File: loc.File, Line: loc.Line})
			relatedSet[loc.File] = true
			if len(collected) >= deepRefMaxRefs {
				goto capReached
			}
		}
	}
capReached:

	if len(collected) == 0 {
		return nil, nil
	}

	// Sort refs deterministically: file, then line, then symbol.
	sort.SliceStable(collected, func(i, j int) bool {
		if collected[i].File != collected[j].File {
			return collected[i].File < collected[j].File
		}
		if collected[i].Line != collected[j].Line {
			return collected[i].Line < collected[j].Line
		}
		return collected[i].Symbol < collected[j].Symbol
	})

	// Sort and cap related files.
	related := make([]string, 0, len(relatedSet))
	for rf := range relatedSet {
		related = append(related, rf)
	}
	sort.Strings(related)
	if len(related) > deepRefMaxRelatedFiles {
		related = related[:deepRefMaxRelatedFiles]
	}

	return collected, related
}

// dedupFiles returns base with any new files from extra appended in the order
// extra provides them (already sorted by the caller). Files already in base
// are not re-added. The returned slice may share the backing array of base
// when no extras are added.
func dedupFiles(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	inBase := make(map[string]bool, len(base))
	for _, f := range base {
		inBase[f] = true
	}
	result := make([]string, len(base), len(base)+len(extra))
	copy(result, base)
	for _, f := range extra {
		if !inBase[f] {
			result = append(result, f)
		}
	}
	return result
}

package ingest

import "sort"

// displayName maps a Language to its human-readable rendering for persona
// strings (e.g. "Go", "JavaScript/TypeScript"). Languages absent from this map
// (LangOther) have no meaningful persona rendering and are excluded upstream.
var displayName = map[Language]string{
	LangGo:         "Go",
	LangPython:     "Python",
	LangJavaScript: "JavaScript",
	LangTypeScript: "TypeScript",
	LangRust:       "Rust",
	LangJava:       "Java",
	LangC:          "C",
	LangCPP:        "C++",
	LangRuby:       "Ruby",
	LangCSharp:     "C#",
	LangPHP:        "PHP",
	LangSwift:      "Swift",
	LangKotlin:     "Kotlin",
	LangShell:      "Shell",
}

// maxProfileLanguages caps how many languages a persona names. Beyond a couple,
// listing more dilutes the persona rather than sharpening it.
const maxProfileLanguages = 3

// DominantLanguages returns the languages that cover the bulk of a snapshot's
// files, ordered most-common first, capped at maxProfileLanguages. It counts
// files (not bytes): a snapshot's intent is "what kind of repo is this", and a
// file count is robust to a handful of large generated or vendored files
// skewing a byte-weighted measure. LangOther and LangShell are excluded as
// noise — LangOther is unclassified and LangShell is near-ubiquitous tooling
// that rarely defines a repo's character.
//
// Ties (equal file counts) break by the Language string for deterministic,
// stable output across runs. The result is empty when no qualifying language is
// present (e.g. an empty snapshot or one of only LangOther/LangShell files).
func DominantLanguages(snap *Snapshot) []Language {
	if snap == nil {
		return nil
	}

	counts := make(map[Language]int)
	for _, f := range snap.Files {
		if f.Language == LangOther || f.Language == LangShell {
			continue
		}
		counts[f.Language]++
	}
	if len(counts) == 0 {
		return nil
	}

	langs := make([]Language, 0, len(counts))
	for l := range counts {
		langs = append(langs, l)
	}
	// Sort by descending count, then ascending Language string for a stable
	// tie-break so the same snapshot always yields the same ordering.
	sort.Slice(langs, func(i, j int) bool {
		if counts[langs[i]] != counts[langs[j]] {
			return counts[langs[i]] > counts[langs[j]]
		}
		return langs[i] < langs[j]
	})

	if len(langs) > maxProfileLanguages {
		langs = langs[:maxProfileLanguages]
	}
	return langs
}

// PersonaLanguages renders a language list as the expertise clause of an
// engineer persona. The result is the bare phrase, with no leading/trailing
// punctuation, so callers can splice it into a sentence:
//
//	one language  -> "senior Go engineer"
//	two+ languages -> "senior software engineer with deep Go and Python expertise"
//	empty/unknown  -> "senior software engineer"
//
// The output is deterministic given a deterministic langs slice (see
// DominantLanguages). Languages without a display name are skipped.
func PersonaLanguages(langs []Language) string {
	names := make([]string, 0, len(langs))
	for _, l := range langs {
		if n, ok := displayName[l]; ok {
			names = append(names, n)
		}
	}

	switch len(names) {
	case 0:
		return "senior software engineer"
	case 1:
		return "senior " + names[0] + " engineer"
	default:
		return "senior software engineer with deep " + joinAnd(names) + " expertise"
	}
}

// Persona derives the finder/verifier engineer persona phrase from a snapshot's
// dominant language mix. It is the one-call helper the funnel uses; see
// DominantLanguages and PersonaLanguages for the underlying steps.
func Persona(snap *Snapshot) string {
	return PersonaLanguages(DominantLanguages(snap))
}

// joinAnd joins names with commas and a final "and": "Go", "Go and Python",
// "Go, Python and Rust". It assumes len(names) >= 1.
func joinAnd(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		joined := ""
		for i := 0; i < len(names)-1; i++ {
			if i > 0 {
				joined += ", "
			}
			joined += names[i]
		}
		return joined + " and " + names[len(names)-1]
	}
}

package ingest

import (
	"sort"
)

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
// qualifiers optionally maps a Language to a dialect string that replaces the
// base display name for that language. For example, qualifiers[LangCPP]="C++20"
// causes "C++" to render as "C++20" in the output. Passing nil qualifiers
// produces the base behaviour. This mechanism is general: it can later serve
// Python 2/3, Java 8/21, etc. without changes to the Language vocabulary.
//
// The output is deterministic given a deterministic langs slice (see
// DominantLanguages) and a fixed qualifiers map.
func PersonaLanguages(langs []Language, qualifiers map[Language]string) string {
	names := make([]string, 0, len(langs))
	for _, l := range langs {
		// A qualifier overrides the base display name when present, allowing
		// "C++" to become "C++20" without adding Language enum variants.
		if q, ok := qualifiers[l]; ok {
			names = append(names, q)
			continue
		}
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
//
// When snap.Root is non-empty and the snapshot's dominant languages include
// C++, Persona probes the repository for its C++ standard (via
// DetectCPPStandard) and qualifies the persona accordingly — e.g. "senior
// C++20 engineer" instead of "senior C++ engineer". The qualifier applies only
// to LangCPP for now; the underlying map[Language]string mechanism is general
// and can be extended to other languages (Python 2/3, Java 8/21, etc.) without
// touching this function.
func Persona(snap *Snapshot) string {
	langs := DominantLanguages(snap)

	// Build the qualifier map only when the repo root is known and C++ is among
	// the dominant languages — DetectCPPStandard is filesystem I/O, so we avoid
	// calling it on snapshots that have no C++ or no known root.
	var qualifiers map[Language]string
	if snap != nil && snap.Root != "" && containsLang(langs, LangCPP) {
		if std, ok := DetectCPPStandard(snap.Root); ok {
			qualifiers = map[Language]string{LangCPP: std}
		}
	}

	return PersonaLanguages(langs, qualifiers)
}

// containsLang reports whether langs contains l.
func containsLang(langs []Language, l Language) bool {
	for _, lang := range langs {
		if lang == l {
			return true
		}
	}
	return false
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

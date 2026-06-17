package funnel

import "strings"

// isExecutableClaim reports whether the candidate's claim describes a
// deterministic, pure function's input->output behavior — i.e. a claim that
// can be falsified empirically by EXECUTING the function with a constructed
// input, rather than by re-reading the code or reasoning about concurrency,
// I/O, or environment.
//
// The heuristic is precision-first and intentionally tight:
//   - false NEGATIVES (a claim that could be falsified is misclassified as
//     non-executable) are acceptable: the candidate just takes the normal
//     verify path with no sandbox and no executor seat — same as today.
//   - false POSITIVES (a claim we wrongly classify as executable) merely
//     OFFER the refuter a sandbox_exec tool and assign one panel seat an
//     executor-style clause. The refuter is free to ignore them; the panel
//     verdict logic is unchanged.
//
// The classifier is keyword-based on the lowercased title + description.
// A claim is executable when it mentions at least one of the deterministic
// markers AND none of the environmental markers. The marker sets are
// curated to (a) match the parseSARIF-cap claim from the false-positive
// incident (bugbot-aud, GH #64) and (b) NOT match concurrency / I/O /
// environment claims like "data race on shared map in goroutine".
//
// If this heuristic ever needs to be expanded, prefer adding markers to the
// set rather than introducing a second classifier — false positives are
// cheap, but splitting classification across two functions makes the
// "what counts as executable?" answer a search.
func isExecutableClaim(c Candidate) bool {
	text := strings.ToLower(c.Title + " " + c.Description)

	// Non-deterministic / environmental / I/O markers. Presence of any
	// of these indicates the claim cannot be settled by executing the
	// function alone — a sandbox run would be unreliable or misleading.
	for _, m := range envClaimMarkers {
		if strings.Contains(text, m) {
			return false
		}
	}

	// Deterministic / pure-function markers. The claim is about a
	// concrete input->output behavior of a function's logic: parsing,
	// capping, comparison, ordering, indexing, encoding, regex matching,
	// and so on. These are the shapes a falsification test can pin down.
	for _, m := range detClaimMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// envClaimMarkers are the non-deterministic / I/O / environment substrings
// whose presence on a claim rules it out as "executable" — i.e. empirical
// execution of the function in a sandbox is not the right way to falsify
// it. Each entry is a literal substring; some are intentionally prefixed
// with a space so they only match at word boundaries (e.g. " race" matches
// "data race" but not "parse").
var envClaimMarkers = []string{
	"goroutine",
	" race", // data race, race condition
	"deadlock",
	"concurren", // concurrent, concurrency
	"network",
	"http",
	"socket",
	"filesystem",
	" disk", // disk I/O
	"database",
	"timeout",
	" clock",  // wall-clock
	" random", // randomness, randomly
	"env var",
	"environment variable",
}

// detClaimMarkers are the deterministic / pure-function substrings whose
// presence on a claim marks it as a candidate for empirical falsification.
// Each entry is a literal substring; some are intentionally prefixed with a
// space so they only match at word boundaries (e.g. " sort" matches "stable
// sort" but not "resort" — though resort in the claim is unlikely).
var detClaimMarkers = []string{
	"off-by-one",
	"cap", // cap, capped, capping, capacity
	"bypass",
	"boundary",
	"overflow",
	"underflow",
	"truncat", // truncate, truncation
	" round",  // rounding
	"parse",   // parse, parser, parseSARIF
	"encode",
	"decode",
	"precedence",
	"computes",
	"miscalculat", // miscalculate, miscalculation
	"index out of",
	" loop", // loop, looping
	" sort", // sort, sorting
	"comparison",
	"regex",
}

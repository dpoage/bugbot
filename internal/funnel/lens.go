package funnel

import (
	"sort"

	"github.com/dpoage/bugbot/internal/ingest"
)

// Lens is a single hypothesis specialization for the finder stage. Each lens
// narrows the finder agent to one class of defect, with a system-prompt
// specialization that focuses attention without changing the strict
// precision-first reporting contract.
//
// Lenses exist because a single broad "find all the bugs" prompt produces
// diffuse, low-precision output: the model spreads its attention and reports
// shallow style nits. Splitting the search into focused, independently-run
// lenses keeps each finder agent's reasoning concentrated on one failure mode,
// which both raises hit-rate within that mode and makes the candidates easier
// to triage and verify.
//
// A lens is split into a universal Core (here) and per-language manifestation
// rows (the manifestations table in lens_manifestations.go): the Core states
// the failure mode in language-free terms, and prompt composition appends a
// "How this manifests in <Language>" block for each language in the chunk that
// has manifestation rows. A lens with no manifestation rows at all composes
// Core alone — that is a first-class shape, not an error (some lenses, e.g. a
// commit-intent lens, are inherently language-free).
type Lens struct {
	// Name is the stable lens identifier. It is recorded on every candidate and
	// finding (Finding.Lens) and is part of the dedup fingerprint, so it must be
	// stable across runs. It also keys the manifestations and lensYields tables.
	Name string
	// Core is the universal, language-free statement of this lens's failure
	// mode, appended to the shared finder system prompt to focus the agent. It
	// must NOT relax the precision rules in the shared prompt (it only narrows
	// what to look for), and it must not name language-specific constructs —
	// those belong in the manifestations table so non-matching repos never see
	// them.
	Core string
}

// BuiltinLenses returns the default lens set, ordered by descending Go-column
// yield (the historical default priority). The per-run launch/degradation
// order is NOT this order: it is recomputed from the repo's dominant languages
// via lensesByYield, because a lens's expected yield is language-dependent
// (see lensYields).
//
// Each Core is deliberately concrete: it names the exact failure shape to hunt
// for so the model investigates real code paths rather than speculating. None
// of them loosen the "concrete bugs only, empty list is fine" contract from
// the shared finder prompt.
func BuiltinLenses() []Lens {
	return []Lens{
		{
			Name: "nil-safety/error-handling",
			Core: "Hunt for null/absent-value dereferences and mishandled failures: " +
				"using a value that a reachable code path can leave null, missing, or " +
				"invalid; using an operation's result WITHOUT first checking the error " +
				"or failure signal returned alongside it; swallowed or ignored failures " +
				"whose bogus result is then used as if the operation succeeded; and " +
				"operations that report success while producing an empty or invalid " +
				"value the caller will use. Confirm the bad value or missed failure can " +
				"actually reach the use site on a reachable path.",
		},
		{
			// diff-intent is the unique-advantage lens on commit scans, where the
			// commit message and diff are both available and the change is fresh.
			// It fires ONLY on commit-triggered runs (the funnel emits zero
			// diff-intent tasks on sweeps or when ChangeContext is nil), so it
			// never competes with the taxonomy lenses on sweep runs. It is
			// language-free: Core-only, no manifestation rows; its yield lives in
			// lensYields under anyLanguage (95 — above concurrency's Go column,
			// below nil-safety's).
			Name: "diff-intent",
			Core: "Hunt for intent-vs-implementation mismatches in a specific commit: " +
				"the change's implementation contradicts its stated intent (the diff does " +
				"something the commit message says it does not, or omits something it " +
				"claims to do); and existing callers whose assumptions the change silently " +
				"breaks (a function's contract, precondition, or return invariant shifts " +
				"in the diff, but call sites checked via find_references still rely on the " +
				"old behavior). Confirm every finding by reading the diff AND the call " +
				"sites with find_references — do not report a mismatch you have not " +
				"verified in the actual code. Finding nothing is a valid outcome.",
		},
		{
			Name: "concurrency",
			Core: "Hunt for concurrency defects: shared state read and written from " +
				"concurrently executing code without synchronization; a lock that is " +
				"acquired but not released on every path out of the critical section; " +
				"deadlocks from inconsistent lock ordering or from waiting on an event " +
				"no other task will ever deliver; and lifecycle races where concurrent " +
				"work outlives or interleaves with the state it touches. Confirm the " +
				"concurrent access is real by reading the sites where the concurrent " +
				"work is launched or scheduled.",
		},
		{
			Name: "resource-leaks",
			Core: "Hunt for leaked resources: acquired resources must be released on " +
				"every path, including early-return and error paths. Look for " +
				"acquisitions (files, sockets, connections, handles, timers, background " +
				"work, memory) whose release is missing or skipped on some reachable " +
				"path, and cleanup that is registered in a way that never runs or " +
				"accumulates instead of running promptly. Read the function fully to " +
				"confirm no later release exists before reporting.",
		},
		{
			Name: "boundary-conditions",
			Core: "Hunt for boundary and bounds defects: off-by-one errors in index or " +
				"length arithmetic; indexing or slicing with an attacker- or " +
				"caller-controlled value without a bounds check; assuming a collection " +
				"or string is non-empty before reading its first or last element; " +
				"integer overflow/underflow or truncation in size and index arithmetic; " +
				"and incorrect handling of the empty-input case. Confirm the index can " +
				"actually reach the out-of-range value on a reachable path.",
		},
		{
			Name: "api-contract-misuse",
			Core: "Hunt for misuse of an API's documented contract: calling a function " +
				"with arguments it forbids; ignoring a documented precondition or " +
				"required ordering (e.g. must initialize before use, must not reuse " +
				"after close); and usage the compiler or runtime accepts but the " +
				"documentation forbids. Read the callee's documentation or definition " +
				"to confirm the contract before reporting.",
		},
		{
			Name: "injection/input-validation",
			Core: "Hunt for injection and missing input validation: building a query, " +
				"command, file path, or markup/template output by concatenating " +
				"untrusted input instead of parameterizing or escaping it; path " +
				"traversal from unsanitized user paths; unbounded reads or allocations " +
				"sized by untrusted input; and trusting external input (headers, " +
				"request bodies, env, CLI args) without validating it before use in a " +
				"sensitive sink. Trace the input from its source to the sink to confirm " +
				"it is actually untrusted and unvalidated.",
		},
		{
			// cross-language-boundary is the cross-language differentiator: in a
			// polyglot repo the densest bug habitat is the seam BETWEEN
			// languages (a producer in one language, a consumer in another).
			// Single-language tools are blind there because they only see one
			// side of the contract. The lens is Core-only/language-free (no
			// manifestation rows): the failure mode is the contract gap, which
			// is language-free by definition — what matters is that two
			// different languages touch the same surface. Like diff-intent, it
			// is a custom-unit lens: hypothesize skips it in the per-chunk loop
			// (see buildUnits) and emits exactly one custom task per cross-
			// language seam discovered by EnumerateSeams (see buildSeamTask).
			// Yield lives in lensYields under anyLanguage only, since the lens
			// applies to every language mix (the precondition is "the repo is
			// polyglot", which the seam discovery enforces upstream).
			Name: "cross-language-boundary",
			Core: "Hunt for producer/consumer contract mismatches across a language " +
				"boundary where one side WRITES a shared data format or env var and " +
				"another side READS it: a field that one side emits and the other " +
				"side does not expect (or expects under a different name); a type, " +
				"unit, encoding, or nullability disagreement (string vs int, " +
				"seconds vs milliseconds, base64 vs raw, empty-string vs null); a " +
				"value one side can emit that the other side cannot parse; and a " +
				"required-vs-optional disagreement (a producer that sometimes omits " +
				"a key the consumer requires, or vice versa). Confirm every finding " +
				"by reading BOTH named sides end-to-end — do not report a mismatch " +
				"you have not verified in the actual code on each side. Finding " +
				"nothing is a valid outcome: many seams are perfectly aligned.",
		},
	}
}

// selectLenses resolves the effective lens set for a run: if override is
// non-empty it filters the builtins to the named lenses (preserving builtin
// order), otherwise it returns all builtins. Unknown names in override are
// ignored; if nothing matches, the full builtin set is returned so a typo never
// silently disables the whole finder stage.
func selectLenses(override []string) []Lens {
	all := BuiltinLenses()
	if len(override) == 0 {
		return all
	}
	want := make(map[string]bool, len(override))
	for _, n := range override {
		want[n] = true
	}
	var out []Lens
	for _, l := range all {
		if want[l.Name] {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return all
	}
	return out
}

// unlistedLensYield is the yield assumed for a lens with no row in lensYields
// at all (or a row missing its anyLanguage default). A mid-table value keeps
// an unregistered lens running under mild budget pressure without letting it
// displace the known high-yield lenses.
const unlistedLensYield = 50

// effectiveYield resolves the budget-degradation yield for the named lens on a
// repo whose dominant languages are langs: the MAX over the per-language
// columns in lensYields, with the anyLanguage column standing in for any
// language that has no explicit column. Max (not mean) because a lens that is
// high-yield for ANY substantial language in the repo is worth keeping — a
// mixed Go/Python repo still has Go concurrency bugs. Empty langs (no
// detectable dominant language) resolves to the anyLanguage column.
func effectiveYield(name string, langs []ingest.Language) int {
	cols, ok := lensYields[name]
	if !ok {
		return unlistedLensYield
	}
	def, ok := cols[anyLanguage]
	if !ok {
		def = unlistedLensYield
	}
	if len(langs) == 0 {
		return def
	}
	best := 0
	for _, lang := range langs {
		y, ok := cols[lang]
		if !ok {
			y = def
		}
		if y > best {
			best = y
		}
	}
	return best
}

// lensesByYield returns lenses reordered by descending effective yield for
// langs (see effectiveYield). The sort is stable, so equal-yield lenses keep
// their relative builtin order — the per-run order is deterministic for a
// given language mix. This order is both the launch order (so budget flows to
// high-yield lenses first) and the degradation order (degradedLensNames keeps
// its head).
func lensesByYield(lenses []Lens, langs []ingest.Language) []Lens {
	out := make([]Lens, len(lenses))
	copy(out, lenses)
	sort.SliceStable(out, func(i, j int) bool {
		return effectiveYield(out[i].Name, langs) > effectiveYield(out[j].Name, langs)
	})
	return out
}

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
	// Languages gates which chunk language sets the lens emits units for: nil
	// or empty means the lens applies to ALL languages (preserves every
	// language-free builtin byte-for-byte), and a non-empty set means the lens
	// emits units ONLY for chunks whose language set intersects it (see
	// lensAppliesTo). This is the mechanism that lets memory-safety,
	// exception-safety, and dynamic-typing be present in BuiltinLenses without
	// running on Go repos where their failure modes do not apply.
	Languages []ingest.Language
}

// lensAppliesTo reports whether the lens should emit units for a chunk whose
// language set is langs. A lens with no Languages entry (nil/empty) applies
// to every language — that is the default and the byte-identical behavior for
// every language-free lens in BuiltinLenses. A lens with a non-empty
// Languages list applies only if at least one of its declared languages
// appears in the chunk's language set, so e.g. memory-safety (CPP/C/Rust)
// emits nothing on a pure Go chunk and dynamic-typing does not run on a
// Go-only repo.
func lensAppliesTo(l Lens, langs []ingest.Language) bool {
	if len(l.Languages) == 0 {
		return true
	}
	for _, want := range l.Languages {
		for _, have := range langs {
			if want == have {
				return true
			}
		}
	}
	return false
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
			// Memory-safety is a defect class Go's runtime and borrow checker
			// (had it one) would eliminate: use-after-free, double-free, buffer
			// overflow, uninitialized reads, dangling iterators/references/
			// pointers. Go's GC owns freeing and bounds-checks slice access, so
			// this lens would produce nothing but noise on a Go repo and is
			// gated to C, C++, and Rust (where the borrow checker catches
			// borrow-rule violations but NOT raw-pointer or unsafe-block
			// lifetimes).
			Name: "memory-safety",
			Core: "Hunt for memory-safety defects: a use-after-free or use-after-" +
				"realloc (reading through a pointer the allocator or owner has " +
				"already freed or moved); a double-free or mismatched free (delete " +
				"vs delete[], free on a pointer that was not malloc'd, a smart " +
				"pointer's destructor racing with a manual release); a buffer " +
				"overflow or out-of-bounds write through pointer arithmetic or an " +
				"unchecked index; reading from a buffer or scalar that a reachable " +
				"path leaves uninitialized; and dangling iterators, references, or " +
				"pointers held past the end of the owning container's lifetime " +
				"(iterators into a vector across a push_back, string_view kept past " +
				"the string's modification, raw pointers outliving the object). " +
				"Confirm the bad access is reachable — read the allocation, the " +
				"free/move/resize, and the use site. Finding nothing is a valid " +
				"outcome.",
			Languages: []ingest.Language{ingest.LangCPP, ingest.LangC, ingest.LangRust},
		},
		{
			// Exception-safety is the family of bugs where a thrown exception
			// (or an error that Python/JS handle like one) skips cleanup,
			// leaves an object half-mutated, or gets swallowed by an over-broad
			// catch. Go's `error` return channel sidesteps the throwing control
			// flow, so this lens is gated to languages with throwing control
			// flow: Python (raise), C++ (throw), JavaScript/TypeScript (throw).
			Name: "exception-safety",
			Core: "Hunt for exception-safety defects: cleanup (locks, transactions, " +
				"file/network handles, partially-constructed objects, file " +
				"renames, temporary directories) skipped because an exception " +
				"thrown mid-function bypasses the cleanup section; an object left " +
				"half-mutated across a throw so the caller sees invariants broken " +
				"but no error (Python generators paused mid-update, C++ objects " +
				"with one-of-N members set before the throwing operation, JS " +
				"objects with one field updated and a sibling not); a catch / " +
				"except that swallows the failure (bare except, except Exception: " +
				"pass, catch (e) {}) and lets the caller proceed with a missing or " +
				"stale result; and a new exception thrown FROM a cleanup block " +
				"(a __exit__ that raises, a C++ destructor that throws, a finally " +
				"that throws) masking the original error. Read the function and " +
				"every throw / raise site to confirm the cleanup is actually " +
				"skipped. Finding nothing is a valid outcome.",
			Languages: []ingest.Language{ingest.LangPython, ingest.LangCPP, ingest.LangJavaScript, ingest.LangTypeScript},
		},
		{
			// Dynamic-typing is the family of bugs the type system would have
			// caught at compile time if it had been told: None/undefined
			// propagating far from its origin, implicit coercion (== vs ===,
			// str/bytes, numeric/string), attribute/key errors from duck-typing
			// on a value whose actual type disagrees with the call site, and
			// wrong-type arguments that fail only at the runtime call boundary.
			// Go's static type system rules this class out almost entirely, so
			// the lens is gated to Python, JavaScript, and TypeScript (where the
			// TS type system is erased at runtime and `any` / untyped JSX / union
			// narrowing holes reintroduce the same failures at the JS boundary).
			Name: "dynamic-typing",
			Core: "Hunt for dynamic-typing defects: None / undefined / null " +
				"propagating far from its origin (a default-dict.get, an " +
				"optional-chaining yield, a JSON.parse on a missing field) into " +
				"arithmetic, indexing, or a template that then breaks downstream; " +
				"implicit coercion bugs — == vs === (null == undefined, '' == 0), " +
				"str vs bytes concatenation that raises, numeric / string " +
				"concatenation that silently produces the wrong type, truthy " +
				"checks on values that conflate None with 0 / '' / []; attribute " +
				"or key errors from duck-typing on a value whose actual type " +
				"disagrees with the call site (e.g. expecting a dict and getting " +
				"a list, expecting str and getting bytes, accessing a key on a " +
				"None or array); and wrong-type arguments to a function that the " +
				"runtime only rejects at the call boundary, leaving the caller " +
				"with a TypeError / AttributeError far from the call site. Trace " +
				"the value from its origin to the use. Finding nothing is a valid " +
				"outcome.",
			Languages: []ingest.Language{ingest.LangPython, ingest.LangJavaScript, ingest.LangTypeScript},
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

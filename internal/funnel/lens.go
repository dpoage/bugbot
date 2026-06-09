package funnel

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
type Lens struct {
	// Name is the stable lens identifier. It is recorded on every candidate and
	// finding (Finding.Lens) and is part of the dedup fingerprint, so it must be
	// stable across runs.
	Name string
	// Specialization is appended to the shared finder system prompt to focus the
	// agent on this lens's defect class. It must NOT relax the precision rules in
	// the shared prompt; it only narrows what to look for.
	Specialization string
	// Yield ranks lenses for budget degradation: when the run is over its
	// soft-budget threshold, only the highest-yield lenses keep running. Higher
	// is kept longer. These rankings encode the empirical reality that
	// nil/concurrency/resource bugs are both more common and more severe than the
	// others in typical Go code.
	Yield int
}

// BuiltinLenses returns the default lens set, ordered by descending yield. The
// order is also the default execution priority for budget degradation.
//
// Each Specialization is deliberately concrete: it names the exact patterns to
// hunt for so the model investigates real code paths rather than speculating.
// None of them loosen the "concrete bugs only, empty list is fine" contract
// from the shared finder prompt.
func BuiltinLenses() []Lens {
	return []Lens{
		{
			Name:  "nil-safety/error-handling",
			Yield: 100,
			Specialization: "Hunt for nil-pointer dereferences and mishandled errors: " +
				"dereferencing a pointer, map, slice, channel, or interface that a " +
				"reachable code path can leave nil; using a value returned alongside an " +
				"error WITHOUT checking that error first; ignored errors that hide a " +
				"failed operation whose result is then used; type assertions without the " +
				"comma-ok form on a value that may not hold that type; and returning a " +
				"nil error while also returning a zero/invalid value the caller will use.",
		},
		{
			Name:  "concurrency",
			Yield: 90,
			Specialization: "Hunt for concurrency defects: data races on shared state " +
				"accessed from multiple goroutines without synchronization; a mutex that " +
				"is taken and not released on every return path; deadlocks from lock " +
				"ordering or from sending/receiving on a channel no one services; " +
				"closing or writing to a channel from multiple goroutines; loop-variable " +
				"capture in goroutines; and WaitGroup Add/Done imbalances. Confirm the " +
				"concurrent access is real by reading the goroutine launch sites.",
		},
		{
			Name:  "resource-leaks",
			Yield: 80,
			Specialization: "Hunt for leaked resources: an opened file, network " +
				"connection, HTTP response body, database rows/statement, ticker, or " +
				"context-cancel func that is not closed/stopped on every return path " +
				"(especially early-return error paths); goroutines that can never exit " +
				"because their stop signal is unreachable; and defers placed inside loops " +
				"that accumulate until the function returns. Read the function fully to " +
				"confirm no later Close exists.",
		},
		{
			Name:  "boundary-conditions",
			Yield: 60,
			Specialization: "Hunt for boundary and bounds defects: off-by-one in slice " +
				"or array indexing; indexing or slicing with an attacker- or " +
				"caller-controlled length without a bounds check (panic on out-of-range); " +
				"assuming a slice/map/string is non-empty before indexing [0] or [len-1]; " +
				"integer overflow/underflow in size or index arithmetic; and incorrect " +
				"handling of the empty-input case. Confirm the index can actually reach " +
				"the out-of-range value on a reachable path.",
		},
		{
			Name:  "api-contract-misuse",
			Yield: 50,
			Specialization: "Hunt for misuse of an API's documented contract: calling a " +
				"function with arguments it forbids; ignoring a documented precondition " +
				"or required ordering (e.g. must call Init before Use, must not reuse " +
				"after Close); misusing the standard library (e.g. time.After in a hot " +
				"loop, sql.Rows not iterated to completion, json/encoding round-trip " +
				"assumptions); and passing a value where the API requires a pointer or " +
				"vice versa in a way the compiler allows but the contract forbids.",
		},
		{
			Name:  "injection/input-validation",
			Yield: 40,
			Specialization: "Hunt for injection and missing input validation: building a " +
				"SQL query, shell command, file path, or HTML/template output by " +
				"concatenating untrusted input instead of parameterizing/escaping it; " +
				"path traversal from unsanitized user paths; unbounded reads/allocations " +
				"sized by untrusted input; and trusting external input (headers, request " +
				"bodies, env, CLI args) without validating it before use in a sensitive " +
				"sink. Trace the input from its source to the sink to confirm it is " +
				"actually untrusted and unvalidated.",
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

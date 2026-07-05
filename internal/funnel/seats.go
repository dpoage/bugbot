package funnel

// refuterSeat is one panel seat's refutation specialty. Diverse seats catch
// failure modes identical refuters cannot: each attacks the report from a
// different direction, which is what makes a split verdict informative.
type refuterSeat struct {
	name   string
	clause string
}

// builtinSeats are the three built-in refuter specialties, assigned round-robin
// to panel positions. A diverse panel is more informative than N copies of the
// same generalist: each seat attacks the report from a distinct angle, so a
// split verdict (one seat refuted, another could not) signals genuine ambiguity
// rather than random noise.
var builtinSeats = []refuterSeat{
	{
		name: "reachability",
		clause: "YOUR REFUTATION SPECIALTY (reachability): attack the claimed code path's reachability." +
			" Hunt for guards that return first, conditions that can never hold, callers that never pass the bad value," +
			" dead code, build tags or platform guards that exclude the path." +
			" The strongest refutation you can produce is a concrete demonstration that the defective line" +
			" cannot execute with the claimed bad state.",
	},
	{
		name: "semantics",
		clause: "YOUR REFUTATION SPECIALTY (semantics): attack the claimed behavior itself." +
			" Verify the reporter read the language and library semantics correctly: operator precedence," +
			" integer conversion and overflow rules, slice/pointer aliasing, what the standard library" +
			" actually returns and when." +
			" The strongest refutation you can produce is a demonstration that the code's actual behavior" +
			" is correct and the report misread it.",
	},
	{
		name: "guards",
		clause: "YOUR REFUTATION SPECIALTY (guards): attack the report's assumption that nothing else prevents the bug." +
			" Hunt for existing protection at OTHER sites: caller-side validation, type invariants," +
			" constructor checks, prior normalization, locks already held, deferred cleanup." +
			" Use find_references to enumerate real call sites." +
			" The strongest refutation you can produce is an existing guard that already makes the claimed state impossible.",
	},
}

// executorSeat is the panel seat assigned to position 0 when the candidate's
// claim is classified as executable AND a sandbox is available. Its job is
// to break the false-positive trap of three refuters all re-reading the
// same code: instead of static analysis, the executor WRITES a minimal
// falsification test (via sandbox_exec's `files` argument) and RUNS it
// against the exact claimed scenario. The empirical result — clean exit
// on a path that should have triggered the bug, or a non-zero exit that
// actually confirms the defect — decides. This seat is intentionally
// separate from builtinSeats: it is only ever assigned via
// seatForCandidate, never via seatForIndex's round-robin, so the
// reachability/semantics/guards trio is preserved for non-executable
// claims.
var executorSeat = refuterSeat{
	name: "executor",
	clause: "YOUR REFUTATION SPECIALTY (executor): when the claim is about a deterministic / pure" +
		" function's input->output behavior, do NOT rely on re-reading the code — the same three" +
		" model instances re-reading the same source are correlated, not independent, and a false" +
		" positive survives exactly because every refuter reads the bug into the code. Instead," +
		" WRITE a minimal falsification test or program (via the sandbox_exec `files` argument)" +
		" that exercises the EXACT claimed scenario, then RUN it and let the empirical result" +
		" decide." +
		" Concretely: construct the input the report describes (the boundary case, the over-cap" +
		" input, the malformed payload, the off-by-one index, the precedence corner), invoke the" +
		" function on it, and assert the actual return value. A clean exit (exit_code=0) on a" +
		" path that SHOULD trigger the bug is the strongest possible refutation — the code" +
		" behaves correctly where the report claims it fails. A non-zero exit / wrong result /" +
		" panic output that confirms the defect means DO NOT refute — the run just validated" +
		" the report." +
		" Prefer running an EXISTING test or guard over hand-rolling one when one exists; reach" +
		" for hand-rolled probes only when no test covers the claimed scenario. If your first" +
		" run is inconclusive, tighten the input (more runs, harder boundary) and try again —" +
		" but stop after the per-candidate exec budget and decide on what you have." +
		" FALLBACK: if the claimed scenario is nondeterministic or IO-bound (timing-dependent," +
		" network-dependent, file-system-dependent) such that a deterministic probe is infeasible," +
		" fall back to code reading — trace the actual code path and call sites to evaluate the" +
		" claim statically, just as the other seats would.",
}

// seatForCandidate returns the seat assigned to panel position i (0-based)
// for candidate c. It is the production seat picker used by runRefuters.
// Behavior:
//   - n <= 1: returns the zero seat (no clause). Preserves the byte-identical
//     n=1 prompt tested by TestRunRefuters_N1_NoSeatClause.
//   - n >= 2 AND hasSandbox AND isExecutableClaim(c) AND i == 0: returns
//     executorSeat. One executor per panel is the maximum useful: more
//     than one refuter writing and running the same falsification test
//     is wasted compute and reduces seat diversity.
//   - otherwise: returns seatForIndex(i, n), the round-robin reachability /
//     semantics / guards seat.
//
// The executor condition is gated on hasSandbox because the executor's
// instruction is meaningless without sandbox_exec; without it, the seat
// would have to fall back to static re-reading, the very thing it exists
// to avoid.
func seatForCandidate(i, n int, c Candidate, hasSandbox bool) refuterSeat {
	if n <= 1 {
		return refuterSeat{}
	}
	if i == 0 && hasSandbox && isExecutableClaim(c) {
		return executorSeat
	}
	return seatForIndex(i, n)
}

// seatForIndex returns the seat assigned to panel position i (0-based).
// When n == 1 (degraded / single refuter) no seat clause is assigned — the
// single generalist must produce today's prompt byte-identical, so we return
// the zero refuterSeat (empty name and clause).
// When n >= 2, seats are assigned round-robin from builtinSeats so every panel
// position up to len(builtinSeats) gets a distinct specialty; positions beyond
// len(builtinSeats) cycle back.
func seatForIndex(i, n int) refuterSeat {
	if n <= 1 {
		return refuterSeat{}
	}
	return builtinSeats[i%len(builtinSeats)]
}

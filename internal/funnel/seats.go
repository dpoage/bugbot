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

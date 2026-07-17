package repro

import (
	"strings"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// evidence_binding.go implements bugbot-u47n: closing the vacuous-promotion
// channel where interpret()'s STRUCTURED path (classifyGoEvents on go test
// -json, parseJUnitXML on captured JUnit XML) finds dispositive ran-evidence
// from ANY failing test in the targeted package/module — including an
// unrelated, pre-existing failure that has nothing to do with the plan's own
// injected test. bugbot-c49s's 2/2 determinism re-run does NOT catch this: a
// deterministic pre-existing failure demonstrates twice, identically, and
// promotes just as confidently as a genuine repro.
//
// bindTestEvidence is a pure function of (verdict, files) — no state, no I/O
// — so it applies identically whether called once (the official run) or
// twice (bugbot-c49s's confirmation re-run): both calls see the same plan
// and, for a genuinely deterministic foreign failure, the same failing test
// name, so both classify foreign_test_failure the same way.

// bindTestEvidence checks a demonstrated verdict's STRUCTURED failing-test
// evidence (v.structuredFailingTests, set only by interpret()'s go test
// -json / JUnit XML path) against the test names declared in files (a
// plan's or an iteration workspace's injected file set — see
// extractGoTestNames / extractPyTestNames). Three outcomes:
//
//   - v is not a structured-path demonstration (v.demonstrated is false, or
//     structuredFailingTests is empty — the marker-cascade path never
//     populates it): v is returned UNCHANGED. This is the mechanism by
//     which every existing marker-path demonstration (cargo, bazel, JS,
//     unknown ecosystems, and Go/Python runs whose structured record failed
//     to parse) is completely unaffected by this feature.
//   - files has no extractable declared test names for v's ecosystem
//     (extractGoTestNames/extractPyTestNames returns nil — e.g. a bare
//     script, or a test file whose declaration this scan's dumb regex
//     missed): v is returned UNCHANGED. Binding only ever TIGHTENS the
//     structured path when declared names are actually available; it must
//     never turn "we couldn't extract a name" into a false rejection.
//   - files HAS declared names, but NONE of v.structuredFailingTests
//     matches one: v is downgraded to a fresh, non-promoting verdict with
//     VerdictReasonForeignFailure, naming the offending foreign test in
//     v.foreignTest for feedback()'s corrective message.
//
// Must be called AFTER interpret() but BEFORE witnessDemonstration: an
// unrelated failing test proves nothing about the target file either, but
// "foreign_test_failure" is the more specific, more actionable diagnosis and
// must not be masked by witnessDemonstration's coverage-based downgrade to
// target_not_executed.
func bindTestEvidence(v verdict, files map[string]string) verdict {
	if !v.demonstrated || len(v.structuredFailingTests) == 0 {
		return v
	}

	var declared []string
	var matches func(failing, declared string) bool
	switch v.ecosystem {
	case sandbox.EcosystemGo:
		declared = extractGoTestNames(files)
		matches = goFailingNameMatches
	case sandbox.EcosystemPython:
		declared = extractPyTestNames(files)
		matches = substringNameMatches
	default:
		// No structured path exists for any other ecosystem today
		// (interpret()'s switch only covers Go and Python), so
		// structuredFailingTests can never be populated here — defensive
		// pass-through in case that ever changes.
		return v
	}
	if len(declared) == 0 {
		return v
	}

	for _, failing := range v.structuredFailingTests {
		for _, d := range declared {
			if matches(failing, d) {
				return v
			}
		}
	}

	return verdict{
		reason:      VerdictReasonForeignFailure,
		summary:     v.summary,
		ecosystem:   v.ecosystem,
		foreignTest: v.structuredFailingTests[0],
	}
}

// goFailingNameMatches reports whether failing (a go test -json Test field,
// e.g. "TestFoo" or "TestFoo/sub_case") is the declared test itself or one of
// its subtests. Subtests are Go's own "Parent/Child" naming convention, so a
// declared "TestFoo" must match a failing "TestFoo/sub_case" — the plan
// wrote TestFoo; t.Run created the subtest at runtime.
func goFailingNameMatches(failing, declared string) bool {
	return failing == declared || strings.HasPrefix(failing, declared+"/")
}

// substringNameMatches reports whether declared appears anywhere in failing.
// Used for pytest JUnit XML, whose testcase "name" attribute typically
// carries a module/class-qualified id pytest itself constructs (e.g.
// "test_mod.TestClass::test_thing" or a fixture-decorated variant) — the
// bare declared name (e.g. "test_thing") is a substring of it, never an
// exact match.
func substringNameMatches(failing, declared string) bool {
	return strings.Contains(failing, declared)
}

// pytestNodeMatches reports whether a pytest node id — the token after
// "FAILED " in a short-summary line, e.g.
// "tests/test_x.py::TestClass::test_thing[param]" — names the declared test.
// It compares declared ONLY against the "::"-separated components AFTER the
// leading file-path token (with any "[param]" suffix stripped), so a
// declared "test_thing" matches "…::test_thing" and "…::test_thing[case]",
// and a declared suite class "TestClass" matches the class component — but a
// foreign "…::test_thing_helper" never matches "test_thing", and the
// file-path token ("test_things.py") can never be mistaken for a test name.
// This is the pytest analogue of goFailingNameMatches: boundary-anchored,
// never a raw substring of the whole line.
func pytestNodeMatches(node, declared string) bool {
	parts := strings.Split(node, "::")
	if len(parts) < 2 {
		return false // no nodeid separator: not a pytest test id
	}
	for _, p := range parts[1:] {
		if i := strings.IndexByte(p, '['); i >= 0 {
			p = p[:i]
		}
		if p == declared {
			return true
		}
	}
	return false
}

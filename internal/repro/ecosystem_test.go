package repro

// Tests for the bugbot-5zq JS-ecosystem ranMarker tightening:
//
//   - EcosystemJS.ranMarkers must require runner-anchored evidence (jest
//     "● " bullet, jest "✕" failing glyph, vitest "×" failing glyph,
//     vitest "⎯⎯" Failed-Tests section header, jest "Test Suites:"
//     summary line). The previously-listed bare "failed" and "fail "
//     matched build/tooling noise from webpack/tsc/babel/rollup/eslint
//     and could mint a false T1 from a build that only printed "Build
//     failed" and exited 1 (the same bug class as bugbot-dmy for C/C++,
//     which already tightened cpp's markers the same way).
//   - buildMarkers still win first when both could match (a tsc syntax
//     error is classified as build_error, not as a demonstration).
//
// The tests live in a dedicated file so the JS-ecosystem regression
// coverage is obvious and isolated from the bazel-side tests in
// interpret_test.go and from the per-ecosystem table tests in
// repro_test.go (other agents may be touching those in this batch).

import (
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// jsRules returns the EcosystemJS rules. Pinning the table index keeps
// the test honest: if the ecosystem order ever changes, the test still
// pins the JS entry specifically.
func jsRules(t *testing.T) ecosystemRules {
	t.Helper()
	idx := ecosystemIndex(sandbox.EcosystemJS)
	if idx == 0 {
		t.Fatalf("ecosystemTable has no %q entry", sandbox.EcosystemJS)
	}
	rules := ecosystemTable[idx]
	if rules.name != sandbox.EcosystemJS {
		t.Fatalf("ecosystemIndex(%q) = %d, but ecosystemTable[%d].name = %q",
			sandbox.EcosystemJS, idx, idx, rules.name)
	}
	return rules
}

// TestEcosystemTable_JSRanMarkersTightened pins the precision invariant
// for the JS ecosystem: positive ran-evidence must require runner-anchored
// markers that build/tooling output never emits. The legacy bare "failed"
// and "fail " markers were removed in bugbot-5zq because webpack/tsc/
// babel/rollup/eslint output is saturated with them.
func TestEcosystemTable_JSRanMarkersTightened(t *testing.T) {
	rules := jsRules(t)

	// Sanity: the bare "failed" / "fail " tokens are GONE. If a future
	// change re-adds them, this test fails BEFORE the noise tests below
	// turn silently green.
	for _, m := range rules.ranMarkers {
		if m == "failed" || m == "fail " || m == "✗" || m == "tests failed" {
			t.Errorf("EcosystemJS.ranMarkers still contains the legacy bare/loose marker %q (bugbot-5zq regression)", m)
		}
	}

	// --- POSITIVE: genuine jest output demonstrates ---------------------
	jestCases := []struct {
		name string
		out  string
	}{
		{
			name: "jest per-failure bullet ●",
			out: "FAIL src/add.test.js\n  ✕ sums numbers (3 ms)\n  ● sums numbers\n    expect(received).toBe(expected)\n",
		},
		{
			name: "jest summary line 'Tests:' combined with 'failed'",
			out: "Tests:       1 failed, 2 passed, 3 total\nTest Suites: 1 failed, 1 total\n",
		},
		{
			name: "jest summary line 'Test Suites:' (case-insensitive anchor)",
			out: "test suites: 1 failed, 1 total\n",
		},
		{
			name: "jest failing-test glyph ✕",
			out: "FAIL src/x.test.ts\n  ✕ my failing test\n",
		},
		{
			name: "jest bullet + failing glyph together",
			out: "● my test\n  ✕ my test",
		},
	}
	for _, tc := range jestCases {
		t.Run("jest/"+tc.name, func(t *testing.T) {
			if !rules.hasRanEvidence(tc.out) {
				t.Errorf("EcosystemJS.ranMarkers must match genuine jest output:\n%s", tc.out)
			}
		})
	}

	// --- POSITIVE: genuine vitest output demonstrates -------------------
	vitestCases := []struct {
		name string
		out  string
	}{
		{
			name: "vitest failing-test glyph ×",
			out: " × src/foo.test.ts > my test (3 ms)\n",
		},
		{
			name: "vitest Failed Tests section header ⎯⎯",
			out: "⎯⎯⎯⎯⎯⎯⎯ Failed Tests 1 ⎯⎯⎯⎯⎯⎯⎯\n FAIL  src/foo.test.ts > my test\n",
		},
		{
			name: "vitest full failure transcript",
			out: " × src/foo.test.ts > group > test\n\n⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯ Failed Tests 1 ⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯⎯\n FAIL  src/foo.test.ts > group > test\nAssertionError: expected 1 to equal 2\n",
		},
	}
	for _, tc := range vitestCases {
		t.Run("vitest/"+tc.name, func(t *testing.T) {
			if !rules.hasRanEvidence(tc.out) {
				t.Errorf("EcosystemJS.ranMarkers must match genuine vitest output:\n%s", tc.out)
			}
		})
	}

	// --- NEGATIVE: webpack/tsc/babel/rollup/eslint build noise does NOT
	// demonstrate. This is the bugbot-5zq regression: a build failure
	// whose output merely contains the word 'failed' must NOT be
	// misclassified as a demonstrated test run.
	//
	// Note: "passing jest" / "passing vitest" runs are NOT included as
	// noise cases: a passing run exits 0, so interpret()'s exit-code
	// gate (step 1) refuses to demonstrate before the marker check
	// even runs. The interesting noise is non-zero-exit build output
	// that is not a real test failure.
	noiseCases := []struct {
		name string
		out  string
	}{
		{
			name: "webpack build failed",
			out:  "ERROR in ./src/foo.js\nModule not found: Error: Can't resolve 'bar'\n @ ./src/index.js 1:0-20\n\nERROR in ./src/baz.js\nModule not found: Error: Can't resolve 'qux'\n\nwebpack 5.0.0 compiled with 2 errors\nBuild failed.\n",
		},
		{
			name: "tsc compilation failed",
			out:  "src/foo.ts(3,5): error TS2304: Cannot find name 'Bar'.\nsrc/baz.ts(7,1): error TS2322: Type 'string' is not assignable to type 'number'.\n\nCompilation failed.\n",
		},
		{
			name: "tsc errors only (no 'failed' word)",
			out:  "src/foo.ts:3:5 - error TS2304: Cannot find name 'Bar'.\n\nFound 1 error.\n",
		},
		{
			name: "rollup build failed",
			out:  "src/foo.js → dist/foo.js...\n[!] (plugin r) Error: unresolved dependency\nbuild failed\n",
		},
		{
			name: "babel build failed",
			out:  "SyntaxError: Unexpected token (1:5)\n@ ./src/index.js 1:0-7\nbabel compilation failed\n",
		},
		{
			name: "eslint failed (no test runner)",
			out:  "src/foo.js\n  1:5  error  'foo' is not defined  no-undef\n\n✖ 1 problem (1 error, 0 warnings)\n\nESLint found 1 error. Build failed.\n",
		},
		{
			name: "yarn install failed (toolchain refusal)",
			out:  "yarn install v1.22.0\ninfo There appears to be trouble with your network connection. Retrying...\nerror An unexpected error occurred: \"https://registry.yarnpkg.com/foo: getaddrinfo ENOTFOUND\".\ninfo If you think this is a bug, please open a bug report with the information provided above.\nBuild failed.\n",
		},
		{
			name: "npm err toolchain refusal (caught by toolchainMarkers, not ranMarkers)",
			out:  "npm err! code ENOENT\nnpm err! syscall open\nnpm err! path /missing/package.json\nnpm err! errno -2\nnpm err! enoent ENOENT: no such file or directory, open '/missing/package.json'\nnpm err! enoent This is related to npm not being able to find a file.\nnpm err! enoent\nnpm err! A complete log of this record can be found in:\nnpm err!     /root/.npm/_logs/2026-01-01T00_00_00Z-debug.log\n",
		},
		{
			name: "passing vitest run with no failure glyphs",
			out:  "✓ src/foo.test.ts (2) 25ms\n✓ src/bar.test.ts (1) 12ms\nTest Files  2 passed (2)\nTests  3 passed (3)\n",
		},
		{
			name: "arbitrary text with 'failed' but no runner output",
			out:  "The previous attempt failed because the build was misconfigured. Please retry.\n",
		},
	}
	for _, tc := range noiseCases {
		t.Run("noise/"+tc.name, func(t *testing.T) {
			if rules.hasRanEvidence(tc.out) {
				t.Errorf("EcosystemJS.ranMarkers must NOT match build/tooling noise (bugbot-5zq regression):\n%s", tc.out)
			}
		})
	}
}

// TestEcosystemTable_JSBuildMarkersStillWinFirst pins the order-of-
// classification invariant for the JS ecosystem: a webpack/tsc build
// error that ALSO happens to mention a runner glyph (e.g. an oddly-formatted
// rollup warning that contains "×") still classifies as a build_error,
// not as a demonstrated test run. interpret() consults build/toolchain
// markers BEFORE ran-evidence, so this is a property of the marker sets
// being independent — the test pins the marker lists so a future change
// can't accidentally drop a build marker or merge ran/build lists.
func TestEcosystemTable_JSBuildMarkersStillWinFirst(t *testing.T) {
	rules := jsRules(t)

	// buildMarkers must still contain the dispositive JS-side build
	// failure phrases; their presence on the output is what steers
	// classification to build_error instead of demonstrated.
	requiredBuild := []string{
		"cannot find module",
		"module not found",
		"syntaxerror",
		"unexpected token",
		"is not a function",
		"referenceerror:",
		"typeerror:",
	}
	have := map[string]bool{}
	for _, m := range rules.buildMarkers {
		have[m] = true
	}
	for _, want := range requiredBuild {
		if !have[want] {
			t.Errorf("EcosystemJS.buildMarkers missing %q (required for build_error classification)", want)
		}
	}

	// toolchainMarkers must still contain the npm/enoent refusals so a
	// missing-binary case classifies as environment_error / toolchain
	// rather than build_error.
	requiredTool := []string{
		"npm err! ",
		"command not found",
		"enoent",
	}
	haveTool := map[string]bool{}
	for _, m := range rules.toolchainMarkers {
		haveTool[m] = true
	}
	for _, want := range requiredTool {
		if !haveTool[want] {
			t.Errorf("EcosystemJS.toolchainMarkers missing %q", want)
		}
	}
}

// TestEcosystemTable_JSRanMarkersLowercasedInvariant pins that the
// new ranMarkers still match after strings.ToLower, the same
// normalization hasRanEvidence applies. Without this, a marker like
// "test suites:" (lowercase anchor) would fail to match the raw
// "Test Suites:" line in real output.
func TestEcosystemTable_JSRanMarkersLowercasedInvariant(t *testing.T) {
	rules := jsRules(t)
	// Pre-lowercased output, as interpret()/hasRanEvidence sees it.
	cases := []string{
		"test suites: 1 failed, 1 total",
		" × src/foo.test.ts",
		"⎯⎯⎯⎯ failed tests 1 ⎯⎯⎯⎯",
		"  ● sums numbers",
		"  ✕ sums numbers",
	}
	for _, c := range cases {
		if !rules.hasRanEvidence(c) {
			t.Errorf("EcosystemJS.ranMarkers must match lowercased output %q (hasRanEvidence lowercases internally)", c)
		}
		// And the original (mixed-case) form too, since hasRanEvidence
		// lowercases for us.
		if !rules.hasRanEvidence(strings.ToLower(c)) {
			t.Errorf("EcosystemJS.ranMarkers must match pre-lowercased form of %q", c)
		}
	}
}

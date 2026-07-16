package ecosystem

// interp.go holds the per-ecosystem output-interpretation registry: the rules
// that classify a sandbox run's stdout/stderr as "demonstrated", "env error",
// "build error", "toolchain refusal", or "not demonstrated". The table was
// previously defined inline in repro/ecosystem.go; moving it here makes it the
// single source of truth for output interpretation alongside the existing test-
// argv registry in registry.go.
//
// repro/ecosystem.go re-exports all public symbols from this file for backward
// compatibility; callers inside repro that use the lowercase aliases still work.
//
// # Adding a new ecosystem
//
// Append one InterpRules entry to InterpTable and extend DetectEcosystem to
// recognise the launcher. The interpretation pipeline (interpret.go in repro)
// does not change. Run `go test ./internal/ecosystem/...` — the completeness
// check will catch a missing entry if you also add the entry to the
// allKnownEcosystems list in interp_test.go.

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

// InterpRules describes how to interpret a sandbox result for a given testing
// ecosystem. The rules are intentionally positive: a non-zero exit becomes a
// "demonstrated" outcome only when the recorded output contains positive
// evidence the test RAN and FAILED for that ecosystem.
//
// Fields are exported so repro/ecosystem.go and repro/interpret.go can
// consume them without an alias.
type InterpRules struct {
	// Name is the ecosystem identifier; mirrors Ecosystem values.
	Name Ecosystem
	// RanMarkers are lowercase substrings whose presence on combined output is
	// positive evidence the test process actually RAN.
	RanMarkers []string
	// LineAnchoredRanMarkers are ran-evidence markers that must match at the
	// START of a line rather than anywhere in the output — for markers short
	// or generic enough (e.g. TAP's "not ok ") that a free substring match
	// would risk false positives on unrelated log lines. bugbot-ds90.
	LineAnchoredRanMarkers []string
	// NotRanMarkers are lowercase substrings that, when present, prove the test
	// collection / setup FAILED before any test ran.
	NotRanMarkers []string
	// BuildMarkers are lowercase substrings that classify the failure as a
	// build/compile/import error.
	BuildMarkers []string
	// ToolchainMarkers are lowercase substrings that indicate the project's
	// own toolchain refused the request.
	ToolchainMarkers []string
	// LineAnchoredToolchainMarkers are toolchain-refusal patterns that must
	// match at the START of a line.
	LineAnchoredToolchainMarkers []string
}

// HasRanEvidence reports whether out contains at least one of the ecosystem's
// positive ran-markers (free substring or line-anchored). Matching is
// case-insensitive.
func (e *InterpRules) HasRanEvidence(out string) bool {
	if e == nil {
		return false
	}
	low := strings.ToLower(out)
	for _, m := range e.RanMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return HasAnyMarkerAtLineStart(out, e.LineAnchoredRanMarkers)
}

// HasNotRanEvidence reports whether out contains any of the ecosystem's
// NotRanMarkers.
func (e *InterpRules) HasNotRanEvidence(out string) bool {
	if e == nil {
		return false
	}
	low := strings.ToLower(out)
	for _, m := range e.NotRanMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// HasLineAnchoredToolchainMarker checks both the free ToolchainMarkers
// (substring) AND the LineAnchoredToolchainMarkers (line-start anchored).
func (e *InterpRules) HasLineAnchoredToolchainMarker(out string) bool {
	if HasAnyMarker(out, e.ToolchainMarkers) {
		return true
	}
	return HasAnyMarkerAtLineStart(out, e.LineAnchoredToolchainMarkers)
}

// HasAnyMarker reports whether out contains any of the given (lowercase)
// substrings. Case-insensitive.
func HasAnyMarker(out string, markers []string) bool {
	low := strings.ToLower(out)
	for _, m := range markers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// HasAnyMarkerAtLineStart reports whether any of the given (lowercase) markers
// appears at the beginning of a line (after '\n' or at the very start of out).
func HasAnyMarkerAtLineStart(out string, markers []string) bool {
	low := strings.ToLower(out)
	for _, m := range markers {
		if strings.HasPrefix(low, m) {
			return true
		}
		idx := 0
		for {
			nl := strings.Index(low[idx:], "\n")
			if nl < 0 {
				break
			}
			lineStart := idx + nl + 1
			if lineStart >= len(low) {
				break
			}
			if strings.HasPrefix(low[lineStart:], m) {
				return true
			}
			idx = lineStart
		}
	}
	return false
}

// DefaultEnvMarkers are environment markers common to every ecosystem —
// a read-only root or a full disk should never count as a reproduction
// regardless of language.
var DefaultEnvMarkers = []string{
	"failed to initialize build cache",
	"read-only file system",
	"no space left on device",
	"cannot create temporary",
}

// BazelEnvMarkers is DefaultEnvMarkers WITHOUT "read-only file system". Every
// bazel run in the read-only-root sandbox prints benign "(Read-only file
// system)" disk-cache warnings.
var BazelEnvMarkers = []string{
	"failed to initialize build cache",
	"no space left on device",
	"cannot create temporary",
}

// SanitizerReportMarkers are the unambiguous report headers a sanitizer or
// valgrind emits ONLY when it DETECTS a violation.
var SanitizerReportMarkers = []string{
	"sanitizer:",
	"detected memory leaks",
	"definitely lost",
	"invalid read of size",
	"invalid write of size",
}

// RuntimeFailureMarkers is the full dispositive ran-and-failed set:
// SanitizerReportMarkers plus looser runtime-failure phrases.
var RuntimeFailureMarkers = append([]string{
	"runtime error:",
	"data race",
}, SanitizerReportMarkers...)

// ReproSentinelDemonstrated is the exact literal token the reproducer agent
// must print to stdout ONLY on the code path confirming the bug is present.
const ReproSentinelDemonstrated = "BUGBOT_REPRO_DEMONSTRATED"

// ReproSentinelMarkers is the marker slice wrapping ReproSentinelDemonstrated
// so it composes with the existing HasAnyMarker helper.
var ReproSentinelMarkers = []string{
	strings.ToLower(ReproSentinelDemonstrated),
}

// InterpTable is the ordered registry of supported ecosystems for output
// interpretation. Order matters only for tie-breaking in DetectEcosystem;
// the first match wins. Go is intentionally first to keep legacy verdicts
// bit-for-bit compatible.
//
// Adding a new ecosystem: append one InterpRules entry here and extend
// DetectEcosystem to recognise its launcher.
var InterpTable = []InterpRules{
	{
		Name: EcosystemGo,
		RanMarkers: []string{
			"--- fail",
			"fail\t",
			"panic:",
			"warning: data race",
			"fatal error:",
		},
		BuildMarkers: []string{
			"build failed",
			"[build failed]",
			"cannot find package",
			"undefined:",
			"undeclared name",
			"no required module provides package",
			"missing go.sum entry",
			"cannot find module",
			"expected declaration",
			"syntax error",
			"is not in std",
			"go: updates to go.mod needed",
			"go: downloading",
			": cannot use ",
			"too many errors",
		},
		LineAnchoredToolchainMarkers: []string{
			"go: ",
		},
	},
	{
		Name: EcosystemPython,
		RanMarkers: []string{
			// "failed " (bare, with trailing space) was REMOVED here
			// (bugbot-2zoo round 3): it is pass-ambiguous, matching a
			// PASSING pytest -v test whose NAME happens to end in
			// "failed" (e.g. "test_login_when_credentials_failed PASSED"
			// lowercases to "..._failed passed", which contains
			// "failed "). pipefail (injected by internal/repro when an
			// agent bounds output with an early-terminating filter like
			// `head -N`) can turn a passing pytest run into a non-zero
			// exit (Python converts a broken pipe from a closed
			// downstream filter into a BrokenPipeError -> exit 1, NOT a
			// SIGPIPE-range exit, so there is no exit-code backstop for
			// this one) with exactly that PASSED line captured — the old
			// marker would then mint a false demonstrated. Genuine pytest
			// failures remain covered below by "= failures =" (pytest's
			// own FAILURES banner) and "short test summary" (both printed
			// only when at least one test fails), plus "assertionerror"
			// and "traceback (most recent call last)". unittest's
			// failure-only "FAILED (failures=N)"/"FAILED (errors=N)"
			// summary line is covered by LineAnchoredRanMarkers below
			// instead of as a bare substring: unittest's verbose PASSING
			// output lists each test as "<name> (<module.Class>) ... ok",
			// and a test named e.g. "test_login_failed" produces the line
			// "test_login_failed (tests.TestX) ... ok" — which contains
			// the substring "failed (" mid-line despite the test PASSING.
			// Anchoring to the START of the line excludes that trap while
			// still matching "FAILED (failures=1)", which unittest always
			// prints flush-left with no other line ever starting that way.
			"= failures =",
			"short test summary",
			"assertionerror",
			"traceback (most recent call last)",
		},
		LineAnchoredRanMarkers: []string{
			"failed (",
		},
		NotRanMarkers: []string{
			"errors during collection",
			"error during collection",
			"error collecting",
			"= no tests ran =",
			"no tests ran",
			"collected 0 items",
		},
		BuildMarkers: []string{
			"syntaxerror",
			"importerror:",
			"modulenotfounderror:",
			"indentationerror",
			"nameerror:",
		},
		ToolchainMarkers: []string{
			"pytest: error:",
			"no module named pytest",
			"command not found",
		},
	},
	{
		Name: EcosystemRust,
		RanMarkers: []string{
			// "test result: failed" (not the bare "test result:" prefix,
			// which also appears on a PASSING run as "test result: ok.") —
			// bugbot-2zoo: pipefail (injected by internal/repro when an
			// agent bounds output with an early-terminating filter like
			// `head -N`/`grep -m1`) can make a passing `cargo test`
			// pipeline exit 141 on SIGPIPE while the doctest runner is
			// still writing output; a bare "test result:" marker would
			// then match the PASSING unit-test summary that already
			// scrolled past and mint a false demonstrated. Genuine
			// failures always print "test result: FAILED." (cargo's own
			// wording, case-folded by HasRanEvidence's lowercasing).
			"test result: failed",
			"failing tests:",
			"thread '",
			"panicked at",
		},
		BuildMarkers: []string{
			"error[e",
			"error: cannot find",
			"error: expected",
			"error: unresolved import",
			"unresolved import",
			"aborting due to ",
			"could not compile",
		},
		ToolchainMarkers: []string{
			"cargo: not found",
			"error: no such command",
		},
	},
	{
		Name: EcosystemJS,
		RanMarkers: []string{
			"● ",
			"✕",
			"×",
			"⎯⎯",
			// "test suites:" was REMOVED here (bugbot-2zoo): it is
			// pass-ambiguous, matching a PASSING jest summary line
			// ("Test Suites: 1 passed, 1 total") just as readily as a
			// failing one. pipefail (injected by internal/repro when an
			// agent bounds output with an early-terminating filter like
			// `grep -m1`) can make a passing jest pipeline exit 141 on
			// SIGPIPE, and a bare "test suites:" marker would then match
			// the passing summary and mint a false demonstrated. Genuine
			// jest/vitest failures remain covered by the glyph markers
			// above, "assertionerror" below, and the line-anchored TAP
			// "not ok " marker (node:test).
			"assertionerror",
		},
		// node:test's TAP output marks a failing subtest with a line starting
		// "not ok " — anchored to line-start (not a free substring) since the
		// bare phrase is generic enough to risk false positives elsewhere in
		// combined stdout/stderr. bugbot-ds90.
		LineAnchoredRanMarkers: []string{
			"not ok ",
		},
		BuildMarkers: []string{
			"cannot find module",
			"module not found",
			"syntaxerror",
			"unexpected token",
			"is not a function",
			"referenceerror:",
			"typeerror:",
		},
		ToolchainMarkers: []string{
			"npm err! ",
			"command not found",
			"enoent",
		},
	},
	{
		Name: EcosystemCpp,
		RanMarkers: []string{
			"[  failed  ]",
			"failed test",
			// "tests failed" (bare) was replaced with the failure-only
			// "the following tests failed" (bugbot-2zoo round 3): a
			// PASSING ctest run's own summary line reads "100% tests
			// passed, 0 tests failed out of N" — which contains the bare
			// substring "tests failed". pipefail (injected by
			// internal/repro) can make a `bash -c` ctest pipeline bounded
			// by an early-terminating filter exit non-zero on SIGPIPE
			// (ctest is a compiled binary, so unlike Python it does hit
			// the SIGPIPE-range exit) while that passing summary line is
			// what got captured, which used to mint a false demonstrated.
			// ctest only ever prints "The following tests FAILED:" (case
			// preserved here for clarity; matching is case-folded by
			// HasRanEvidence) when at least one test genuinely failed.
			"the following tests failed",
			"assertion failed",
			"assertion \x60",
		},
		BuildMarkers: []string{
			"error: ",
			"undefined reference",
			"fatal error:",
			"no such file",
		},
		ToolchainMarkers: []string{
			"cmake error",
			"ctest: not found",
		},
	},
	{
		Name: EcosystemBazel,
		RanMarkers: []string{
			"fails locally",
			"failed in ",
		},
		BuildMarkers: []string{
			"no such target",
			"no such package",
			"build did not complete",
			"error: ",
		},
		ToolchainMarkers: []string{
			"command not found",
			"bazel: not found",
		},
	},
	{
		Name: EcosystemUnknown,
		RanMarkers: []string{
			"[  failed  ]",
			"assertion failed",
			"assertion \x60",
			"assertionerror",
			"panic:",
		},
		BuildMarkers: []string{},
		ToolchainMarkers: []string{
			"command not found",
			"enoent",
		},
	},
}

// WitnessRules describes how to detect, from a sandbox run's combined
// output, low-false-positive evidence that the finding's TARGET FILE
// specifically was (or was not) exercised during the run. This sits next
// to InterpTable: InterpTable answers "did a test run and fail";
// WitnessTable answers "does a per-file coverage report say the target was
// touched". bugbot-qb4r layer (b).
//
// Deliberately NOT based on stack-trace/traceback file references: an
// ordinary failing assertion (assertEqual, t.Errorf, expect().toBe()) in
// EVERY one of these ecosystems reports the file:line of the ASSERTION
// (the test file), not the target file being asserted on, even for a
// completely genuine bug demonstration — only a panic/exception that
// unwinds through the target's own frames would show it. Trying to require
// a target-file trace line would reject the overwhelmingly common "wrong
// value, not a crash" bug shape. Coverage-tool output, when the agent's
// command happens to produce it, is the one signal that is both reliably
// parseable and low-false-positive: a coverage report explicitly saying
// the target file has 0% coverage IS trustworthy negative evidence: the
// file's code truly never ran. Its ABSENCE (no coverage row for the
// target at all) proves nothing either way, so callers treat "no data" as
// permissive — see repro.witnessDemonstration.
//
// Ecosystems absent from WitnessTable (bazel, unknown) have no
// standardized coverage-report format at all — the caller downgrades those
// to the existing witness-only promotion path instead of trusting a bare
// exit-code/ran-marker demonstration as full Tier-1.
type WitnessRules struct {
	// Name is the ecosystem identifier; mirrors Ecosystem values.
	Name Ecosystem
	// CoverageRowPatterns are regexp templates with exactly one %s verb
	// (filled with the target file's escaped basename before compiling)
	// and exactly one capturing group: the file's covered-percentage as a
	// plain number (no '%' sign), taken from that ecosystem's coverage-tool
	// summary report line for the file (e.g. coverage.py's `Name Stmts
	// Miss Cover` row, jest --coverage's per-file table row, `go tool
	// cover -func`'s per-function row).
	CoverageRowPatterns []string
}

// WitnessTable is the per-ecosystem execution-witness registry. Only
// ecosystems with a standard, parseable coverage-report format are listed;
// bazel (target-label-level summaries) and unknown (no agreed format) are
// intentionally absent.
var WitnessTable = []WitnessRules{
	{
		// go tool cover -func output: "pkg/widget.go:10:  New   100.0%"
		Name:                EcosystemGo,
		CoverageRowPatterns: []string{`%s:\d+:\s+\S+\s+(\d+(?:\.\d+)?)%`},
	},
	{
		// coverage.py / pytest-cov term report: "agent/main.py  42  5  88%"
		Name:                EcosystemPython,
		CoverageRowPatterns: []string{`%s\s+\d+\s+\d+\s+(\d+(?:\.\d+)?)%`},
	},
	{
		// jest/istanbul --coverage text-summary table: " main.js | 85.71 | ..."
		Name:                EcosystemJS,
		CoverageRowPatterns: []string{`%s\s*\|\s*(\d+(?:\.\d+)?)`},
	},
	{
		// cargo llvm-cov / tarpaulin summary line: "widget.rs: 85.00% ..."
		Name:                EcosystemRust,
		CoverageRowPatterns: []string{`%s:?\s+(\d+(?:\.\d+)?)%`},
	},
	{
		// gcov/lcov summary: "File 'widget.cpp' ... Lines executed:85.00%"
		Name:                EcosystemCpp,
		CoverageRowPatterns: []string{`%s'[\s\S]{0,160}?executed:(\d+(?:\.\d+)?)%`},
	},
}

// WitnessRulesFor returns the WitnessRules for name, or (zero, false) when
// the ecosystem has no execution-witness support.
func WitnessRulesFor(name Ecosystem) (WitnessRules, bool) {
	for _, w := range WitnessTable {
		if w.Name == name {
			return w, true
		}
	}
	return WitnessRules{}, false
}

// TargetCoverage reports the target file's covered-percentage found in out,
// per this ecosystem's coverage-report row patterns. found is false when no
// row for the target file's basename is present at all (the run did not
// produce coverage output, or produced it for other files only) — callers
// MUST treat that as "no evidence either way", not as a negative signal.
// When found is true, pct is the parsed percentage (0 is a genuine, trusted
// "this file never ran" result).
func (w WitnessRules) TargetCoverage(out, targetPath string) (pct float64, found bool) {
	if targetPath == "" || len(w.CoverageRowPatterns) == 0 {
		return 0, false
	}
	base := regexp.QuoteMeta(path.Base(targetPath))
	for _, tmpl := range w.CoverageRowPatterns {
		// strings.Replace, not fmt.Sprintf: the templates contain literal
		// '%' characters (the coverage percentage sign) that fmt.Sprintf
		// would otherwise try to interpret as format verbs.
		re, err := regexp.Compile(strings.Replace(tmpl, "%s", base, 1))
		if err != nil {
			continue
		}
		m := re.FindStringSubmatch(out)
		if m == nil {
			continue
		}
		if v, perr := strconv.ParseFloat(m[1], 64); perr == nil {
			return v, true
		}
	}
	return 0, false
}

// interpIndex returns the position of the named entry in InterpTable, or 0
// if the name is unknown (defensive default — the Go entry MUST be at index 0).
func interpIndex(name Ecosystem) int {
	for i, e := range InterpTable {
		if e.Name == name {
			return i
		}
	}
	return 0
}

// IsGoTestSubcommand reports whether s is one of the Go subcommands that runs
// tests and produces testing-package output.
func IsGoTestSubcommand(s string) bool {
	switch s {
	case "test", "vet", "run", "tool", "generate":
		return true
	}
	return false
}

// benignWrapperEnvAssignment matches a leading inline env-var assignment
// token, e.g. "FOO=bar" or "PYTHONPATH=/x:/y", prefixing the real command.
var benignWrapperEnvAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// shellSegmentSplit finds top-level ';' and '&&' command separators in a
// compound shell command string ("a; b", "a && b").
var shellSegmentSplit = regexp.MustCompile(`;|&&`)

// splitShellSegments splits s on ';' and '&&' into trimmed, non-empty
// segments, preserving order. Does not attempt to respect quoting — same
// simplification UnwrapShell already made via strings.Fields.
func splitShellSegments(s string) []string {
	parts := shellSegmentSplit.Split(s, -1)
	segments := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			segments = append(segments, p)
		}
	}
	return segments
}

// stripBenignWrapper iteratively removes leading tokens that are benign
// process wrappers rather than the actual test launcher: inline env-var
// assignments ("FOO=bar cmd"), "exec", "env" (its own assignments are
// peeled by the env-assignment case on the next iteration), "timeout
// <duration>", and "nice"/"nice -n N". bugbot-ds90: these wrappers are
// common in agent-authored repro commands and previously made detection see
// the wrapper's own name instead of the real launcher.
func stripBenignWrapper(argv []string) []string {
	for len(argv) > 0 {
		switch tok := argv[0]; {
		case benignWrapperEnvAssignment.MatchString(tok):
			argv = argv[1:]
		case tok == "exec" || tok == "env":
			argv = argv[1:]
		case tok == "timeout":
			argv = argv[1:]
			if len(argv) > 0 {
				argv = argv[1:] // drop the duration operand
			}
		case tok == "nice":
			argv = argv[1:]
			if len(argv) > 0 && argv[0] == "-n" {
				argv = argv[1:]
				if len(argv) > 0 {
					argv = argv[1:] // drop the niceness value
				}
			}
		default:
			return argv
		}
	}
	return argv
}

// basenameArgv0 returns a copy of argv with argv[0] replaced by its
// basename, so an absolute toolchain path
// (/opt/bugbot-toolchains/node/bin/node) matches the same launcher as a
// bare "node" on $PATH. Leaves argv untouched (no aliasing).
func basenameArgv0(argv []string) []string {
	if len(argv) == 0 {
		return argv
	}
	out := append([]string(nil), argv...)
	out[0] = path.Base(out[0])
	return out
}

// UnwrapShell peels off `bash -c '...'` / `sh -c '...'` / `bash -lc '...'`
// style wrappers. argv[1] matches a shell flag cluster whose final flag is -c
// (e.g. "-c", "-lc", "-ec"): bash/sh read the command string from the next
// operand, so argv[2] is the inner command regardless of the leading flags.
//
// A compound inner command ("export FOO=bar; exec python3 -m pytest ...",
// "cmake -B build && ctest") is split on top-level ';' and '&&'. Segments
// are tried from LAST to FIRST — the last segment is normally the one that
// actually runs the test, e.g. "exec real-command" after a setup prefix —
// and the first segment (scanning backward) whose stripped, basenamed
// launcher token is recognised by launcherEcosystem wins (bugbot-ds90). If
// no segment is recognised, the last non-empty segment is returned as-is
// (unrecognised launcher; classifies as EcosystemUnknown downstream, same
// as before this normalization existed). Anything that does not match the
// shell-wrapper shape is left alone.
func UnwrapShell(argv []string) []string {
	if len(argv) < 3 {
		return argv
	}
	shell := argv[0]
	if shell != "bash" && shell != "sh" && shell != "/bin/bash" && shell != "/bin/sh" {
		return argv
	}
	// Accept any flag cluster ending in "c" (e.g. "-c", "-lc", "-ec").
	if !strings.HasPrefix(argv[1], "-") || !strings.HasSuffix(argv[1], "c") {
		return argv
	}
	segments := splitShellSegments(argv[2])
	if len(segments) <= 1 {
		return strings.Fields(argv[2])
	}
	var lastCandidate []string
	for i := len(segments) - 1; i >= 0; i-- {
		fields := strings.Fields(segments[i])
		if len(fields) == 0 {
			continue
		}
		candidate := basenameArgv0(stripBenignWrapper(fields))
		if lastCandidate == nil {
			lastCandidate = candidate
		}
		if len(candidate) > 0 && launcherEcosystem(candidate) != EcosystemUnknown {
			return candidate
		}
	}
	if lastCandidate != nil {
		return lastCandidate
	}
	return strings.Fields(argv[2])
}

// normalizeArgv is the shared argv-normalization pipeline consumed by both
// DetectEcosystem (this file) and InferToolFromCmd (infer.go) — bugbot-ds90
// acceptance: shell/wrapper/absolute-path handling lives in exactly one
// place instead of being duplicated per caller. It (1) unwraps a bash/sh -c
// shell wrapper, resolving compound "a; b" / "a && b" strings to the
// actually-relevant segment (see UnwrapShell); (2) strips benign process
// wrappers off the front — env-var assignments, exec, env, timeout
// <duration>, nice [-n N] — so e.g. "timeout 60 python3 x.py" and "env
// FOO=1 python3 x.py" both resolve to the real launcher; (3) basenames
// argv[0] so an absolute toolchain path matches the same launcher as a bare
// name on $PATH.
func normalizeArgv(argv []string) []string {
	argv = UnwrapShell(argv)
	return basenameArgv0(stripBenignWrapper(argv))
}

// launcherEcosystem returns the Ecosystem for an ALREADY-normalized argv
// (see normalizeArgv), or EcosystemUnknown. This is the single argv[0]
// launcher switch; UnwrapShell also calls it to pick the correct segment
// out of a compound shell command instead of duplicating the launcher list.
func launcherEcosystem(argv []string) Ecosystem {
	if len(argv) == 0 {
		return EcosystemUnknown
	}
	switch strings.ToLower(argv[0]) {
	case "go":
		if len(argv) >= 2 {
			return EcosystemGo
		}
	case "pytest", "py.test":
		return EcosystemPython
	case "python", "python3":
		// Any invocation with an argument — a script path ("python3 x.py"),
		// "-m unittest"/"-m pytest"/"-m py.test", or any other -m module —
		// is Python. A bare "python"/"python3" with no arguments (e.g. an
		// interactive REPL launch) produces no test-relevant output and
		// stays Unknown. bugbot-ds90.
		if len(argv) >= 2 {
			return EcosystemPython
		}
	case "node", "nodejs":
		return EcosystemJS
	case "cargo":
		if len(argv) >= 2 {
			return EcosystemRust
		}
	case "npm", "yarn", "pnpm", "npx":
		return EcosystemJS
	case "jest", "vitest", "mocha":
		return EcosystemJS
	case "cmake", "gcc", "g++", "clang", "clang++", "cc", "c++", "ctest", "meson":
		return EcosystemCpp
	case "bazel", "bazelisk":
		return EcosystemBazel
	}
	return EcosystemUnknown
}

// DetectEcosystem picks the InterpRules for the plan's argv launcher. The
// argv may be wrapped in a shell, wrapped in benign process wrappers (env,
// exec, timeout, nice), or reference an absolute toolchain path — see
// normalizeArgv for the full normalization pipeline.
//
// Unknown commands fall back to InterpTable["unknown"], which still requires
// positive ran-evidence.
func DetectEcosystem(argv []string) InterpRules {
	argv = normalizeArgv(argv)
	if len(argv) == 0 {
		return InterpTable[interpIndex(EcosystemUnknown)]
	}
	return InterpTable[interpIndex(launcherEcosystem(argv))]
}

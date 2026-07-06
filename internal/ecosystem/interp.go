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
// positive ran-markers. Matching is case-insensitive substring match.
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
	return false
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
			"failed ",
			"= failures =",
			"short test summary",
			"assertionerror",
			"traceback (most recent call last)",
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
			"test result:",
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
			"test suites:",
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
			"tests failed",
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

// UnwrapShell peels off `bash -c '...'` / `sh -c '...'` / `bash -lc '...'`
// style wrappers. argv[1] matches a shell flag cluster whose final flag is -c
// (e.g. "-c", "-lc", "-ec"): bash/sh read the command string from the next
// operand, so argv[2] is the inner command regardless of the leading flags.
// The inner command is split on whitespace to recover the leading launcher
// token for ecosystem detection. Anything that does not match is left alone.
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
	return strings.Fields(argv[2])
}

// DetectEcosystem picks the first InterpRules whose launcher regex matches
// the plan's argv. The argv may be wrapped in a shell ("bash", "-c",
// "go test ./...") — those are walked through by UnwrapShell.
//
// Unknown commands fall back to InterpTable["unknown"], which still requires
// positive ran-evidence.
func DetectEcosystem(argv []string) InterpRules {
	argv = UnwrapShell(argv)
	if len(argv) == 0 {
		return InterpTable[interpIndex(EcosystemUnknown)]
	}

	first := strings.ToLower(argv[0])
	switch first {
	case "go":
		if len(argv) >= 2 && IsGoTestSubcommand(argv[1]) {
			return InterpTable[interpIndex(EcosystemGo)]
		}
		if len(argv) >= 2 {
			return InterpTable[interpIndex(EcosystemGo)]
		}
	case "pytest", "py.test":
		return InterpTable[interpIndex(EcosystemPython)]
	case "python", "python3":
		if len(argv) >= 3 && argv[1] == "-m" {
			mod := strings.ToLower(argv[2])
			if mod == "pytest" || mod == "py.test" {
				return InterpTable[interpIndex(EcosystemPython)]
			}
		}
	case "cargo":
		if len(argv) >= 2 && (argv[1] == "test" || argv[1] == "bench") {
			return InterpTable[interpIndex(EcosystemRust)]
		}
		if len(argv) >= 2 {
			return InterpTable[interpIndex(EcosystemRust)]
		}
	case "npm", "yarn", "pnpm", "npx":
		return InterpTable[interpIndex(EcosystemJS)]
	case "jest", "vitest", "mocha":
		return InterpTable[interpIndex(EcosystemJS)]
	case "cmake", "gcc", "g++", "clang", "clang++", "cc", "c++", "ctest", "meson":
		return InterpTable[interpIndex(EcosystemCpp)]
	case "bazel", "bazelisk":
		return InterpTable[interpIndex(EcosystemBazel)]
	}
	return InterpTable[interpIndex(EcosystemUnknown)]
}

package repro

import (
	"strings"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// ecosystemRules describes how to interpret a sandbox result for a given
// testing ecosystem. The rules are intentionally positive: a non-zero exit
// becomes a "demonstrated" outcome only when the recorded output contains
// positive evidence the test RAN and FAILED for that ecosystem. Blacklists
// (build/env/toolchain markers) are SECONDARY: they only refine the failure
// category for the non-demonstrated branch. This inverts the old
// "non-zero-by-default-demonstrates" rule, which depended on Go-only marker
// blacklists and silently minted false T1s for every non-Go ecosystem and
// for toolchain refusals that happened to exit non-zero (e.g.
// "go: -race requires cgo" — see bugbot-vig).
//
// Adding a new ecosystem means appending one entry to ecosystemTable plus
// extending detectEcosystem to recognize its launcher. The interpretation
// pipeline itself does not change.
type ecosystemRules struct {
	// name is the ecosystem identifier; it mirrors sandbox.Ecosystem values.
	name sandbox.Ecosystem
	// ranMarkers are lowercase substrings whose presence on the combined
	// output is positive evidence the test process actually RAN. At least
	// one of these (or the legacy patterns below) must match for a non-zero
	// exit to be classified as a demonstration.
	ranMarkers []string
	// buildMarkers are lowercase substrings that, when present, classify
	// the failure as a build/compile/import error. This is the
	// false-reproduction guard: a repro that never compiled has not
	// demonstrated anything.
	buildMarkers []string
	// toolchainMarkers are lowercase substrings that indicate the
	// project's own toolchain refused the request (e.g. Go's "go: "
	// prefix, pip's "ModuleNotFoundError"). They take precedence over
	// buildMarkers when both could match, since toolchain refusals are
	// a subclass of "did not run".
	toolchainMarkers []string
}

// defaultEnvMarkers are environment markers common to every ecosystem —
// a read-only root or a full disk should never count as a reproduction
// regardless of language.
var defaultEnvMarkers = []string{
	"failed to initialize build cache",
	"read-only file system",
	"no space left on device",
	"cannot create temporary",
}

// bazelEnvMarkers is defaultEnvMarkers WITHOUT "read-only file system". Every
// bazel run in the read-only-root sandbox prints benign "(Read-only file
// system)" disk-cache warnings, so that marker cannot signal a real environment
// failure for bazel; the remaining markers (disk full, build-cache init, temp
// failures) still do. Consulted by interpret()/patchVerdict()'s bazel branch.
var bazelEnvMarkers = []string{
	"failed to initialize build cache",
	"no space left on device",
	"cannot create temporary",
}

// sanitizerReportMarkers are the unambiguous report headers a sanitizer or
// valgrind emits ONLY when it DETECTS a violation; they never appear in a clean
// run's output. Their presence alone proves the instrumented binary ran and
// failed, so interpret() trusts them INDEPENDENTLY of the recorded exit code —
// which reproducer agents routinely clobber to 0 by piping the test through
// `| tail`/`| head` (a pipeline's status is the last stage's) or appending
// `; echo EXIT=$?` (a trailing echo always exits 0). Keep this list to
// signatures that cannot occur on a successful run.
//
// Entries are lowercase for case-insensitive comparison:
//   - "sanitizer:" matches AddressSanitizer:/LeakSanitizer:/MemorySanitizer:/
//     ThreadSanitizer:/UndefinedBehaviorSanitizer: headers from clang/gcc
//     sanitizer runtimes (a TSan "ThreadSanitizer: data race" report included).
//   - "detected memory leaks" is LSan's final summary line.
//   - "definitely lost" / "invalid read of size" / "invalid write of size" are
//     valgrind/memcheck lines that appear only when the process ran.
var sanitizerReportMarkers = []string{
	"sanitizer:",
	"detected memory leaks",
	"definitely lost",
	"invalid read of size",
	"invalid write of size",
}

// runtimeFailureMarkers is the full dispositive ran-and-failed set: the
// sanitizerReportMarkers headers PLUS looser runtime-failure phrases that can
// also surface in benign program prose ("runtime error:" is UBSan's
// per-violation prefix; bare "data race" covers non-sanitizer tools like
// helgrind). The looser phrases are consulted ONLY behind a non-zero exit
// (interpret()'s post-exit-switch check) so a passing test that merely mentions
// them is never mistaken for a demonstration. They outrank the per-ecosystem
// env/toolchain/build markers because such output can only appear after the
// binary built and ran. bugbot-vig still holds: a BARE non-zero exit with none
// of these substrings still NEVER demonstrates.
var runtimeFailureMarkers = append([]string{
	"runtime error:",
	"data race",
}, sanitizerReportMarkers...)

// ecosystemTable is the ordered registry of supported ecosystems. Order
// matters only for tie-breaking in the (rare) case that the same argv
// prefix matches two entries; the first match wins. Go is intentionally
// first to keep the legacy Go verdicts bit-for-bit compatible.
var ecosystemTable = []ecosystemRules{
	{
		name: sandbox.EcosystemGo,
		ranMarkers: []string{
			"--- fail", // --- FAIL: TestX
			"fail\t",   // FAIL\tgithub.com/... — go test summary line
			"panic:",
			"warning: data race",
			"fatal error:", // runtime fatal — still a ran-and-failed
		},
		buildMarkers: []string{
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
		// Go's toolchain refusal: "go: -race requires cgo",
		// "go: command not found", "go: cannot find main module", etc.
		// "go: " is the universal Go toolchain error prefix.
		toolchainMarkers: []string{
			"go: ",
		},
	},
	{
		name: sandbox.EcosystemPython,
		ranMarkers: []string{
			"failed ",                           // pytest's "FAILED tests/test_x.py::TestX"
			"= failures =",                      // pytest's "= FAILURES =" section header
			"short test summary",                // pytest summary block
			"assertionerror",                    // pytest AssertionError per-test
			"traceback (most recent call last)", // Python exception = test ran
		},
		buildMarkers: []string{
			"syntaxerror",
			"importerror:",
			"modulenotfounderror:",
			"indentationerror",
			"nameerror:", // top-level name error usually means collection failed
		},
		// pip / pytest toolchain refusals land here.
		toolchainMarkers: []string{
			"pytest: error:",
			"no module named pytest",
			"command not found", // shell-level toolchain refusal
		},
	},
	{
		name: sandbox.EcosystemRust,
		ranMarkers: []string{
			"test result:",   // cargo test "test result: FAILED. N passed; M failed"
			"failing tests:", // cargo test failure section header
			"thread '",       // rust panic header
			"panicked at",
		},
		buildMarkers: []string{
			"error[e", // rustc error[E0xxx]:
			"error: cannot find",
			"error: expected",
			"error: unresolved import",
			"unresolved import",
			"aborting due to ",
			"could not compile",
		},
		toolchainMarkers: []string{
			"cargo: not found",
			"error: no such command",
		},
	},
	{
		name: sandbox.EcosystemJS,
		// jest / vitest / npm test. They all emit a "FAIL" line per
		// failing suite and a final summary; vitest adds a "✗" glyph
		// (kept as a fallback marker).
		ranMarkers: []string{
			"fail ", // jest "FAIL src/foo.test.js"
			"failed",
			"✗",
			"tests failed",
			"×", // vitest failure glyph
		},
		buildMarkers: []string{
			"cannot find module",
			"module not found",
			"syntaxerror",
			"unexpected token",
			"is not a function",
			"referenceerror:",
			"typeerror:",
		},
		toolchainMarkers: []string{
			"npm err! ",
			"command not found",
			"enoent",
		},
	},
	{
		name: sandbox.EcosystemCpp,
		// Positive ran-evidence MUST be anchored to real test-runner output.
		// Bare "failed"/"fail" are deliberately NOT used: C/C++ build, link,
		// and CMake-configure output is saturated with them ("ABI info -
		// failed", "Build step ... failed", "linker command failed"), so a
		// non-zero exit from a broken build would otherwise be minted as a
		// demonstration (the services-runtime standalone-repro regression).
		// What IS dispositive: gtest prints "[  FAILED  ]" per failing
		// EXPECT_*/ASSERT_*/CHECK plus an "N FAILED TEST(S)" tail; ctest prints
		// "N tests failed out of M" / "The following tests FAILED:"; a tripped
		// assert() aborts with "Assertion `expr' failed." (glibc) or "Assertion
		// failed: (expr)" (BSD/macOS). Catch2/doctest/Boost.Test summary lines
		// are NOT matched yet — precision-first, such repros fall through to
		// not_demonstrated and the agent revises (tracked in bugbot-dmy notes).
		ranMarkers: []string{
			"[  failed  ]",     // gtest per-test FAILED line + summary
			"failed test",      // gtest "N FAILED TEST(S)" tail
			"tests failed",     // ctest summary / "The following tests FAILED:"
			"assertion failed", // BSD/macOS assert(): "Assertion failed: (expr)"
			"assertion \x60",   // glibc assert(): "Assertion `expr' failed." (\x60 = backtick; expr splits the two words)
		},
		buildMarkers: []string{
			"error: ",
			"undefined reference",
			"fatal error:",
			"no such file",
		},
		toolchainMarkers: []string{
			"cmake error",
			"ctest: not found",
		},
	},
	{
		name: sandbox.EcosystemBazel,
		// Bazel reproduction IS supported: the sandbox image carries bazel,
		// vendored deps and a warm cache and runs offline. interpret() classifies
		// bazel by its authoritative EXIT CODE in a dedicated branch (exit 3 =
		// build OK, tests ran, >=1 FAILED = demonstrated). These ranMarkers are
		// only the BACKSTOP that branch consults when the exit code was lost (e.g.
		// the agent piped the output): they are bazel's test-result lines, which
		// appear ONLY on a real test failure — never on a passing run or a
		// build/analysis abort. buildMarkers/toolchainMarkers below classify the
		// non-demonstrating failures for agent feedback.
		ranMarkers: []string{
			"fails locally", // "Executed N out of M test: X fails locally."
			"failed in ",    // "//pkg:target  FAILED in 0.3s" (test-result line)
		},
		buildMarkers: []string{
			"no such target",
			"no such package",
			"build did not complete",
			"error: ",
		},
		toolchainMarkers: []string{
			"command not found",
			"bazel: not found",
		},
	},
	// Fallback: any non-zero exit lacking environment-failure markers is
	// treated as a build/toolchain failure, NEVER as a demonstration. We
	// still require explicit positive evidence (FAIL/FAILED/failed) so an
	// arbitrary shell command with no known runner does not silently
	// promote to T1.
	{
		name: sandbox.EcosystemUnknown,
		// Strong, generic "the program ran and blew up" signals ONLY. Bare
		// "failed"/"fail "/"failure" were removed: they match build/link noise
		// and even test-target names (e.g. *Failure*), which minted false T1s
		// for C/C++ repros that landed here before cmake/g++/clang were routed
		// to the cpp ecosystem. gtest's "[  failed  ]" is kept because a bare
		// `./run_tests` binary (no ctest wrapper) lands on unknown yet is still
		// dispositive ran-evidence.
		ranMarkers: []string{
			"[  failed  ]",     // gtest run via a bare ./test binary
			"assertion failed", // BSD/macOS assert()
			"assertion \x60",   // glibc assert() (\x60 = backtick)
			"assertionerror",   // Python-style AssertionError
			"panic:",
		},
		// No ecosystem-specific build markers — we don't know the
		// toolchain's error vocabulary, so we can't safely classify.
		buildMarkers: []string{},
		// shell / generic toolchain refusals land here.
		toolchainMarkers: []string{
			"command not found",
			"enoent",
		},
	},
}

// detectEcosystem picks the first ecosystemRules whose launcher regex
// matches the plan's argv. The argv may be wrapped in a shell ("bash",
// "-c", "go test ./...") — those are walked through.
//
// Unknown commands fall back to ecosystemTable["unknown"], which still
// requires positive ran-evidence (see ranMarkers for that entry). This
// preserves the bug's central invariant: a bare non-zero exit is NEVER a
// demonstration.
func detectEcosystem(argv []string) ecosystemRules {
	// Flatten a shell wrapper so we can pattern-match the inner command.
	argv = unwrapShell(argv)
	if len(argv) == 0 {
		return ecosystemTable[ecosystemIndex(sandbox.EcosystemUnknown)]
	}

	// Heuristic: the first token that looks like a known test runner
	// wins. We do not require the runner to be argv[0] because plans
	// commonly use "go test ..." directly, while CI scripts wrap things
	// in "bash -c" or invoke interpreters ("python -m pytest ...").
	first := strings.ToLower(argv[0])
	switch first {
	case "go":
		// Only classify as Go if "go test" or a related testing subcommand
		// is invoked; `go build` is a build step, not a test step.
		if len(argv) >= 2 && isGoTestSubcommand(argv[1]) {
			return ecosystemTable[ecosystemIndex(sandbox.EcosystemGo)]
		}
		// `go vet`, `go run` of a *_test.go file, etc. are still Go
		// output but not test runs. Treat as Go so unrecognized output
		// does not promote under the unknown default.
		if len(argv) >= 2 {
			return ecosystemTable[ecosystemIndex(sandbox.EcosystemGo)]
		}
	case "pytest", "py.test":
		return ecosystemTable[ecosystemIndex(sandbox.EcosystemPython)]
	case "python", "python3":
		// `python -m pytest ...` is the conventional cross-platform
		// pytest launcher. We match "pytest" or "py.test" as the module
		// name; anything else ("python script.py") falls through to
		// unknown, which still requires ran-evidence.
		if len(argv) >= 3 && argv[1] == "-m" {
			mod := strings.ToLower(argv[2])
			if mod == "pytest" || mod == "py.test" {
				return ecosystemTable[ecosystemIndex(sandbox.EcosystemPython)]
			}
		}
	case "cargo":
		// `cargo test ...` is the test invocation. `cargo build`,
		// `cargo run`, `cargo check` are build steps but still produce
		// Rust-toolchain output we want to classify as rust so unknown
		// stderr does not silently promote.
		if len(argv) >= 2 && (argv[1] == "test" || argv[1] == "bench") {
			return ecosystemTable[ecosystemIndex(sandbox.EcosystemRust)]
		}
		if len(argv) >= 2 {
			return ecosystemTable[ecosystemIndex(sandbox.EcosystemRust)]
		}
	case "npm", "yarn", "pnpm", "npx":
		// `npm test`, `yarn test`, `pnpm test`, `npx jest`, etc. All
		// land on the JS test-runner path.
		return ecosystemTable[ecosystemIndex(sandbox.EcosystemJS)]
	case "jest", "vitest", "mocha":
		return ecosystemTable[ecosystemIndex(sandbox.EcosystemJS)]
	case "cmake", "gcc", "g++", "clang", "clang++", "cc", "c++", "ctest", "meson":
		// The C/C++ build + compile + test launchers (CMake/CTest and Meson
		// included). Routing them to the cpp ecosystem means a broken build is
		// classified against cpp's toolchain ("cmake error") and build
		// ("error: ", "fatal error:", "undefined reference", "no such file")
		// markers BEFORE ran-evidence, so a configure/compile/link failure is
		// never mistaken for a demonstration. `make` and `ninja` are left on the
		// unknown path: they are generic build tools, and the not-demonstrated
		// contract for a bare non-zero exit is preserved by the tightened unknown
		// ran-markers.
		return ecosystemTable[ecosystemIndex(sandbox.EcosystemCpp)]
	case "bazel", "bazelisk":
		// Bazel is a build/test launcher. Bugbot runs `bazel test
		// --build_tests_only --test_output=errors //...` directly (see
		// repro.patch.detectSuiteCmd) and classifies any
		// `bazel` / `bazelisk` invocation as the bazel ecosystem so
		// non-zero exits surface the bazel-specific environment_error
		// summary instead of falling through to the generic unknown
		// branch. unwrapShell in the caller already walks through
		// `bash -c 'bazel test //...'` wrappers, so this case also
		// matches that form.
		return ecosystemTable[ecosystemIndex(sandbox.EcosystemBazel)]
	}
	// Fall through: unrecognized launcher. Pick the conservative
	// "unknown" entry — it still requires positive ran-evidence.
	return ecosystemTable[ecosystemIndex(sandbox.EcosystemUnknown)]
}

// unwrapShell peels off `bash -c '...'` / `sh -c '...'` style wrappers
// when the inner command is a single contiguous string. Anything more
// elaborate is left alone.
func unwrapShell(argv []string) []string {
	// argv[1] matches a shell flag cluster whose final flag is -c (e.g. "-c",
	// "-lc" for a login shell, "-ec"): bash/sh read the command string from the
	// next operand, so argv[2] is the inner command regardless of the leading
	// flags. Matching only "-c" missed the common `bash -lc '...'` form.
	if len(argv) >= 3 && (argv[0] == "bash" || argv[0] == "sh" || argv[0] == "/bin/bash" || argv[0] == "/bin/sh") &&
		strings.HasPrefix(argv[1], "-") && strings.HasSuffix(argv[1], "c") {
		// Split the inner command on whitespace to recover the actual
		// launcher tokens. This is best-effort: quoted args with
		// embedded spaces will be mis-tokenized, but for the purpose
		// of ecosystem detection the leading token is enough to pick
		// the right runner.
		return strings.Fields(argv[2])
	}
	return argv
}

// ecosystemIndex returns the position of the named entry in
// ecosystemTable, or 0 if the name is unknown (a defensive default — the
// go entry MUST be at index 0, see the table comment).
func ecosystemIndex(name sandbox.Ecosystem) int {
	for i, e := range ecosystemTable {
		if e.name == name {
			return i
		}
	}
	return 0
}

// isGoTestSubcommand reports whether s is one of the Go subcommands
// that runs tests and produces testing-package output (`--- FAIL`,
// `FAIL\t...`, `panic:`, etc.). Anything else (`go build`, `go vet`,
// `go run`) is still classified as Go by the caller — the distinction
// matters for the Go build-marker list, which assumes the Go toolchain.
func isGoTestSubcommand(s string) bool {
	switch s {
	case "test", "vet", "run", "tool", "generate":
		return true
	}
	return false
}

// hasRanEvidence reports whether out contains at least one of the
// ecosystem's positive ran-markers. Matching is case-insensitive
// substring match. A nil rules pointer (or a rules value with no
// ranMarkers) returns false — unknown ecosystems therefore NEVER
// demonstrate on a bare non-zero exit, satisfying the central
// invariant of bugbot-vig.
func (e *ecosystemRules) hasRanEvidence(out string) bool {
	if e == nil {
		return false
	}
	low := strings.ToLower(out)
	for _, m := range e.ranMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// hasAnyMarker reports whether out contains any of the given
// (lowercase) substrings. Used for the build / toolchain / env-failure
// classifications.
func hasAnyMarker(out string, markers []string) bool {
	low := strings.ToLower(out)
	for _, m := range markers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

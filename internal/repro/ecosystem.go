package repro

import "strings"

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
	// name is the lowercase identifier (e.g. "go", "python", "unknown").
	name string
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

// ecosystemTable is the ordered registry of supported ecosystems. Order
// matters only for tie-breaking in the (rare) case that the same argv
// prefix matches two entries; the first match wins. Go is intentionally
// first to keep the legacy Go verdicts bit-for-bit compatible.
var ecosystemTable = []ecosystemRules{
	{
		name: "go",
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
		name: "python",
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
		name: "rust",
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
		name: "js",
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
		name: "cpp",
		ranMarkers: []string{
			"failed",
			"fail",
			"assertion failed",
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
	// Fallback: any non-zero exit lacking environment-failure markers is
	// treated as a build/toolchain failure, NEVER as a demonstration. We
	// still require explicit positive evidence (FAIL/FAILED/failed) so an
	// arbitrary shell command with no known runner does not silently
	// promote to T1.
	{
		name: "unknown",
		ranMarkers: []string{
			"failed",
			"fail ",
			"failure",
			"assertion failed",
			"assertionerror",
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
		return ecosystemTable[ecosystemIndex("unknown")]
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
			return ecosystemTable[ecosystemIndex("go")]
		}
		// `go vet`, `go run` of a *_test.go file, etc. are still Go
		// output but not test runs. Treat as Go so unrecognized output
		// does not promote under the unknown default.
		if len(argv) >= 2 {
			return ecosystemTable[ecosystemIndex("go")]
		}
	case "pytest", "py.test":
		return ecosystemTable[ecosystemIndex("python")]
	case "python", "python3":
		// `python -m pytest ...` is the conventional cross-platform
		// pytest launcher. We match "pytest" or "py.test" as the module
		// name; anything else ("python script.py") falls through to
		// unknown, which still requires ran-evidence.
		if len(argv) >= 3 && argv[1] == "-m" {
			mod := strings.ToLower(argv[2])
			if mod == "pytest" || mod == "py.test" {
				return ecosystemTable[ecosystemIndex("python")]
			}
		}
	case "cargo":
		// `cargo test ...` is the test invocation. `cargo build`,
		// `cargo run`, `cargo check` are build steps but still produce
		// Rust-toolchain output we want to classify as rust so unknown
		// stderr does not silently promote.
		if len(argv) >= 2 && (argv[1] == "test" || argv[1] == "bench") {
			return ecosystemTable[ecosystemIndex("rust")]
		}
		if len(argv) >= 2 {
			return ecosystemTable[ecosystemIndex("rust")]
		}
	case "npm", "yarn", "pnpm", "npx":
		// `npm test`, `yarn test`, `pnpm test`, `npx jest`, etc. All
		// land on the JS test-runner path.
		return ecosystemTable[ecosystemIndex("js")]
	case "jest", "vitest", "mocha":
		return ecosystemTable[ecosystemIndex("js")]
	case "ctest":
		return ecosystemTable[ecosystemIndex("cpp")]
	}
	// Fall through: unrecognized launcher. Pick the conservative
	// "unknown" entry — it still requires positive ran-evidence.
	return ecosystemTable[ecosystemIndex("unknown")]
}

// unwrapShell peels off `bash -c '...'` / `sh -c '...'` style wrappers
// when the inner command is a single contiguous string. Anything more
// elaborate is left alone.
func unwrapShell(argv []string) []string {
	if len(argv) >= 3 && (argv[0] == "bash" || argv[0] == "sh" || argv[0] == "/bin/bash" || argv[0] == "/bin/sh") && argv[1] == "-c" {
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
// "go" entry MUST be at index 0, see the table comment).
func ecosystemIndex(name string) int {
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

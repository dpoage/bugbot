package repro

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/util"
)

// jsTSGuidance is shared by JavaScript and TypeScript, which use the same test
// runners.
const jsTSGuidance = "For JavaScript/TypeScript, write a\n" +
	"  *.test.(js|ts) file using the repository's existing test runner and run\n" +
	"  just it, e.g. " + "`npx vitest run <file>`" + " or " + "`npm test -- -t <name>`" + "."

// genericGuidance is the fallback for languages with no specific entry, so a
// repro is still attemptable in a language we have no guidance for.
const genericGuidance = "Use the repository's standard test framework for its language: write the\n" +
	"  smallest test in the conventional location and run just that test."

// cmakeGuidance is the C/C++ guidance for repos whose root carries a
// CMakeLists.txt. CMake+CTest is the only C/C++ toolchain with an unambiguous
// test entry point (ctest --test-dir), so it is the only tier that earns
// specific guidance; GoogleTest and Catch2 both register tests through CTest.
//
// For memory-safety findings (leaks, use-after-free, overflows): configure with
// -fsanitize=address -g so the sanitizer report aborts non-zero — that report
// IS the demonstration. For uninitialized-read findings: prefer
// -fsanitize=memory (clang) or valgrind --error-exitcode=1.
const cmakeGuidance = "For C/C++ with CMake, configure and build first:\n" +
	"  " + "`cmake -B build -S . && cmake --build build`" + "\n" +
	"  then run the new or relevant test via CTest:\n" +
	"  " + "`ctest --test-dir build -R <TestName> --output-on-failure`" + "\n" +
	"  or execute the test binary directly (e.g. " + "`./build/tests/<TestBinary>`" + ").\n" +
	"  GoogleTest targets are conventionally under tests/ or test/; Catch2 targets\n" +
	"  follow the same layout.\n" +
	"  For memory-safety findings (leaks, use-after-free, overflows): add\n" +
	"  `-DCMAKE_CXX_FLAGS=\"-fsanitize=address -g\"` to the cmake configure step\n" +
	"  so AddressSanitizer/LeakSanitizer aborts non-zero — the sanitizer report\n" +
	"  IS the demonstration. For data-race / concurrency findings (a bug that\n" +
	"  only manifests when threads run concurrently, e.g. a use-after-free that\n" +
	"  races with cleanup): use `-DCMAKE_CXX_FLAGS=\"-fsanitize=thread -g\"`\n" +
	"  instead — ThreadSanitizer reports the race deterministically, whereas a\n" +
	"  single AddressSanitizer run only fails if the use-after-free window\n" +
	"  happens to fire. For uninitialized-read findings: use\n" +
	"  `-DCMAKE_CXX_FLAGS=\"-fsanitize=memory\"` (clang) or run the binary under\n" +
	"  `valgrind --error-exitcode=1`. TSan, ASan and MSan cannot be combined in\n" +
	"  one build — pick the one matching the bug class."

// mesonGuidance is the C/C++ guidance for repos whose root carries a
// meson.build but no CMakeLists.txt. Meson exposes a single test entry point
// (`meson test`) so repro is unambiguous.
//
// For memory-safety findings: use -Db_sanitize=address to enable
// AddressSanitizer/LeakSanitizer so the sanitizer report aborts non-zero.
const mesonGuidance = "For C/C++ with Meson, set up and build first:\n" +
	"  " + "`meson setup build && meson compile -C build`" + "\n" +
	"  then run the specific test by name:\n" +
	"  " + "`meson test -C build <TestName>`" + ".\n" +
	"  For memory-safety findings (leaks, use-after-free, overflows): configure\n" +
	"  with `-Db_sanitize=address` so AddressSanitizer/LeakSanitizer aborts\n" +
	"  non-zero — the sanitizer report IS the demonstration. For data-race /\n" +
	"  concurrency findings (a bug that only manifests when threads run\n" +
	"  concurrently): use `-Db_sanitize=thread` instead — ThreadSanitizer\n" +
	"  reports the race deterministically, whereas a single AddressSanitizer run\n" +
	"  only fails if the use-after-free window happens to fire. For uninitialized\n" +
	"  reads: use `-Db_sanitize=memory` (clang) or valgrind --error-exitcode=1.\n" +
	"  TSan, ASan and MSan cannot be combined in one build."

// mavenGuidance is the Java/Kotlin guidance for repos whose root carries a
// pom.xml. Maven's Surefire plugin registers JUnit tests so the run command
// is unambiguous: identify the class and method to minimise the test run.
const mavenGuidance = "For Java/Kotlin with Maven, add a\n" +
	"  " + "`@Test`" + " method (JUnit 5) in the relevant test class and run just that\n" +
	"  test with\n" +
	"  " + "`mvn test -Dtest=ClassName#method`" + "."

// gradleGuidance is the Java/Kotlin guidance for repos whose root carries a
// Gradle build file but no pom.xml. Gradle's test filter syntax routes to a
// fully-qualified class-and-method pattern.
const gradleGuidance = "For Java/Kotlin with Gradle, add a\n" +
	"  " + "`@Test`" + " method (JUnit 5) in the relevant test class and run just that\n" +
	"  test with\n" +
	"  " + "`gradle test --tests <ClassName.method>`" + "."

// specificGuidance is the per-language test-framework guidance spliced into
// the reproducer system prompt. It is the single source of truth for which
// build-system-independent languages have specific guidance: langGuidance reads
// the text from it and HasGuidance reports membership, so the two cannot drift.
// The Go text is verbatim from the original prompt; the others give the
// idiomatic test file + run command for each ecosystem.
//
// C/C++ and Java/Kotlin are build-system-dependent and are handled in the
// langGuidance switch (like C/C++ with cmakeGuidance/mesonGuidance); they do
// not appear here. C# uses `dotnet test` uniformly regardless of build system,
// so it is build-system-independent and belongs in this map.
var specificGuidance = map[ingest.Language]string{
	ingest.LangGo: "For Go, write a\n" +
		"  *_test.go file in the package that contains the bug and run it with\n" +
		"  " + "`go test -run <TestName> ./<pkg>`" + " (or the module path that targets it).\n" +
		"  The harness appends -json to a direct `go test` argv automatically; do not\n" +
		"  rely on any particular human-readable console output shape.",
	ingest.LangPython: "For Python, write a\n" +
		"  test_*.py file (pytest style) next to or under the package with the bug\n" +
		"  and run it with " + "`pytest -k <test_name> <path>`" + ".\n" +
		"  The harness appends --junitxml to a direct pytest argv automatically; do not\n" +
		"  rely on any particular human-readable console output shape.",
	ingest.LangJavaScript: jsTSGuidance,
	ingest.LangTypeScript: jsTSGuidance,
	ingest.LangRust: "For Rust, add a\n" +
		"  " + "`#[test]`" + " function (in the crate with the bug or a tests/ file) and run it\n" +
		"  with " + "`cargo test <test_name>`" + ".",
	ingest.LangCSharp: "For C#, add a\n" +
		"  test method annotated with " + "`[Fact]`" + " (xUnit) or " + "`[Test]`" + " (NUnit) in the\n" +
		"  relevant test project and run it with\n" +
		"  " + "`dotnet test --filter <Name>`" + ".",
	ingest.LangZig: "For Zig, add a\n" +
		"  test block in the file with the bug and run it with\n" +
		"  " + "`zig build test`" + ".",
	ingest.LangGleam: "For Gleam, add a\n" +
		"  test function under test/ and run it with " + "`gleam test`" + ".",
	ingest.LangElixir: "For Elixir, add an\n" +
		"  ExUnit test under test/ and run it with " + "`mix test`" + ".",
}

// HasGuidance reports whether lang has language-specific repro guidance in
// langGuidance (as opposed to the generic fallback). Doctor and the setup
// wizard call this to warn when a dominant language lacks specific guidance,
// reading the live table rather than maintaining a parallel list.
//
// systems is variadic so callers that do not yet track build systems (e.g. the
// existing doctor check) can pass nothing and still compile. When systems are
// provided, C/C++ repos with CMake or Meson are considered to have guidance,
// and Java/Kotlin repos with Maven or Gradle are considered to have guidance.
func HasGuidance(lang ingest.Language, systems ...ingest.BuildSystem) bool {
	return langGuidance(lang, systems) != genericGuidance
}

// langGuidance returns the test-framework guidance spliced into the reproducer
// system prompt for the finding's language. For C/C++ the result depends on
// which build systems were detected: cmake earns specific guidance, then meson,
// then the generic fallback (raw make/ninja or nothing). For Java/Kotlin the
// result depends on build systems too: maven earns specific guidance, then
// gradle, then the generic fallback. For all other languages the specificGuidance
// map is the sole source of truth; systems is ignored for those.
func langGuidance(lang ingest.Language, systems []ingest.BuildSystem) string {
	switch lang {
	case ingest.LangC, ingest.LangCPP:
		for _, s := range systems {
			if s == ingest.BuildSystemCMake {
				return cmakeGuidance
			}
		}
		for _, s := range systems {
			if s == ingest.BuildSystemMeson {
				return mesonGuidance
			}
		}
		return genericGuidance
	case ingest.LangJava, ingest.LangKotlin:
		for _, s := range systems {
			if s == ingest.BuildSystemMaven {
				return mavenGuidance
			}
		}
		for _, s := range systems {
			if s == ingest.BuildSystemGradle {
				return gradleGuidance
			}
		}
		return genericGuidance
	}
	if g, ok := specificGuidance[lang]; ok {
		return g
	}
	return genericGuidance
}

// systemPrompt instructs the reproducer agent to produce a MINIMAL,
// assertion-bearing failing test for the finding. The emphasis is that the
// repro must fail *because of the bug* and would pass if the bug were fixed —
// not merely crash, and not fail to compile. The lang argument selects the
// language-specific test-framework guidance (see langGuidance); systems
// refines that selection for C/C++ (cmake > meson > generic fallback).
// caps enumerates the probed capability modes; the prompt instructs the agent
// to avoid modes that are unavailable in the image. pb is the per-repo
// verified-command playbook (bugbot-u2v5, playbook.go's playbookGuidance);
// an empty Playbook adds nothing.
func systemPrompt(lang ingest.Language, systems []ingest.BuildSystem, caps sandbox.CapabilitySet, pb Playbook) string {
	return `You are Bugbot's reproducer agent. Your job is to write a MINIMAL test that
demonstrates a specific, already-verified bug by FAILING because of it.

You have read-only tools (read_file, list_dir, grep) rooted at the target
repository. Investigate the finding's file, line, and reasoning first, then
produce a repro plan.

Hard requirements for the repro:
- STRONGLY PREFER a test wrapped in the repo's standard test framework run
  through its standard runner (gtest, ` + "`go test`" + `, pytest, cargo test,
  jest/vitest). Use the repo's standard runner; the harness verifies the run
  independently. This is the default and should be your first choice whenever
  the bug class supports it.
- The test MUST FAIL (exit non-zero) on the CURRENT, buggy code, and MUST PASS
  once the bug is fixed. Encode the bug as an explicit assertion: call the
  buggy code and assert the CORRECT expected result, so the wrong current
  behavior makes the assertion fail. Do NOT write a test that merely triggers a
  panic or crash without an assertion unless the panic itself is the bug being
  demonstrated and the test asserts it should not panic. For sanitizer-detected
  memory or race bugs the sanitizer report replaces the explicit assertion: the
  sanitizer output and non-zero exit IS the demonstration.
- ESCAPE HATCH (FALLBACK ONLY) for non-runtime bug classes that genuinely have
  no standard test-framework runner — e.g. build-system/config bugs, shader /
  asset semantics, header-only or macro-only changes — wrap the assertion in a
  short bash script or a bare compiled binary (` + "`cmake/g++/clang++`" + `). Make the
  script/binary print the literal token ` + "`" + reproSentinelDemonstrated + "`" + ` to
  stdout ONLY on the code path that confirms the bug is present, immediately
  before exiting non-zero; print nothing (and exit 0) when the bug is absent.
  Use this ONLY when there is no realistic test framework; do NOT default to it.
- The repro command's real exit status and output must reach bugbot: make the
  test (or sanitized binary) the FINAL command in cmd. Do NOT append a trailing
  "| tail", "| head", or "; echo ..." after it — a pipeline's exit status is
  its last stage's and a trailing echo always exits 0, so either silently masks
  a real failure as a clean exit. Piping an EARLIER build step to trim its log
  is fine.
- Keep it minimal: the smallest test that exercises the bug. Do not add new
  dependencies. Use only the standard library and what the repository already
  imports. The test must COMPILE — a compile error or missing dependency is NOT
  a reproduction and will be rejected.
- Self-terminating (TEST phase): the test command MUST finish on its own within
  seconds — never read from stdin, sleep unboundedly, wait on the network or a
  port, or deadlock; a hung test is idle-killed and the attempt is wasted with
  no real verdict. Build steps may take minutes; the idle watchdog handles
  build hangs. Bound the TEST command: use ` + "`go test -timeout 60s`" + ` (built-in);
  wrap other runners with ` + "`timeout 60 <runner>`" + ` (coreutils); for
  jest/vitest use ` + "`--testTimeout 60000`" + `.
` + capabilityGuidance(caps) + playbookGuidance(pb) + `
Language-specific guidance:
` + langGuidance(lang, systems) + `
Return a repro plan describing the files to inject, the command to run them,
and a short description of the expected failure.`
}

// bazelGuidance is appended to the reproducer system prompt when the repo is a
// Bazel monorepo (BuildSystemBazel in r.buildSystems). It steers the agent to
// a TARGETED or DIRECT strategy — never `bazel test //...` — and pre-commits
// it to the bazel exit-code contract (exit 3 is the demonstration) so the
// reproducer plans around it instead of treating exit 3 as a sandbox failure.
//
// The wrapped leading newline matches the other appended guidance sections
// (pkgContextGuidance, reproSandboxGuidance): each section begins with one
// blank line so the concatenated prompt reads as discrete paragraphs.
func bazelGuidance() string {
	return `

BAZEL MONOREPO — reproduction strategy (this repo builds with Bazel):
- PREFER A DIRECT RUN when the bug is in a directly-runnable entrypoint (a script or a built binary). Example: a Python CLI bug reproduces with ` + "`python3 path/to/tool.py <args>`" + ` — a traceback or non-zero exit is a clean demonstration and does NOT require Bazel at all.
- OTHERWISE write a MINIMAL failing test (cc_test / py_test / rust_test) in the relevant package's BUILD file and run ONLY that one target. Pass run_tests a specific target via pkg, e.g. pkg="//common/tests:my_repro_test". NEVER run ` + "`//...`" + ` — it builds the whole repo and burns your limited run_tests budget.
- Bazel exit codes you MUST plan around:
    exit 0 = built and all tests PASSED → NOT a reproduction.
    exit 3 = built and at least one TEST FAILED → THIS is a valid reproduction. Aim for exit 3.
    exit 1 = build/analysis failed, or no such target → NOT a reproduction; fix your target or test.
    exit 4 = no tests found → your target pattern is wrong.
- Always add ` + "`--test_output=errors`" + ` so the failing test's output is captured.
- Before spending a run on a ` + "`bazel test //pkg:target`" + `, confirm the target label exists by reading that package's BUILD file. The sandbox is offline (network=none) with vendored deps and bazel preinstalled, so targeted ` + "`bazel test //pkg:target`" + ` works.
- Benign ` + "`WARNING: ... (Read-only file system)`" + ` lines about the bazel disk cache are EXPECTED noise; ignore them — they are not an environment failure.
`
}

// pkgContextGuidance is appended to the reproducer system prompt when the
// get_package_context tool is wired. It steers the agent to pull the repo's
// test-package summary for build/test orientation before planning, so it does
// not burn its whole tool budget rediscovering how the repo builds and runs
// tests (the dominant failure mode on large C/C++ repos).
const pkgContextGuidance = `

You also have get_package_context: it returns a cached one-paragraph summary of
any package (a repo-relative directory) describing its purpose, invariants, and
build/test layout. Use it to orient FAST instead of reading many files. Before
proposing your plan, look up the repository's TEST package (e.g. "test",
"tests", or the directory that holds the existing test target) to learn how
tests are built and run here: the test runner/binary, where to place a new test
file, and whether the build fetches dependencies at configure time.`

// runTestsGuidance is appended to the reproducer system prompt when the
// run_tests tool is wired. It tells the agent it may call run_tests up to
// maxExec times to verify the toolchain and learn the repo's test layout
// before writing its repro plan — and that run_tests MUST NOT serve as the
// demonstration itself (the repro must still be a new minimal failing test).
func runTestsGuidance(maxExec int) string {
	return fmt.Sprintf(`

You also have run_tests: it runs the repository's EXISTING test suite inside
the sandbox (network-none, offline). You MAY call it up to %d time(s) to
confirm the toolchain is working and to learn the repo's test layout and build
conventions BEFORE proposing your repro plan. Typical uses: verify the suite
compiles and runs, inspect which tests exist, confirm a sanitizer flag works.

NOTE: run_tests always runs OFFLINE (network-none). Your final repro plan runs
with the configured network, so a run_tests failure at the dependency-fetch or
configure step (e.g. cmake FetchContent) is EXPECTED offline and does NOT mean
the repro path is broken — proceed with your plan.

IMPORTANT: run_tests executes the repo's pre-existing tests — it is NOT a way
to demonstrate the bug. Your repro plan MUST still inject a NEW minimal failing
test (via the plan's files map) and run it with a targeted cmd. Do NOT use
run_tests as the demonstration or include it in cmd.`, maxExec)
}

// workspaceGuidance is appended to the reproducer system prompt when the
// workspace tool set (write_repro_file, delete_repro_file, workspace) is
// wired. It is an ORDERED workflow, not a feature list — bugbot-jto7's live
// dogfood evidence showed an agent burning its ENTIRE exec budget on
// environment probes before ever writing a candidate file, so the ordering
// here (investigate -> orient in the sandbox with FREE applets -> author ->
// loop the BUDGETED exec -> submit) is deliberate, not decorative. It also
// spells out each tool's argument shape (live runs showed agents guessing
// files-as-array and empty-files calls when the contract was only implied),
// and states plainly that promotion is judged by an INDEPENDENT re-run of
// the final plan in a fresh workspace — containing the repo plus exactly
// the tracked files — so command side effects from iteration (including
// workspace exec's) can never substitute for the plan itself demonstrating
// the bug. bugbot-0wvg extended the FREE applet step with grep/find
// (content search and filename lookup) after dogfood measurement found 53%
// of budgeted exec calls were read-only probes ls/cat could not cover.
func workspaceGuidance(maxExec int) string {
	return fmt.Sprintf(`

You have a persistent WORKSPACE for this attempt (a fresh copy of the repo).
Work it as an ORDERED loop:

1. INVESTIGATE with your read-only tools (read_file, list_dir, grep). They
   are rooted at the HOST repository, NOT the sandbox: any gitignored
   build/vendor artifacts you see there do NOT exist in the sandbox checkout
   your commands actually run against — do not assume they do.
2. ORIENT IN THE SANDBOX using the workspace tool's FREE applets before
   spending any exec budget:
   - workspace {"argv": ["ls", "<dir>"]} lists a workspace-relative
     directory (dir defaults to "."). FREE.
   - workspace {"argv": ["cat", "<file>"]} shows a workspace-relative
     file's tail. FREE.
   - workspace {"argv": ["grep", "<pattern>", "<dir>"]} searches file
     contents under a workspace-relative directory for a regexp, returning
     'path:line:text' matches. FREE.
   - workspace {"argv": ["find", "<glob-or-substring>", "<dir>"]} locates
     workspace-relative paths by filename. FREE.
   - workspace {"argv": ["status"]} reports whether the workspace is
     materialized, your tracked files, and your exec budget used/remaining.
     FREE.
   These four are how you LOOK at the sandbox: exec is BUDGETED and is for
   RUNNING code, never for probing it — do not spend exec on "ls", "cat",
   "grep", or "find" invocations; use the free applets instead.
   Also prefer get_package_context / run_tests (where wired) over spending
   exec budget: they orient you on the build/test layout for free or out of
   a separate, smaller budget. Reach for workspace {"argv": ["exec", ...]}
   only for what those cannot show you (e.g. confirming a toolchain flag
   actually works in THIS sandbox).
3. AUTHOR your candidate with write_repro_file {"path": "<repo-relative
   path>", "contents": "<full file contents>"} — writes ONE NEW file.
   Calling it again with the same path replaces the file — that is how you
   edit. Writing is free (no budget). You cannot overwrite a file that
   already exists in the repository. delete_repro_file {"path": ...}
   removes a file you wrote earlier (e.g. a broken helper that would
   poison the final build).
4. LOOP: workspace {"argv": ["exec", "<argv...>"]} runs a command against
   the workspace and reports the same demonstrated/reason classification
   the final verdict uses. You may exec up to %d time(s) total; malformed
   calls, unknown applets, and invalid commands are rejected WITHOUT
   consuming the budget, and writes never consume it. Read the
   classification and output, edit your candidate with write_repro_file,
   and re-exec until it reports demonstrated=true FOR THE REASON the
   finding describes. Exec output is automatically tail-capped for you —
   NEVER pipe your test runner through tail/head/grep to bound it
   yourself, that discards the runner's exit code and can turn a real
   failure into a false "passed" (a bash -c pipeline defensively gets
   pipefail injected, but write clean commands and do not rely on it).
5. SUBMIT the EXACT cmd that demonstrated the bug. Every file you wrote
   (and did not delete) is automatically included in your final plan — the
   workspace IS the proof; the plan's "files" field is an optional overlay,
   you do not need to repeat file contents in it. The official verdict
   re-runs your cmd in a BRAND-NEW workspace containing the repo plus
   exactly those tracked files: build artifacts and any other side effects
   of your exec runs are NOT carried over — a file an exec command merely
   CREATED (e.g. via shell redirection) is NOT tracked or submitted, only
   files written via write_repro_file are — so cmd must perform any build
   steps itself, exactly as submitted.`, maxExec)
}

// reproSandboxGuidance renders the sandbox-environment + command-hygiene section
// appended to every reproducer system prompt. It encodes the realities that
// caused observed repro failures: the sandbox is a clean git checkout (gitignored
// build/vendor artifacts the agent sees in its read-only host view are NOT
// present), any operator bind mounts are listed so the agent uses them instead of
// those absent paths, and the command-construction rules prevent the recurring
// malformed-command failures (missing `bash -c`, a test filter passed to cmake,
// a self-overwriting `-o`, the dependency step disabled, placeholder commands).
func reproSandboxGuidance(mounts []sandbox.ROMount) string {
	var b strings.Builder
	b.WriteString(`

Sandbox environment & command hygiene:
- The sandbox holds the repository's git-tracked files (a clean checkout).
  Generated, gitignored paths (build/, .vendor/, node_modules/, ...) are NOT
  present — build or fetch what the test needs as part of cmd; never reference a
  gitignored path you saw in your read-only view but that the checkout omits.
- Prefer the repository's own build/test entrypoint (a Makefile target, CMake
  preset, or test script — discover it via get_package_context or the build
  files) over hand-rolled compiler/cmake flags. Do NOT disable the project's
  dependency or vendor step (e.g. -DBUILD_VENDOR=OFF): the build needs it to
  resolve dependencies.
- Wrap any multi-step command (using &&, ||, |, or redirects) in bash -c "...":
  a bare argv is run directly, so shell operators and later commands would be
  passed as arguments to the first program.
- Pass a test filter (e.g. --gtest_filter) to the TEST BINARY, never to cmake or
  make.
- A single-file compile's -o output path MUST differ from every input file.
- Always return a real command that builds and runs the test; never a placeholder.`)
	if len(mounts) > 0 {
		b.WriteString("\n- These read-only host directories are bind-mounted into the sandbox;\n  reference them instead of gitignored paths:")
		for _, m := range mounts {
			fmt.Fprintf(&b, "\n    %s", m.ContainerPath)
		}
	}
	return b.String()
}

// capabilityGuidance renders the capability-constraint section of the system
// prompt. When caps is nil or empty, nothing is added. When capabilities are
// known, the prompt enumerates available and unavailable modes so the agent
// never proposes an invocation the image cannot run.
func capabilityGuidance(caps sandbox.CapabilitySet) string {
	if len(caps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nSandbox capability constraints (probed for this image):\n")

	// Go ecosystem capabilities.
	if goCaps, ok := caps["go"]; ok {
		if goCaps["race"] {
			b.WriteString("- Go race detector: AVAILABLE. You MAY use `go test -race` to expose data-race bugs.\n")
		} else {
			b.WriteString("- Go race detector: UNAVAILABLE (no cgo / C compiler in this image).\n")
			b.WriteString("  Do NOT use `go test -race`. Use a deterministic assertion-based test instead.\n")
		}
	}

	// C/C++ ecosystem capabilities.
	if cppCaps, ok := caps["cpp"]; ok {
		// TSan: ThreadSanitizer for data-race findings.
		if cppCaps["tsan"] {
			b.WriteString("- C++ ThreadSanitizer (TSan): AVAILABLE. You MAY use `-fsanitize=thread -g` to expose\n")
			b.WriteString("  data-race bugs. Force concurrent entry points / repeat races; set\n")
			b.WriteString("  TSAN_OPTIONS=halt_on_error=1 so the first race aborts non-zero. The line\n")
			b.WriteString("  `WARNING: ThreadSanitizer: data race` in the output IS the demonstration.\n")
			b.WriteString("  Note: TSan and ASan cannot be combined in one build.\n")
		} else {
			b.WriteString("- C++ ThreadSanitizer (TSan): UNAVAILABLE in this image.\n")
			b.WriteString("  Do NOT use `-fsanitize=thread`. Use a deterministic assertion-based test\n")
			b.WriteString("  that encodes the race condition via explicit ordering (e.g. mutexes or\n")
			b.WriteString("  sequenced calls) to show the wrong result without a real race detector.\n")
		}
		// ASan: AddressSanitizer for memory leaks / use-after-free.
		if cppCaps["asan"] {
			b.WriteString("- C++ AddressSanitizer (ASan): AVAILABLE. You MAY use `-fsanitize=address` to expose\n")
			b.WriteString("  memory leaks, use-after-free, and out-of-bounds access. The sanitizer report\n")
			b.WriteString("  aborts non-zero and IS the demonstration.\n")
			b.WriteString("  Note: TSan and ASan cannot be combined in one build.\n")
		} else {
			b.WriteString("- C++ AddressSanitizer (ASan): UNAVAILABLE in this image.\n")
			b.WriteString("  Do NOT use `-fsanitize=address`. Use a deterministic assertion-based test\n")
			b.WriteString("  (e.g. assert the returned pointer is non-null, or check a reference count)\n")
			b.WriteString("  to show the memory-safety bug without a real sanitizer.\n")
		}
	}

	// Rust ecosystem capabilities.
	if rustCaps, ok := caps["rust"]; ok {
		if rustCaps["cargo"] {
			b.WriteString("- Rust cargo test: AVAILABLE. You MAY use `cargo test`.\n")
		} else {
			b.WriteString("- Rust cargo test: UNAVAILABLE (no cargo/rustc in this image).\n")
			b.WriteString("  Do NOT propose `cargo` invocations.\n")
		}
		if rustCaps["miri"] {
			b.WriteString("- Rust Miri: AVAILABLE. You MAY use `cargo miri test` to expose UB/data races;\n")
			b.WriteString("  a Miri abort IS the demonstration.\n")
		} else if rustCaps["cargo"] {
			b.WriteString("- Rust Miri: UNAVAILABLE in this image.\n")
			b.WriteString("  Do NOT use `cargo miri`; use a deterministic assertion-based `cargo test`.\n")
		}
	}

	// JavaScript ecosystem capabilities.
	if jsCaps, ok := caps["js"]; ok {
		if jsCaps["node"] {
			b.WriteString("- Node.js runtime: AVAILABLE. You MAY run JS/TS repros with node.\n")
		} else {
			b.WriteString("- Node.js runtime: UNAVAILABLE in this image.\n")
			b.WriteString("  Do NOT propose node/jest/vitest invocations.\n")
		}
		if jsCaps["node_test"] {
			b.WriteString("- Node built-in test runner (`node --test`): AVAILABLE (node >= 18).\n")
			b.WriteString("  You MAY use `node --test` for a dependency-free repro.\n")
		} else if jsCaps["node"] {
			b.WriteString("- Node built-in test runner (`node --test`): UNAVAILABLE (node < 18).\n")
			b.WriteString("  Use an assert-based script via `node script.js` (non-zero exit on failure).\n")
		}
	}

	// Python ecosystem capabilities.
	if pyCaps, ok := caps["python"]; ok {
		if pyCaps["pytest"] {
			// Suggest --timeout only when the pytest-timeout plugin was confirmed
			// by the capability probe; the plugin is almost never installed in
			// offline sandboxes, and passing --timeout without it yields an
			// "unrecognized arguments" error that wastes the attempt (bugbot-v9d6).
			if pyCaps["pytest_timeout"] {
				b.WriteString("- Python pytest: AVAILABLE. You MAY use `pytest --timeout=60` (pytest-timeout plugin confirmed).\n")
			} else {
				b.WriteString("- Python pytest: AVAILABLE. You MAY use `timeout 60 pytest` to bound the test run\n")
				b.WriteString("  (pytest-timeout plugin not confirmed; use coreutils timeout wrapper instead).\n")
			}
		} else {
			b.WriteString("- Python pytest: UNAVAILABLE in this image.\n")
			if pyCaps["python"] {
				b.WriteString("  Use `python3 -m unittest` or an assert-based script via `python3`\n")
				b.WriteString("  (non-zero exit on failure).\n")
			} else {
				b.WriteString("  No Python interpreter detected; do NOT propose Python invocations.\n")
			}
		}
	}

	// Bazel build driver. Rendered so an agent on a bazel-built repo learns
	// BEFORE planning whether `bazel test` can run at all (bugbot-rj3z);
	// without this the natural first plan on such repos is a bazel
	// invocation that dies at exec time.
	if bzCaps, ok := caps["bazel"]; ok {
		switch {
		case bzCaps["bazel"]:
			b.WriteString("- Bazel: AVAILABLE. You MAY use `bazel test` for targets that need it, but a\n")
			b.WriteString("  direct language test runner is usually faster and more reliable offline.\n")
		case bzCaps["bazelisk"]:
			// The launcher works only under the bazelisk NAME: a bare
			// `bazel` argv dies at exec time (bugbot-4z7m).
			b.WriteString("- Bazel: available ONLY via the `bazelisk` launcher — a bare `bazel` argv will\n")
			b.WriteString("  fail (no binary of that name). Invoke `bazelisk test`/`bazelisk build` if a\n")
			b.WriteString("  target needs it; a direct language test runner is usually faster and more\n")
			b.WriteString("  reliable offline.\n")
		default:
			b.WriteString("- Bazel: UNAVAILABLE in this sandbox, even though the repo may be bazel-built.\n")
			b.WriteString("  Do NOT propose `bazel`/`bazelisk` invocations. Run the language's test runner\n")
			b.WriteString("  directly instead (pytest, node, go test, cargo).\n")
		}
	}

	return b.String()
}

// planSchema is the JSON schema for the reproducer agent's plan output. Only
// cmd is required: files may legitimately be absent when the agent built its
// repro in the iteration workspace (Attempt merges every write_repro_file'd
// file into the plan before validation — the workspace is the proof), and
// when the workspace tools are not wired, validatePlan's recoverable
// "no repro files" feedback handles an empty plan better than a hard schema
// failure. expect is descriptive-only (artifact README, patch context, human
// review) so it is requested but not required — a model that produces a
// runnable plan must not have the whole attempt aborted with a hard parse
// error merely for omitting the prose description.
var planSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "files": {
      "type": "object",
      "description": "Repro/test files to inject, keyed by a workspace-relative path INSIDE the repo (e.g. \"repro_test.cpp\" or \"test/repro_test.cpp\") mapping to FULL file contents. May be OMITTED when you wrote your files via write_repro_file — every workspace file is submitted automatically, and entries here are applied on top. Absolute or escaping paths like \"/tmp/foo.cpp\" are REJECTED — write injected sources into the repo tree. You MAY still emit build outputs to /tmp at run time via cmd (e.g. -o /tmp/repro). For Go, typically one _test.go file.",
      "additionalProperties": {"type": "string"}
    },
    "cmd": {
      "type": "array",
      "description": "Argv to run the repro, e.g. [\"go\",\"test\",\"-timeout\",\"60s\",\"-run\",\"TestX\",\"./pkg\"].",
      "items": {"type": "string"},
      "minItems": 1
    },
    "expect": {
      "type": "string",
      "minLength": 1,
      "description": "Short description of the expected failure (what assertion fails and why)."
    }
  },
  "required": ["cmd"],
  "additionalProperties": false
}`)

// planFor asks the agent for a repro plan for finding. feedback, when
// non-empty, is appended to steer a revision after a prior non-demonstrating
// attempt.
//
// prev, when non-nil, is the previous round's Outcome: planFor then uses
// [agent.Runner.RunJSONContinue] instead of RunJSON, so this round's request
// lands in the SAME conversation as the investigation prev's round performed
// (round 2+ of Attempt's revision loop) instead of re-orienting from
// scratch. prev == nil (round 1) reseeds via RunJSON exactly as before.
//
// The returned Outcome is the round's own, meant to be threaded back in as
// the NEXT round's prev by the caller — this is how the conversation chains
// across rounds without Attempt itself touching message history.
func (r *Reproducer) planFor(ctx context.Context, runner *agent.Runner, finding domain.Finding, pkgSummary, feedback string, prev *agent.Outcome) (*Plan, *agent.Outcome, error) {
	var plan Plan
	var outcome *agent.Outcome
	var err error
	if prev == nil {
		outcome, err = runner.RunJSON(ctx, buildTask(finding, pkgSummary, feedback), planSchema, &plan)
	} else {
		// Only the feedback goes out on a continuation turn: the finding's
		// title/description/reasoning/package summary already sit in history
		// as round 1's opening user turn (RunJSONContinue keeps it), so
		// resending them here would just double the finding's prose on every
		// revision round for no benefit.
		outcome, err = runner.RunJSONContinue(ctx, prev, buildRevisionTask(feedback), planSchema, &plan)
	}
	if err != nil {
		return nil, outcome, err
	}
	return &plan, outcome, nil
}

// buildRevisionTask renders the per-round task prompt for a CONTINUATION
// revision round (round 2+ of Attempt, via planFor's prev != nil branch).
// Unlike buildTask, it carries only the revision feedback: the finding's
// framing has already been sent as the conversation's opening turn and stays
// in history under RunJSONContinue, so it is deliberately not repeated here.
//
// feedback is the verbatim output of verdict.feedback (interpret.go); see
// buildTask's doc comment for the fencing/sanitization contract this
// preserves — feedback MUST NOT be re-wrapped, stripped, or reformatted.
func buildRevisionTask(feedback string) string {
	var b strings.Builder
	b.WriteString("--- Revision required ---\n")
	b.WriteString(feedback)
	b.WriteString("\nProduce a corrected plan.\n")
	return b.String()
}

// buildTask renders the per-finding task prompt, including the finding's
// location and reasoning and any revision feedback.
//
// The feedback string, when non-empty, is the verbatim output of
// verdict.feedback (interpret.go) — which already wraps the untrusted
// sandbox summary in unique data-fence delimiter lines. This function
// embeds feedback as-is: it MUST NOT re-wrap, strip, or reformat the
// fenced sandbox block, or the agent loses the explicit
// "data, not instructions" framing that protects it from treating the
// run output as system-level directives. No double-fencing.
//
// SANITIZATION (bugbot-nzki): model-authored multi-line fields (Description,
// Reasoning) are wrapped in unique delimiter fences (util.FenceBlock) so
// embedded newlines and fake section headers cannot inject structural
// directives. Single-line fields (Title, Severity) are flattened to one line
// (util.FlattenField).
func buildTask(finding domain.Finding, pkgSummary, feedback string) string {
	var b strings.Builder
	b.WriteString("Reproduce the following verified bug with a minimal failing test.\n\n")
	fmt.Fprintf(&b, "Title: %s\n", util.FlattenField(finding.Title))
	if finding.Severity != "" {
		fmt.Fprintf(&b, "Severity: %s\n", util.FlattenField(string(finding.Severity)))
	}
	if finding.File != "" {
		fmt.Fprintf(&b, "Location: %s:%d\n", finding.File, finding.Line)
	}
	if finding.Description != "" {
		fmt.Fprintf(&b, "\nDescription:\n%s\n", util.FenceBlock("DESCRIPTION", finding.Description))
	}
	if finding.Reasoning != "" {
		fmt.Fprintf(&b, "\nVerification reasoning:\n%s\n", util.FenceBlock("REASONING", finding.Reasoning))
	}
	if strings.TrimSpace(pkgSummary) != "" {
		fmt.Fprintf(&b, "\nPackage context for %s (cartographer summary — orient from it, confirm specifics by reading files):\n%s\n", path.Dir(finding.File), pkgSummary)
	}
	if strings.TrimSpace(feedback) != "" {
		b.WriteString("\n--- Revision required ---\n")
		b.WriteString(feedback)
		b.WriteString("\nProduce a corrected plan.\n")
	}
	return b.String()
}

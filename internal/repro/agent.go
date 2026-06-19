package repro

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
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
		"  " + "`go test -run <TestName> ./<pkg>`" + " (or the module path that targets it).",
	ingest.LangPython: "For Python, write a\n" +
		"  test_*.py file (pytest style) next to or under the package with the bug\n" +
		"  and run it with " + "`pytest -k <test_name> <path>`" + ".",
	ingest.LangJavaScript: jsTSGuidance,
	ingest.LangTypeScript: jsTSGuidance,
	ingest.LangRust: "For Rust, add a\n" +
		"  " + "`#[test]`" + " function (in the crate with the bug or a tests/ file) and run it\n" +
		"  with " + "`cargo test <test_name>`" + ".",
	ingest.LangCSharp: "For C#, add a\n" +
		"  test method annotated with " + "`[Fact]`" + " (xUnit) or " + "`[Test]`" + " (NUnit) in the\n" +
		"  relevant test project and run it with\n" +
		"  " + "`dotnet test --filter <Name>`" + ".",
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
// to avoid modes that are unavailable in the image.
func systemPrompt(lang ingest.Language, systems []ingest.BuildSystem, caps sandbox.CapabilitySet) string {
	return `You are Bugbot's reproducer agent. Your job is to write a MINIMAL test that
demonstrates a specific, already-verified bug by FAILING because of it.

You have read-only tools (read_file, list_dir, grep) rooted at the target
repository. Investigate the finding's file, line, and reasoning first, then
produce a repro plan.

Hard requirements for the repro:
- Prefer a standard test for the repository's language. ` + langGuidance(lang, systems) + `
- The test MUST FAIL (exit non-zero) on the CURRENT, buggy code, and MUST PASS
  once the bug is fixed. Encode the bug as an explicit assertion: call the
  buggy code and assert the CORRECT expected result, so the wrong current
  behavior makes the assertion fail. Do NOT write a test that merely triggers a
  panic or crash without an assertion unless the panic itself is the bug being
  demonstrated and the test asserts it should not panic.
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
` + capabilityGuidance(caps) + `
Return a repro plan describing the files to inject, the command to run them,
and a short description of the expected failure.`
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

	return b.String()
}

// planSchema is the JSON schema for the reproducer agent's plan output. Only
// files and cmd are required: they are load-bearing for execution and mirror
// validatePlan's structural gate. expect is descriptive-only (artifact README,
// patch context, human review) so it is requested but not required — a model
// that produces a runnable files+cmd plan must not have the whole attempt
// aborted with a hard parse error merely for omitting the prose description.
var planSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "files": {
      "type": "object",
      "description": "Repro/test files to inject, keyed by a workspace-relative path INSIDE the repo (e.g. \"repro_test.cpp\" or \"test/repro_test.cpp\"). Absolute or escaping paths like \"/tmp/foo.cpp\" are REJECTED — write injected sources into the repo tree. You MAY still emit build outputs to /tmp at run time via cmd (e.g. -o /tmp/repro). For Go, typically one _test.go file.",
      "additionalProperties": {"type": "string"},
      "minProperties": 1
    },
    "cmd": {
      "type": "array",
      "description": "Argv to run the repro, e.g. [\"go\",\"test\",\"-run\",\"TestX\",\"./pkg\"].",
      "items": {"type": "string"},
      "minItems": 1
    },
    "expect": {
      "type": "string",
      "minLength": 1,
      "description": "Short description of the expected failure (what assertion fails and why)."
    }
  },
  "required": ["files", "cmd"],
  "additionalProperties": false
}`)

// planFor asks the agent for a repro plan for finding. feedback, when
// non-empty, is appended to steer a revision after a prior non-demonstrating
// attempt.
func (r *Reproducer) planFor(ctx context.Context, runner *agent.Runner, finding store.Finding, feedback string) (*Plan, llm.Usage, error) {
	task := buildTask(finding, feedback)
	var plan Plan
	outcome, err := runner.RunJSON(ctx, task, planSchema, &plan)
	var usage llm.Usage
	if outcome != nil {
		usage = outcome.Usage
	}
	if err != nil {
		return nil, usage, err
	}
	return &plan, usage, nil
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
func buildTask(finding store.Finding, feedback string) string {
	var b strings.Builder
	b.WriteString("Reproduce the following verified bug with a minimal failing test.\n\n")
	fmt.Fprintf(&b, "Title: %s\n", finding.Title)
	if finding.Severity != "" {
		fmt.Fprintf(&b, "Severity: %s\n", finding.Severity)
	}
	if finding.File != "" {
		fmt.Fprintf(&b, "Location: %s:%d\n", finding.File, finding.Line)
	}
	if finding.Description != "" {
		fmt.Fprintf(&b, "\nDescription:\n%s\n", finding.Description)
	}
	if finding.Reasoning != "" {
		fmt.Fprintf(&b, "\nVerification reasoning:\n%s\n", finding.Reasoning)
	}
	if strings.TrimSpace(feedback) != "" {
		b.WriteString("\n--- Revision required ---\n")
		b.WriteString(feedback)
		b.WriteString("\nProduce a corrected plan.\n")
	}
	return b.String()
}

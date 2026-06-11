package repro

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/ingest"
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

// specificGuidance is the per-language test-framework guidance spliced into
// the reproducer system prompt. It is the single source of truth for which
// languages have specific guidance: langGuidance reads the text from it and
// HasGuidance reports membership, so the two cannot drift. The Go text is
// verbatim from the original prompt; the others give the idiomatic test file +
// run command for each ecosystem.
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
}

// HasGuidance reports whether lang has language-specific repro guidance in
// langGuidance (as opposed to the generic fallback). Doctor and the setup
// wizard call this to warn when a dominant language lacks specific guidance,
// reading the live table rather than maintaining a parallel list.
func HasGuidance(lang ingest.Language) bool {
	_, ok := specificGuidance[lang]
	return ok
}

// langGuidance returns the one-line test-framework guidance spliced into the
// reproducer system prompt for the finding's language.
func langGuidance(lang ingest.Language) string {
	if g, ok := specificGuidance[lang]; ok {
		return g
	}
	return genericGuidance
}

// systemPrompt instructs the reproducer agent to produce a MINIMAL,
// assertion-bearing failing test for the finding. The emphasis is that the
// repro must fail *because of the bug* and would pass if the bug were fixed —
// not merely crash, and not fail to compile. The lang argument selects the
// language-specific test-framework guidance (see langGuidance); everything else
// is language-independent.
func systemPrompt(lang ingest.Language) string {
	return `You are Bugbot's reproducer agent. Your job is to write a MINIMAL test that
demonstrates a specific, already-verified bug by FAILING because of it.

You have read-only tools (read_file, list_dir, grep) rooted at the target
repository. Investigate the finding's file, line, and reasoning first, then
produce a repro plan.

Hard requirements for the repro:
- Prefer a standard test for the repository's language. ` + langGuidance(lang) + `
- The test MUST FAIL (exit non-zero) on the CURRENT, buggy code, and MUST PASS
  once the bug is fixed. Encode the bug as an explicit assertion: call the
  buggy code and assert the CORRECT expected result, so the wrong current
  behavior makes the assertion fail. Do NOT write a test that merely triggers a
  panic or crash without an assertion unless the panic itself is the bug being
  demonstrated and the test asserts it should not panic.
- Keep it minimal: the smallest test that exercises the bug. Do not add new
  dependencies. Use only the standard library and what the repository already
  imports. The test must COMPILE — a compile error or missing dependency is NOT
  a reproduction and will be rejected.

Return a repro plan describing the files to inject, the command to run them,
and a short description of the expected failure.`
}

// planSchema is the JSON schema for the reproducer agent's plan output.
var planSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "files": {
      "type": "object",
      "description": "Repro/test files to inject, keyed by workspace-relative path. For Go, typically one _test.go file.",
      "additionalProperties": {"type": "string"}
    },
    "cmd": {
      "type": "array",
      "description": "Argv to run the repro, e.g. [\"go\",\"test\",\"-run\",\"TestX\",\"./pkg\"].",
      "items": {"type": "string"},
      "minItems": 1
    },
    "expect": {
      "type": "string",
      "description": "Short description of the expected failure (what assertion fails and why)."
    }
  },
  "required": ["files", "cmd", "expect"],
  "additionalProperties": false
}`)

// planFor asks the agent for a repro plan for finding. feedback, when
// non-empty, is appended to steer a revision after a prior non-demonstrating
// attempt.
func (r *Reproducer) planFor(ctx context.Context, runner *agent.Runner, finding store.Finding, feedback string) (*Plan, error) {
	task := buildTask(finding, feedback)
	var plan Plan
	if _, err := runner.RunJSON(ctx, task, planSchema, &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// buildTask renders the per-finding task prompt, including the finding's
// location and reasoning and any revision feedback.
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

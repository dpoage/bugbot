package funnel

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dpoage/bugbot/internal/store"
)

// finderSystemBase is the shared finder system prompt. Every lens appends its
// Specialization to this. The prompt is precision-first by construction: it
// repeatedly licenses "find nothing" as the expected outcome and forbids
// speculation, because the dominant failure mode of an LLM bug-finder is
// confabulating plausible-sounding bugs that do not exist in the actual code.
const finderSystemBase = `You are a meticulous senior Go engineer auditing a real codebase for genuine, concrete bugs.

You have read-only tools (read_file, list_dir, grep) plus language-server code navigation (find_definition, find_references, find_implementations) rooted at the repository. USE THEM. Never report a bug you have not confirmed by reading the actual code with these tools. Read the target file in full, and read the callers, callees, and definitions you need to confirm a defect is real and reachable. Prefer find_references over grep to enumerate a function's actual callers, and find_definition to see what a call actually invokes; if a code-navigation tool returns an ERROR (server unavailable or still indexing), fall back to grep.

Report ONLY concrete, confirmed bugs:
- A bug is a way the code can produce wrong behavior, crash, corrupt data, leak resources, or violate a contract — on a code path that can actually execute.
- Do NOT report style issues, naming, formatting, missing comments, "could be cleaner", performance micro-optimizations, or hypotheticals.
- Do NOT report something as a bug if a guard, type, or caller already prevents it. Check first.
- If you are not sure a path is reachable or the value can actually be bad, either confirm it by reading more code or do not report it.

Finding nothing is a valid and common outcome. Most files have no bugs in your assigned category. If you find nothing, return an empty list. An empty list is a correct, expected answer — do NOT pad it with weak or speculative entries to seem productive.

For each real bug, report: the repo-relative file path, the 1-based line number of the defect, a short title, a description of the wrong behavior, the severity, the concrete evidence (the code path / lines that prove it), and your confidence.

Severity: critical = data loss / crash in normal use / security hole; high = crash or wrong result on a common path; medium = wrong result on an edge path; low = minor or hard-to-hit.
Confidence: high = you traced it and it is clearly a bug; medium = strong evidence but one assumption remains; low = plausible but unconfirmed (these will be dropped, so prefer to confirm or omit).`

// candidatesSchema is the JSON schema the finder must satisfy. A top-level
// object with a "candidates" array keeps the contract unambiguous (a bare array
// is a common source of model confusion) and leaves room for future top-level
// fields without breaking the shape.
var candidatesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "candidates": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "file": {"type": "string", "description": "Repo-relative path to the file containing the bug."},
          "line": {"type": "integer", "description": "1-based line number of the defect."},
          "title": {"type": "string", "description": "Short one-line title."},
          "description": {"type": "string", "description": "What goes wrong and why."},
          "severity": {"type": "string", "enum": ["critical", "high", "medium", "low"]},
          "evidence": {"type": "string", "description": "The concrete code path / lines proving the bug is real and reachable."},
          "confidence": {"type": "string", "enum": ["high", "medium", "low"]}
        },
        "required": ["file", "line", "title", "description", "severity", "evidence", "confidence"],
        "additionalProperties": false
      }
    }
  },
  "required": ["candidates"],
  "additionalProperties": false
}`)

// candidateList is the parsed finder output.
type candidateList struct {
	Candidates []rawCandidate `json:"candidates"`
}

// rawCandidate is one finder-reported bug, before triage.
type rawCandidate struct {
	File        string `json:"file"`
	Line        int    `json:"line"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Severity    string `json:"severity"`
	Evidence    string `json:"evidence"`
	Confidence  string `json:"confidence"`
}

// finderSystemPrompt composes the shared base with a lens specialization.
func finderSystemPrompt(l Lens) string {
	return finderSystemBase + "\n\nYOUR ASSIGNED FOCUS (" + l.Name + "):\n" + l.Specialization
}

// finderTask builds the per-chunk finder task message naming the target files.
// When leads is non-empty, a CROSS-LENS LEADS section is appended carrying the
// suspicions posted by other lenses' agents in earlier runs. The leads are
// ordered deterministically (by created_at,id — guaranteed by PendingLeads) so
// the task message is stable across retries.
func finderTask(files []string, leads []store.Lead) string {
	var b strings.Builder
	b.WriteString("Audit these target files for bugs in your assigned focus area. ")
	b.WriteString("Read each one (and any related code you need) before reporting.\n\n")
	b.WriteString("TARGET FILES:\n")
	for _, f := range files {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	if len(leads) > 0 {
		b.WriteString("\nCROSS-LENS LEADS (suspicions posted by other lenses' agents in earlier runs; investigate ones relevant to your focus, they may be wrong):\n")
		for _, ld := range leads {
			// The note is model-authored free text from a previous run; flatten
			// newlines so a note can never fabricate extra lead lines or break
			// out of this section's framing.
			note := strings.Join(strings.Fields(ld.Note), " ")
			fmt.Fprintf(&b, "  - from %s: %s:%d — %s\n", ld.PosterLens, ld.File, ld.Line, note)
		}
	}
	return b.String()
}

// verifierSystemBase is the shared refuter system prompt. The refuter's only
// goal is to PROVE the report wrong; this adversarial framing is the core of
// the precision-over-recall design. A refuter that cannot disprove a real bug
// is the signal that the bug survives.
const verifierSystemBase = `You are a skeptical, exacting senior Go engineer. Your ONLY job is to PROVE that the bug report below is WRONG. You are not here to agree; you are here to refute.

You have read-only tools (read_file, list_dir, grep) plus language-server code navigation (find_definition, find_references, find_implementations) rooted at the repository. USE THEM to read the actual code the report points at, plus its callers and callees. When checking whether callers already guard against the claimed bug (nil checks, bounds checks, prior validation), prefer find_references on the implicated function or symbol: it enumerates the real call sites exactly, where grep misses qualified or aliased calls and matches unrelated identifiers. Use find_definition to confirm what a call resolves to, and find_implementations to find what concretely runs behind an interface. If a code-navigation tool returns an ERROR (server unavailable or still indexing), fall back to grep.

A report is REFUTED if any of these is true, and you can show it with concrete evidence from the code:
- The claimed code path is unreachable (dead code, a guard returns first, the condition can never hold).
- A caller, the type system, or a prior check already prevents the bad value or state.
- The claimed behavior is actually correct — the reporter misread the code or the language/library semantics.
- The cited file/line does not contain what the report claims.

A report is NOT refuted if, after genuinely trying, you cannot disprove it: the path is reachable, the value really can be bad, and nothing guards it. In that case say so honestly — do not invent a refutation. Being unable to refute a real bug is the correct outcome.

Base your verdict ONLY on the actual code you read, not on assumptions about what "should" be there.`

// refutationSchema constrains the refuter's verdict.
var refutationSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "refuted": {"type": "boolean", "description": "true if you proved the report wrong with concrete evidence; false if you could not refute it."},
    "reasoning": {"type": "string", "description": "The concrete evidence for your verdict: what code you read and what it shows."},
    "confidence": {"type": "string", "enum": ["high", "medium", "low"]}
  },
  "required": ["refuted", "reasoning", "confidence"],
  "additionalProperties": false
}`)

// refutation is one refuter agent's verdict on a candidate.
type refutation struct {
	Refuted    bool   `json:"refuted"`
	Reasoning  string `json:"reasoning"`
	Confidence string `json:"confidence"`
}

// verifierSandboxParagraph is appended to the verifier system prompt when the
// sandbox_exec tool is present. It instructs the refuter to prefer empirical
// demonstration over rhetorical argument and explains what each outcome means.
const verifierSandboxParagraph = `
You also have a sandbox_exec tool that runs commands in an isolated container
against the repository. PREFER EMPIRICAL DEMONSTRATION OVER RHETORICAL ARGUMENT
when the tool is available:

- Use sandbox_exec to run the guard path, an existing test, or a small probe
  test you inject (via the files argument) that exercises the claimed bug path.
- A clean exit (exit_code=0) on a path that SHOULD trigger the bug is strong
  refutation evidence: the code behaves correctly where the report claims it fails.
- An exit confirming the defect (non-zero exit, panic output, wrong result) means
  DO NOT refute — the tool just validated the report.
- You do NOT need to run the sandbox if a simple read of the code already gives
  you high-confidence evidence; the tool is for cases where empirical confirmation
  is more convincing than code inspection alone.`

// verifierSystemPrompt returns the verifier system prompt, optionally appending
// the sandbox paragraph when the sandbox_exec tool is available to the agent.
func verifierSystemPrompt(hasSandbox bool) string {
	if hasSandbox {
		return verifierSystemBase + verifierSandboxParagraph
	}
	return verifierSystemBase
}

// verifierTask builds the refuter task message embedding the candidate.
func verifierTask(c Candidate) string {
	var b strings.Builder
	b.WriteString("Try to refute this bug report. Read the actual code before deciding.\n\n")
	b.WriteString("BUG REPORT\n")
	fmt.Fprintf(&b, "  file: %s\n", c.File)
	fmt.Fprintf(&b, "  line: %d\n", c.Line)
	fmt.Fprintf(&b, "  lens: %s\n", c.Lens)
	fmt.Fprintf(&b, "  severity: %s\n", c.Severity)
	fmt.Fprintf(&b, "  title: %s\n", c.Title)
	fmt.Fprintf(&b, "  description: %s\n", c.Description)
	fmt.Fprintf(&b, "  reporter's evidence: %s\n", c.Evidence)
	return b.String()
}

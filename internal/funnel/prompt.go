package funnel

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// finderSystemBase composes the shared finder system prompt around a persona
// clause derived from the repository's dominant language(s) (e.g. "senior Go
// engineer", "senior software engineer with deep Go and Python expertise"). The
// persona is the ONLY language-dependent text; everything after it is fixed.
// Every lens appends its Core (and per-language manifestation blocks; see
// finderSystemPrompt) to this. The prompt is precision-first
// by construction: it repeatedly licenses "find nothing" as the expected
// outcome and forbids speculation, because the dominant failure mode of an LLM
// bug-finder is confabulating plausible-sounding bugs that do not exist in the
// actual code.
func finderSystemBase(persona string) string {
	return `You are a meticulous ` + persona + ` auditing a real codebase for genuine, concrete bugs.

You have read-only tools (read_file, list_dir, grep) plus language-server code navigation (find_definition, find_references, find_implementations, read_symbol) rooted at the repository. USE THEM. Never report a bug you have not confirmed by reading the actual code with these tools. Read the target file in full, and read the callers, callees, and definitions you need to confirm a defect is real and reachable. Prefer find_references over grep to enumerate a function's actual callers, and find_definition to see what a call actually invokes; when you only need one function/method/type body, prefer read_symbol over read_file — it returns just that declaration. If a code-navigation tool returns an ERROR (server unavailable or still indexing), fall back to grep.

Report ONLY concrete, confirmed bugs:
- A bug is a way the code can produce wrong behavior, crash, corrupt data, leak resources, or violate a contract — on a code path that can actually execute.
- Do NOT report style issues, naming, formatting, missing comments, "could be cleaner", performance micro-optimizations, or hypotheticals.
- Do NOT report something as a bug if a guard, type, or caller already prevents it. Check first.
- If you are not sure a path is reachable or the value can actually be bad, either confirm it by reading more code or do not report it.

Finding nothing is a valid and common outcome. Most files have no bugs in your assigned category. If you find nothing, return an empty list. An empty list is a correct, expected answer — do NOT pad it with weak or speculative entries to seem productive.

For each real bug, report: the repo-relative file path, the 1-based line number of the defect, a short title, a description of the wrong behavior, the severity, the concrete evidence (the code path / lines that prove it), and your confidence.

Severity: critical = data loss / crash in normal use / security hole; high = crash or wrong result on a common path; medium = wrong result on an edge path; low = minor or hard-to-hit.
Confidence: high = you traced it and it is clearly a bug; medium = strong evidence but one assumption remains; low = plausible but unconfirmed (these will be dropped, so prefer to confirm or omit).`
}

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
          "file": {"type": "string", "minLength": 1, "description": "Repo-relative path to the file containing the bug."},
          "line": {"type": "integer", "minimum": 1, "description": "1-based line number of the defect."},
          "title": {"type": "string", "minLength": 1, "description": "Short one-line title."},
          "description": {"type": "string", "minLength": 1, "description": "What goes wrong and why."},
          "severity": {"type": "string", "enum": ["critical", "high", "medium", "low"]},
          "evidence": {"type": "string", "minLength": 1, "description": "The concrete code path / lines proving the bug is real and reachable."},
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

// finderSystemPrompt composes the persona-seeded base with a lens: the
// universal Core, then one "How this manifests in <Language>" block per chunk
// language that has rows in the manifestations table. persona is the
// language-derived engineer description (see ingest.Persona); langs is the
// chunk's language set (union for mixed chunks). The lens name and Core are
// appended verbatim, so the eval harness's lens-name routing is unaffected by
// the persona or the language mix.
//
// Composition is purely data-driven: languages without manifestation rows are
// omitted, and a lens with no manifestation entries at all (a language-free
// lens) composes Core alone — adding a language column or lens row never
// requires a change here. Languages whose row slices are equal (JavaScript and
// TypeScript share theirs) merge into a single block so a mixed JS/TS chunk
// does not carry the same guidance twice.
func finderSystemPrompt(persona string, l Lens, langs []ingest.Language) string {
	var b strings.Builder
	b.WriteString(finderSystemBase(persona))
	b.WriteString("\n\nYOUR ASSIGNED FOCUS (")
	b.WriteString(l.Name)
	b.WriteString("):\n")
	b.WriteString(l.Core)

	// Group the chunk's languages by identical row content so shared tables
	// (JS/TS, C/C++ injection) render once under a merged heading.
	type block struct {
		names []string
		rows  []string
	}
	var blocks []block
	rowsByLang := manifestations[l.Name]
	for _, lang := range langs {
		rows := rowsByLang[lang]
		if len(rows) == 0 {
			continue
		}
		merged := false
		for i := range blocks {
			if equalRows(blocks[i].rows, rows) {
				blocks[i].names = append(blocks[i].names, ingest.DisplayName(lang))
				merged = true
				break
			}
		}
		if !merged {
			blocks = append(blocks, block{names: []string{ingest.DisplayName(lang)}, rows: rows})
		}
	}
	for _, blk := range blocks {
		b.WriteString("\n\nHow this manifests in ")
		b.WriteString(strings.Join(blk.names, "/"))
		b.WriteString(":")
		for _, row := range blk.rows {
			b.WriteString("\n- ")
			b.WriteString(row)
		}
	}
	return b.String()
}

// equalRows reports whether two manifestation row slices carry identical
// content. Shared tables are usually the same slice, but content equality is
// the honest criterion: it is what makes a duplicated block redundant.
func equalRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// finderTask builds the per-chunk finder task message naming the target files.
// When leads is non-empty, a CROSS-LENS LEADS section is appended carrying the
// suspicions posted by other lenses' agents in earlier runs. The leads are
// ordered deterministically (by created_at,id — guaranteed by PendingLeads) so
// the task message is stable across retries.
//
// When repoContext is non-empty (the cartographer pass has summaries to
// inject for the chunk's packages), it is appended AFTER the TARGET FILES
// list and BEFORE the leads section. The injection block is strictly
// additive: it never replaces, mutates, or wraps any other section. This
// placement matters for prompt-cache stability: the prefix up through the
// leads section is the cacheable warm path, and injection sits at the end
// of the user message so the cached prefix bytes do not shift between
// runs that share the same targets and leads.
func finderTask(files []string, leads []store.Lead, repoContext string) string {
	var b strings.Builder
	b.WriteString("Audit these target files for bugs in your assigned focus area. ")
	b.WriteString("Read each one (and any related code you need) before reporting.\n\n")
	b.WriteString("TARGET FILES:\n")
	for _, f := range files {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	if repoContext != "" {
		b.WriteString("\n")
		b.WriteString(repoContext)
	}
	appendLeadsSection(&b, leads)
	return b.String()
}

// verifierToolParagraph is the shared tool-usage paragraph for all verifier-side
// agents (refuters and the arbiter). Keeping it in one place ensures they never
// drift: any change here applies to both.
const verifierToolParagraph = `You have read-only tools (read_file, list_dir, grep) plus language-server code navigation (find_definition, find_references, find_implementations, read_symbol) rooted at the repository. USE THEM to read the actual code the report points at, plus its callers and callees. When checking whether callers already guard against the claimed bug (nil checks, bounds checks, prior validation), prefer find_references on the implicated function or symbol: it enumerates the real call sites exactly, where grep misses qualified or aliased calls and matches unrelated identifiers. Use find_definition to confirm what a call resolves to, find_implementations to find what concretely runs behind an interface, and read_symbol to pull a single function/method/type body without reading the whole file. If a code-navigation tool returns an ERROR (server unavailable or still indexing), fall back to grep.`

// verifierRefutationCriteria is the shared REFUTED/NOT REFUTED criteria block
// used by both refuters and the arbiter. Extracting it prevents the criteria
// from drifting between the two agents.
const verifierRefutationCriteria = `A report is REFUTED if any of these is true, and you can show it with concrete evidence from the code:
- The claimed code path is unreachable (dead code, a guard returns first, the condition can never hold).
- A caller, the type system, or a prior check already prevents the bad value or state.
- The claimed behavior is actually correct — the reporter misread the code or the language/library semantics.
- The cited file/line does not contain what the report claims.

A report is NOT refuted if, after genuinely trying, you cannot disprove it: the path is reachable, the value really can be bad, and nothing guards it. In that case say so honestly — do not invent a refutation. Being unable to refute a real bug is the correct outcome.

MECHANISM CORRECTION: if the bug stands (refuted=false) but the DESCRIBED MECHANISM is inaccurate (wrong function, wrong call path, wrong variable — the bug is real but the report misdescribes how it triggers), set corrected_description to the accurate mechanism. The corrected description will replace the finder's in the published finding so the bug report is accurate. Leave corrected_description absent when the mechanism description is correct.

IMPORTANT — abstention rule: if you CANNOT LOCATE OR READ the cited file(s) (tool errors, file-not-found, path not in repo, access denied), you have not examined the code. Set could_not_read_code=true and refuted=false. This is an ABSTENTION — you never saw the evidence — and is counted separately from reading the code and being unable to refute. Do NOT return could_not_read_code=true when you successfully read the files; only set it when the file is genuinely inaccessible.

Base your verdict ONLY on the actual code you read, not on assumptions about what "should" be there.

When a verdict hinges on the behavior of the standard library, the language runtime, or a third-party dependency, you MUST confirm that behavior by reading the actual source (GOROOT and vendored/module source are available to the read tools) or by running a probe — NEVER assert it from memory. Your reasoning MUST cite what you read or ran: file path and line, or the command and its observed output. An unverified stdlib/runtime/library claim is not acceptable refutation evidence.`

// verifierSystemBase composes the shared refuter system prompt around a persona
// clause derived from the repository's dominant language(s). The persona is the
// ONLY language-dependent text; everything after it is fixed. The refuter's only
// goal is to PROVE the report wrong; this adversarial framing is the core of
// the precision-over-recall design. A refuter that cannot disprove a real bug
// is the signal that the bug survives.
func verifierSystemBase(persona string) string {
	return `You are a skeptical, exacting ` + persona + `. Your ONLY job is to PROVE that the bug report below is WRONG. You are not here to agree; you are here to refute.

` + verifierToolParagraph + `

` + verifierRefutationCriteria
}

// refutationSchema constrains the refuter's verdict. Optional fields:
//   - could_not_read_code: file access failed → abstention
//   - corrected_description: bug stands (refuted=false) but the described
//     mechanism is wrong; refuter supplies the accurate mechanism here so
//     unanimous-survive panels can fold the correction without an arbiter.
//   - hallucinated_rebuttal: only meaningful for a refuter that claims to
//     have refuted; set true when the rebuttal asserts nonexistent code.
//
// All three are absent from required so existing response bodies that omit
// them (e.g. notRefutedJSON in tests) still parse cleanly.
var refutationSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "refuted": {"type": "boolean", "description": "true if you proved the report wrong with concrete evidence; false if you could not refute it."},
    "reasoning": {"type": "string", "minLength": 1, "description": "The concrete evidence for your verdict: what code you read and what it shows."},
    "confidence": {"type": "string", "enum": ["high", "medium", "low"]},
    "could_not_read_code": {"type": "boolean", "description": "true if you could not locate or access the cited file(s) and therefore abstained from a verdict. Set ONLY when file access failed; omit or false when you successfully read the code."},
    "corrected_description": {"type": "string", "description": "When the bug stands (refuted=false) but the described mechanism is inaccurate, emit the accurate mechanism here so the published description is correct. Leave absent when the mechanism description is accurate."},
    "hallucinated_rebuttal": {"type": "boolean", "description": "true if your rebuttal asserts the existence of code NOT present in the cited files (a fabricated 'safe' guard or path). Set ONLY when you refuted; self-identifies fabrication."}
  },
  "required": ["refuted", "reasoning", "confidence"],
  "additionalProperties": false
}`)

// arbiterSchema extends refutationSchema with the same optional correction
// fields. The arbiter synthesizes across all panel seats; refuters use
// refutationSchema (now also carrying the same optional fields so that
// unanimous-survive corrections can be folded without an arbiter round-trip).
var arbiterSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "refuted": {"type": "boolean", "description": "true if you proved the report wrong with concrete evidence; false if the bug stands."},
    "reasoning": {"type": "string", "minLength": 1, "description": "The concrete evidence for your verdict."},
    "confidence": {"type": "string", "enum": ["high", "medium", "low"]},
    "could_not_read_code": {"type": "boolean", "description": "true if you could not locate or access the cited file(s)."},
    "corrected_description": {"type": "string", "description": "When the finding SURVIVES but a panel seat correctly identified an error in the mechanism or sub-claim, emit the accurate mechanism here. Leave absent when no correction is warranted."},
    "hallucinated_rebuttal": {"type": "boolean", "description": "true if a panel seat's rebuttal asserted the existence of code NOT present in the cited files (a fabricated 'safe' path). Do not credit hallucinated rebuttals."}
  },
  "required": ["refuted", "reasoning", "confidence"],
  "additionalProperties": false
}`)

// refutation is one refuter agent's verdict on a candidate. The arbiter also
// uses this struct (with its superset arbiterSchema) so CorrectedDescription
// and HallucinatedRebuttal are populated only when the arbiter ran.
type refutation struct {
	Refuted              bool   `json:"refuted"`
	Reasoning            string `json:"reasoning"`
	Confidence           string `json:"confidence"`
	CouldNotReadCode     bool   `json:"could_not_read_code"`
	CorrectedDescription string `json:"corrected_description"`
	HallucinatedRebuttal bool   `json:"hallucinated_rebuttal"`
	// NoVerdict marks a seat that produced NO genuine verdict: the refuter run
	// failed at the infrastructure level (transport/provider error — zero
	// tokens) or its output could not be parsed even after one repair
	// round-trip. It is set by runRefuters, never by the model (json:"-"). A
	// NoVerdict seat is still "not refuted" for the KILL decision (a broken
	// refuter must never CAUSE a kill) but is EXCLUDED from the survive-trust
	// quorum (genuineVerdicts): a missing verdict is evidence of nothing, so it
	// must never silently PROMOTE a candidate to verified (bugbot-8rd).
	NoVerdict bool `json:"-"`
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

// verifierSystemPrompt returns the verifier system prompt seeded with persona
// (the language-derived engineer description; see ingest.Persona), optionally
// appending the sandbox paragraph when the sandbox_exec tool is available to the
// agent. The sandbox paragraph and all other wording are language-independent.
func verifierSystemPrompt(persona string, hasSandbox bool) string {
	if hasSandbox {
		return verifierSystemBase(persona) + verifierSandboxParagraph
	}
	return verifierSystemBase(persona)
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

// arbiterSystemPrompt builds the system prompt for the deciding arbiter agent.
// The arbiter is invoked on a SPLIT panel verdict and must adjudicate between
// the two sides by reading the actual code — not by averaging opinions.
// It reuses verifierToolParagraph and verifierRefutationCriteria verbatim so
// the arbiter's refutation standard never drifts from the panel's.
func arbiterSystemPrompt(persona string, hasSandbox bool) string {
	p := `You are a senior ` + persona + ` serving as the deciding arbiter on a disputed bug report. A panel of adversarial reviewers split on whether the report below is a real bug. You will be given the report and each reviewer's verdict and reasoning. Your ONLY job is to decide who is right, by reading the actual code with your tools — do not average the opinions, adjudicate them. Weigh a concrete code-backed demonstration over a plausible-sounding argument, whichever side it comes from.

` + verifierToolParagraph + `

` + verifierRefutationCriteria + `

ARBITER ADDITIONAL RULES:
1. MECHANISM CORRECTION: If the finding SURVIVES (refuted=false) but a panel seat correctly identified an error in the described mechanism or sub-claim, emit corrected_description with the accurate mechanism. The published bug report will use your corrected_description instead of the finder's. Leave corrected_description absent when no correction is needed or when you refute the finding.
2. HALLUCINATED REBUTTAL: If a seat's rebuttal asserts the existence of code that is NOT actually present in the cited files (a fabricated 'safe' guard, function, or check), set hallucinated_rebuttal=true and do NOT credit that seat's rebuttal in your decision.`
	if hasSandbox {
		p += verifierSandboxParagraph
	}
	return p
}

// arbiterTask builds the task message for the arbiter agent. It embeds the
// candidate (identical to verifierTask) followed by each panel seat's verdict.
// SANITIZATION: refuter reasoning is model-authored free text crossing a prompt
// boundary. Each reasoning field is flattened (newlines collapsed to spaces, per
// finderTask's lead-note handling at prompt.go:178-180) to protect the
// one-item-per-line format of the PANEL VERDICTS block — a value's newlines
// must not fabricate additional "PANEL VERDICTS" rows or break section
// framing. This is line-format integrity, not a general anti-injection
// guard (see internal/repro/interpret.go for the fencing approach used
// when the untrusted payload must be preserved verbatim, e.g. multi-line
// sandbox output).
func arbiterTask(c Candidate, verdicts []refutation, seatNames []string) string {
	var b strings.Builder
	b.WriteString(verifierTask(c))
	b.WriteString("\nPANEL VERDICTS (split):\n")
	for i, v := range verdicts {
		seatName := ""
		if i < len(seatNames) {
			seatName = seatNames[i]
		}
		verdict := "could not refute"
		if v.CouldNotReadCode {
			verdict = "abstained (could not read cited code)"
		} else if v.NoVerdict {
			verdict = "no verdict (verifier failed to produce a parseable result)"
		} else if v.Refuted {
			verdict = "refuted"
		}
		// Flatten model-authored reasoning: collapse whitespace so embedded
		// newlines cannot fabricate new section headers or panel blocks.
		reasoning := strings.Join(strings.Fields(v.Reasoning), " ")
		if seatName != "" {
			fmt.Fprintf(&b, "  seat %d [%s, %s, confidence=%s]: %s\n", i+1, seatName, verdict, v.Confidence, reasoning)
		} else {
			fmt.Fprintf(&b, "  seat %d [%s, confidence=%s]: %s\n", i+1, verdict, v.Confidence, reasoning)
		}
	}
	b.WriteString("\nRead the code yourself and issue your deciding verdict.\n")
	return b.String()
}

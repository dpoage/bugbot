package funnel

// dedup_arbiter.go implements the bugbot-ezmx.2 LLM dedup arbiter: a single
// bounded, zero-tool RunJSON turn that adjudicates a location collision the
// jaccard gate could not resolve on its own — same locus (or window-near) and
// same/unknown defect_kind, but description-token jaccard below
// mergeSimilarityThreshold (the "0.18 prose cliff"). It is invoked ONLY at
// collision sites (triage_streaming.go's step 5 cluster loop and
// durableCrossLensFold's SimilarFinding fallback), never per-candidate, and
// only on a confident "yes" does the caller fold the candidate into the
// existing finding via its normal merge path (handleMember /
// AddCorroboratingLenses+AppendFindingSites) — "no" and "unsure" both keep the
// two candidates as distinct findings (precision-first: an over-merge buries a
// real distinct finding; a kept near-duplicate only costs an extra panel).
//
// runDedupArbiter is the single exported-shape seam bugbot-ezmx.4 (backlog
// reconcile, wave 3) is expected to reuse: two finding-shaped views plus a
// code excerpt in, a typed verdict out. Machine decisions never key on prose
// (repo invariant) — the response schema requires a closed verdict enum, not
// a boolean inferred from reasoning text.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/util"
)

// dedupVerdict is the arbiter's typed same-defect judgment. Every caller
// switches on this closed type, never on the free-text Reasoning field.
type dedupVerdict string

const (
	dedupSame     dedupVerdict = "yes"
	dedupDistinct dedupVerdict = "no"
	dedupUnsure   dedupVerdict = "unsure"
)

// dedupCandidateView is one side of a collision: just enough of a
// finding-shaped candidate for the arbiter to judge "same defect". It
// deliberately does NOT depend on funnel.Candidate or domain.Finding — each
// call site adapts its own type into this view, so the arbiter itself stays
// usable from both the in-run triage path (Candidate) and, later, backlog
// reconcile (domain.Finding) without a caller-specific parameter creeping in.
type dedupCandidateView struct {
	Title       string
	Description string
	File        string
	Line        int
}

// candidateDedupView adapts a Candidate into the arbiter's view.
func candidateDedupView(c Candidate) dedupCandidateView {
	return dedupCandidateView{Title: c.Title, Description: c.Description, File: c.File, Line: c.Line}
}

// findingDedupView adapts a domain.Finding into the arbiter's view — used by
// durableCrossLensFold, whose comparand is a persisted store row rather than
// an in-run Candidate.
func findingDedupView(f domain.Finding) dedupCandidateView {
	return dedupCandidateView{Title: f.Title, Description: f.Description, File: f.File, Line: f.Line}
}

// dedupArbiterResponse is the wire shape RunJSON parses dedupArbiterSchema into.
type dedupArbiterResponse struct {
	Verdict   dedupVerdict `json:"verdict"`
	Reasoning string       `json:"reasoning"`
}

// dedupArbiterSchema constrains the response to a typed, closed verdict enum
// — the repo invariant that machine decisions never key on prose means "same
// defect" must be a field the caller switches on directly, not a boolean or
// free-text judgment the caller would have to parse out of Reasoning.
var dedupArbiterSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "verdict": {"type": "string", "enum": ["yes", "no", "unsure"], "description": "yes: confident the two reports describe the SAME underlying defect. no: confident they are DISTINCT defects. unsure: the evidence does not clearly support either."},
    "reasoning": {"type": "string", "minLength": 1, "description": "Brief justification citing what in the two descriptions and the code excerpt supports the verdict."}
  },
  "required": ["verdict", "reasoning"],
  "additionalProperties": false
}`)

// dedupArbiterSystemPrompt is fixed (no persona/sandbox variants): the
// arbiter is a single bounded classification turn with no tools, so none of
// the verifier system prompt's tool/sandbox paragraphs apply.
const dedupArbiterSystemPrompt = `You are a precise triage assistant. You will be shown two independently-reported bug candidates that landed at the same (or adjacent) code location and a code excerpt from that location. Decide whether they describe the SAME underlying defect (same root cause, same failure mode, just worded differently) or two DISTINCT defects that merely happen to sit near each other.

Answer "yes" ONLY when you are confident it is the same defect. Answer "no" when you are confident they are different defects. Answer "unsure" whenever the evidence does not clearly support either side — this is the SAFE default: an incorrect "yes" silently buries a real, distinct finding, while an incorrect "unsure" only costs an extra review. When genuinely in doubt, say unsure.

Respond with the required JSON only — no prose outside the JSON fields.`

// dedupArbiterTask builds the task message: both candidate views plus the
// shared code excerpt. Model-authored multi-line fields are fenced and
// single-line fields flattened, matching verifierTask's sanitization pattern
// for text crossing this prompt boundary.
func dedupArbiterTask(a, b dedupCandidateView, excerpt string) string {
	var sb strings.Builder
	sb.WriteString("Are candidate A and candidate B the same underlying defect?\n\n")
	fmt.Fprintf(&sb, "CANDIDATE A\n  file: %s\n  line: %d\n  title: %s\n", a.File, a.Line, util.FlattenField(a.Title))
	sb.WriteString(util.FenceBlock("DESCRIPTION_A", a.Description))
	fmt.Fprintf(&sb, "\nCANDIDATE B\n  file: %s\n  line: %d\n  title: %s\n", b.File, b.Line, util.FlattenField(b.Title))
	sb.WriteString(util.FenceBlock("DESCRIPTION_B", b.Description))
	sb.WriteString("\n")
	sb.WriteString(util.FenceBlock("CODE_EXCERPT", excerpt))
	return sb.String()
}

// dedupExcerptWindow is the number of source lines read on either side of the
// collision line for the arbiter's code excerpt.
const dedupExcerptWindow = 15

// dedupCodeExcerpt reads a bounded window of source around line from disk
// under root, for embedding directly in the arbiter task. The arbiter is a
// zero-tool bounded turn (not an agentic reader, unlike the split-verdict
// arbiter) — bugbot-ezmx.2 caps the cost at one completion per collision, so
// the code context is inlined rather than fetched via tool calls.
// Best-effort: any read failure (missing root, unreadable file, out-of-range
// line) degrades to an empty excerpt rather than failing the arbiter call —
// the arbiter still has both descriptions to judge from.
func dedupCodeExcerpt(root, file string, line, window int) string {
	if root == "" || line <= 0 {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(root, file))
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	start := line - 1 - window
	if start < 0 {
		start = 0
	}
	end := line + window
	if end > len(lines) {
		end = len(lines)
	}
	if start >= len(lines) || start >= end {
		return ""
	}
	var sb strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&sb, "%d: %s\n", i+1, lines[i])
	}
	return sb.String()
}

// runDedupArbiter spends one bounded, zero-tool RunJSON completion judging
// whether a and b describe the same defect. It builds its own runner (no
// tools, the shared verifier-role client and pool) exactly like the other
// funnel LLM stages. On any infrastructure or parse failure it returns
// dedupUnsure together with the error — never a silent merge — so a broken
// arbiter can only cost an extra kept duplicate, never bury a real finding.
func (f *Funnel) runDedupArbiter(ctx context.Context, client llm.Client, budget *budgetState, a, b dedupCandidateView, excerpt string) (dedupVerdict, int64, error) {
	runner := f.newAgentRunner(client, nil, dedupArbiterSystemPrompt, budget.verifyRunnerLimits(f.opts.Limits.VerifierLimits))
	var out dedupArbiterResponse
	outcome, err := runner.RunJSON(ctx, dedupArbiterTask(a, b, excerpt), dedupArbiterSchema, &out)
	var tokens int64
	if outcome != nil {
		tokens = outcome.Usage.InputTokens + outcome.Usage.OutputTokens
	}
	if err != nil {
		return dedupUnsure, tokens, err
	}
	switch out.Verdict {
	case dedupSame, dedupDistinct, dedupUnsure:
		return out.Verdict, tokens, nil
	default:
		// The schema's enum should prevent this, but a permissive client or
		// repair round-trip could still hand back something unrecognized —
		// never let an unrecognized value be treated as a merge.
		return dedupUnsure, tokens, fmt.Errorf("dedup arbiter: unrecognized verdict %q", out.Verdict)
	}
}

// dedupArbiterConfig bundles everything triageState needs to invoke the LLM
// dedup arbiter. The zero value (a nil *dedupArbiterConfig, the default
// triageState.dedupArbiter from newTriageState) disables the arbiter
// entirely: every existing newTriageState(snap) call site — every test in
// this package — keeps today's fall-through-to-new-primary behavior for free,
// since the feature is opt-in via ts.dedupArbiter, wired only from run() in
// run_pipeline.go.
type dedupArbiterConfig struct {
	f      *Funnel
	client llm.Client
	budget *budgetState
	// root is snap.Root, used to read the code excerpt from disk. Empty
	// degrades dedupCodeExcerpt to an empty excerpt (the arbiter still judges
	// on the two descriptions alone).
	root string
	// cap is the per-scan invocation ceiling (DefaultDedupArbiterCap unless the
	// caller overrides it). invoked is the running count this scan;
	// triageState.process runs on a single goroutine (the triage consumer), so
	// no lock is needed.
	cap     int
	invoked int
}

// dedupVerdictFor is the shared cap-checked entry point both collision sites
// (triage_streaming.go's step 5 cluster loop and durableCrossLensFold) use: it
// reserves one invocation slot against the per-scan cap (or reports the cap
// already exhausted), runs the bounded arbiter turn, and folds the outcome
// into stats. An empty-string return means the arbiter did not run at all (no
// arbiter configured, or the cap was already exhausted) — the caller MUST
// treat that identically to a "no" verdict: pass through, keep both.
func (ts *triageState) dedupVerdictFor(ctx context.Context, a, b dedupCandidateView, stats *Stats) dedupVerdict {
	d := ts.dedupArbiter
	if d == nil {
		return ""
	}
	if d.invoked >= d.cap {
		stats.DedupArbiterSkippedCap++
		return ""
	}
	d.invoked++
	stats.DedupArbiterRuns++
	excerpt := dedupCodeExcerpt(d.root, b.File, b.Line, dedupExcerptWindow)
	verdict, tokens, err := d.f.runDedupArbiter(ctx, d.client, d.budget, a, b, excerpt)
	stats.DedupArbiterTokens += tokens
	if err != nil {
		stats.DedupArbiterFailures++
	}
	if verdict == dedupSame {
		stats.DedupArbiterMerges++
	}
	return verdict
}

package funnel

import (
	"fmt"
	"strings"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// Strategy is a search procedure for finder units, orthogonal to Lens (what
// defect class to hunt) — the strategy decides HOW the agent spends its turns
// over/around the chunk.
type Strategy struct {
	// Name is the stable strategy identifier. It appears in progress labels for
	// non-default strategies (lens@strategy) and in degradation keys.
	Name string
	// SystemClause is appended to the composed finder system prompt under a
	// "YOUR SEARCH STRATEGY (<name>):" heading. Empty for the default strategy
	// (sweep-wide composes nothing, preserving today's prompt byte-for-byte).
	SystemClause string
	// BuildTask builds the per-unit task message. nil means use finderTask
	// (the default file-list task).
	BuildTask func(files []string, leads []store.Lead) string
	// AppliesTo reports whether this strategy emits units for the given lens.
	AppliesTo func(lensName string) bool
	// Weight scales the lens's effective yield for launch/degradation ranking
	// of this strategy's units. The default strategy has weight 1.0.
	Weight float64
}

// sweepWide is the default strategy: every lens, every chunk, no special
// traversal directive. SystemClause is empty so the composed prompt is
// byte-identical to the pre-strategy prompt.
var sweepWide = Strategy{
	Name:         "sweep-wide",
	SystemClause: "",
	BuildTask:    nil,
	AppliesTo:    func(string) bool { return true },
	Weight:       1.0,
}

// contractTraceDeepSystemClause is the verbatim system-prompt addition for the
// contract-trace-deep strategy.
const contractTraceDeepSystemClause = `The target files in your task are your STARTING SEED, not your audit boundary. Do not audit them line-by-line. Instead: (1) enumerate the contracts they DECLARE that have a far end elsewhere in the repository — configuration/option struct fields and the code that validates them, parameters or fields whose doc comments state sentinel semantics (a special value that disables, removes a limit, or matches everything), exported functions whose doc comments promise invariants, constants encoding protocol assumptions; (2) pick the most load-bearing of these (at most 8) and use find_references (fall back to grep) to visit EVERY site that reads, writes, validates, or documents each one; (3) at each site, compare what that site believes — from its code and comments — against what the declaration site enforces and documents. Report a bug when two sites disagree: a validator rejecting a value the docs define as meaningful, a consumer assuming semantics the producer does not guarantee, a doc comment promising what the code does not implement. Budget your turns for traversal: prefer read_symbol and narrow read_file ranges at many sites over whole-file reads.`

// contractTraceDeep is the first non-default strategy. It treats the task
// chunk as a seed rather than a boundary and traces declared contracts outward
// via find_references to hunt belief asymmetries between declaration and use
// sites.
var contractTraceDeep = Strategy{
	Name:         "contract-trace-deep",
	SystemClause: contractTraceDeepSystemClause,
	BuildTask:    buildContractTraceDeepTask,
	AppliesTo:    func(lensName string) bool { return lensName == "api-contract-misuse" },
	Weight:       0.9,
}

// buildContractTraceDeepTask builds the finder task for the contract-trace-deep
// strategy. The chunk files are framed as SEED FILES (a starting point for
// contract tracing) rather than an audit boundary. The CROSS-LENS LEADS section
// is rendered via the same shared helper as finderTask so the one-item-per-line
// lead-list format is preserved across both call sites.
func buildContractTraceDeepTask(files []string, leads []store.Lead) string {
	var b strings.Builder
	b.WriteString("Trace the contracts declared in these SEED files to their far ends across the repository, following your search strategy.\n\n")
	b.WriteString("SEED FILES:\n")
	for _, f := range files {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	appendLeadsSection(&b, leads)
	return b.String()
}

// appendLeadsSection renders the CROSS-LENS LEADS block when leads is non-empty.
// Factored out so both finderTask and buildContractTraceDeepTask share the exact
// same newline-flattening logic — that flattening protects the
// one-item-per-line format of the lead list (a value's newlines must not
// fabricate extra bullet rows or break out of this section's framing), and
// must not fork.
func appendLeadsSection(b *strings.Builder, leads []store.Lead) {
	if len(leads) == 0 {
		return
	}
	b.WriteString("\nCROSS-LENS LEADS (suspicions posted by other lenses' agents in earlier runs; investigate ones relevant to your focus, they may be wrong):\n")
	for _, ld := range leads {
		// The note is model-authored free text from a previous run; flatten
		// newlines so a note can never fabricate extra lead lines or break
		// out of this section's framing.
		note := strings.Join(strings.Fields(ld.Note), " ")
		fmt.Fprintf(b, "  - from %s: %s:%d — %s\n", ld.PosterLens, ld.File, ld.Line, note)
	}
}

// stateTraceDeepSystemClause is the verbatim system-prompt addition for the
// state-trace-deep strategy. It reframes the chunk as a starting point for
// tracing shared mutable state and lifecycle-managed resources to every
// access site, so the agent hunts for defects that live only in the
// cross-file join.
const stateTraceDeepSystemClause = `The target files in your task are your STARTING SEED, not your audit boundary. Do not audit them line-by-line. Instead: (1) enumerate the SHARED MUTABLE STATE and lifecycle-managed resources the seed declares — fields guarded by a mutex/lock, package-level/global vars, shared maps/slices/channels, reference-counted or pooled objects, anything with acquire/release, lock/unlock, open/close, or start/stop pairing; (2) pick the most load-bearing of these (at most 8) and use find_references (fall back to grep) to visit EVERY site that reads, writes, locks, unlocks, acquires, releases, or closes each one — ACROSS FILES; (3) at each site, check that the discipline holds: every mutation under the lock that other sites hold, every acquire paired with a release on ALL paths including early-return and error/panic paths, no site assuming an invariant another site can violate. Report a bug when sites DISAGREE: a write missing the lock its siblings hold, a path that skips release, a lifecycle ordering two sites disagree on. Emphasize: the defect lives only in the cross-file JOIN — any single site viewed in isolation looks correct. Budget your turns for traversal: prefer read_symbol and narrow read_file ranges at many sites over whole-file reads.`

// stateTraceDeep is the second non-default strategy. It treats the task chunk
// as a seed and traces shared mutable state and lifecycle-managed resources
// outward via find_references to hunt discipline asymmetries between access
// sites — the canonical cross-file join that produces data races and leaks.
//
// Weight 0.85 is a reasoned prior: live per-unit yield data on the deep axis
// is still thin (mi5.12's dogfood produced traversal-born hypotheses but zero
// verified findings on that run), so 0.85 places this strategy just below
// contract-trace-deep's 0.9. Under budget pressure it is shed just after
// contract-trace-deep. Correct from mi5.10 data when available.
var stateTraceDeep = Strategy{
	Name:         "state-trace-deep",
	SystemClause: stateTraceDeepSystemClause,
	BuildTask:    buildStateTraceDeepTask,
	AppliesTo:    func(lensName string) bool { return lensName == "concurrency" || lensName == "resource-leaks" },
	Weight:       0.85,
}

// buildStateTraceDeepTask builds the finder task for the state-trace-deep
// strategy. The chunk files are framed as SEED FILES (a starting point for
// shared-state and resource-lifecycle tracing) rather than an audit boundary.
// The CROSS-LENS LEADS section is rendered via the same shared helper as
// finderTask and buildContractTraceDeepTask so the newline-flattening
// prompt-injection guard is never forked.
func buildStateTraceDeepTask(files []string, leads []store.Lead) string {
	var b strings.Builder
	b.WriteString("Trace the shared mutable state and resource lifecycles declared in these SEED files to every access site across the repository, following your search strategy.\n\n")
	b.WriteString("SEED FILES (a starting point for shared-state and resource-lifecycle tracing):\n")
	for _, f := range files {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	appendLeadsSection(&b, leads)
	return b.String()
}

// builtinStrategies returns all builtin strategies in definition order. The
// first entry is always sweepWide (the default).
func builtinStrategies() []Strategy {
	return []Strategy{sweepWide, contractTraceDeep, stateTraceDeep}
}

// composeFinderSystemPrompt composes the full system prompt for one finder
// unit: the persona+lens+manifestations prompt, then the strategy's clause
// under a labeled heading. For the default strategy (empty SystemClause)
// nothing is appended, so the output is byte-identical to the pre-strategy
// finderSystemPrompt — the invariant the strategy axis must preserve.
func composeFinderSystemPrompt(persona string, l Lens, langs []ingest.Language, st Strategy) string {
	sysprompt := finderSystemPrompt(persona, l, langs)
	if st.SystemClause != "" {
		sysprompt += "\n\nYOUR SEARCH STRATEGY (" + st.Name + "):\n" + st.SystemClause
	}
	return sysprompt
}

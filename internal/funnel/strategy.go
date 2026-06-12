package funnel

import (
	"fmt"
	"strings"

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
// is rendered via the same shared helper as finderTask so the newline-flattening
// prompt-injection guard is never forked.
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
// same newline-flattening logic — that flattening is a prompt-injection guard and
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

// builtinStrategies returns all builtin strategies in definition order. The
// first entry is always sweepWide (the default).
func builtinStrategies() []Strategy {
	return []Strategy{sweepWide, contractTraceDeep}
}

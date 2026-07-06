// hypothesize_tasks.go holds the unit-class degradation ranking and custom-task builders extracted from hypothesize.go for readability.
// Pure code motion: no logic changes.

package funnel

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// diffIntentDiffCap is the maximum number of bytes of unified diff embedded in
// a diff-intent finder task. Beyond this limit the diff is truncated with an
// explicit marker so the model knows it is reading a partial diff, not the full
// change. 48 KB is large enough to cover most single-commit diffs while keeping
// the finder's context window from being dominated by raw diff bytes.
const diffIntentDiffCap = 48 * 1024

// diffIntentMsgCap is the maximum number of bytes of commit message embedded in
// a diff-intent finder task. Beyond this limit the message is truncated with an
// explicit marker. 4 KB comfortably covers any reasonable commit message body.
const diffIntentMsgCap = 4 * 1024

// unit is one finder work item: a (lens × strategy × chunk) triple. The
// customTask field is non-empty only for the diff-intent lens, which uses a
// pre-built task string rather than the standard file-list format. The struct
// is package-level so observability.go can reference it in the recording
// helpers without duplicating the definition.
type unit struct {
	lens       Lens
	strategy   Strategy
	files      []string
	langs      []ingest.Language // the chunk's language set, for prompt composition
	leads      []store.Lead      // pre-fetched leads for this lens, already consumed
	customTask string            // non-empty for diff-intent: overrides task(files, leads)
}

// buildUnits builds the unit-of-work list as (lens × strategy × chunk)
// triples in CHUNK-MAJOR order: every active lens (and applicable strategy)
// visits chunk 0 before any lens visits chunk 1. Within a chunk, lenses
// iterate in the caller-supplied yield order and strategies in builtin order
// (sweep-wide before deep).
//
// Chunk-major interleaving is a latency policy, not a budget policy: it gives
// every defect class — including low-yield lenses, whose units previously
// launched only after every higher-yield lens had covered the whole repo —
// running coverage within the first chunks of the sweep, so time-to-first-
// finding no longer scales with a lens's position in the yield ranking.
// Budget degradation is unaffected: the launch-loop gate checks each unit's
// (lens, strategy) class against the yield-ranked survivor set at launch
// time (degradedUnitClasses), which never depended on launch order. Under
// pressure the spend now distributes across all classes up to the soft
// threshold instead of exhausting the top lenses first; past the threshold
// only survivor-class units launch, exactly as before. Chunks arrive in the
// sweep's anti-starvation order (run.go), so the hottest/stalest files get
// full multi-lens coverage first.
//
// For each lens × chunk pair the default strategy (sweep-wide) is emitted
// exactly as before the strategy axis; additionally, each non-default builtin
// strategy that AppliesTo the lens emits one extra unit per chunk.
//
// diff-intent never gets chunk-based units here: it is either absent (sweeps,
// nil ChangeContext) or emitted by the caller as exactly ONE custom task
// prepended to the list. Skipping it ensures zero tasks from this lens on
// sweeps while still allowing the degradation logic to treat it as part of
// the set.
func buildUnits(lenses []Lens, strategies []Strategy, chunks []fileChunk, leadsByLens map[string][]store.Lead) []unit {
	var units []unit
	for _, c := range chunks {
		for _, l := range lenses {
			// Custom-unit lenses (per-chunk work would be the wrong
			// shape): diff-intent fires one task per commit-scoped run
			// and cross-language-boundary fires one task per seam.
			// Both are emitted by the caller as custom units adjacent
			// to this list, NEVER as chunk units, so skipping here
			// guarantees no per-chunk contamination and zero tasks on
			// runs where the caller chose not to emit them (e.g.
			// sweep with no seams).
			if l.Name == "diff-intent" || l.Name == "cross-language-boundary" {
				continue
			}
			if !lensAppliesTo(l, c.langs) {
				continue
			}
			for _, s := range strategies {
				if !s.AppliesTo(l.Name) {
					continue
				}
				units = append(units, unit{
					lens:     l,
					strategy: s,
					files:    c.files,
					langs:    c.langs,
					leads:    leadsByLens[l.Name],
				})
			}
		}
	}
	return units
}

// lensStrategyClass identifies a (lens × strategy) unit-class for degradation
// ranking. It carries the weight so the ranking can be computed without
// re-fetching the strategy.
type lensStrategyClass struct {
	lensName     string
	strategyName string
	weight       float64
}

// degradedUnitClasses returns the set of unit-class keys (lens@strategy) that
// survive budget degradation. It ranks each (lens, strategy) class by effective
// yield = per-language lens yield × strategy.Weight, descending, and keeps the
// head degradedLensCount classes.
//
// The sort is stable and compares ONLY the score: equal-score classes keep
// their input order, which callers supply in lensesByYield order (wide before
// deep within a lens). That makes the equal-yield tiebreak identical to the
// pre-strategy degradedLensNames semantics — head-of-lensesByYield — rather
// than introducing a new (e.g. alphabetical) tiebreak that would silently
// change survivors the next time the yield tables are retuned. A deep
// unit-class (weight 0.9) ranks just below its lens's sweep-wide class and is
// therefore shed first under pressure — intended behavior.
//
// CRITICAL INVARIANT: with only sweep-wide in play (weight 1.0), the survivors
// must be exactly the top degradedLensCount lenses by yield — identical to the
// pre-strategy degradedLensNames behavior.
//
// Callers must pass only classes that actually emitted units this run (see
// hypothesize): a zero-unit lens must never occupy a survivor slot.
func degradedUnitClasses(classes []lensStrategyClass, langs []ingest.Language) map[string]bool {
	type ranked struct {
		key   string
		score float64
	}
	r := make([]ranked, len(classes))
	for i, c := range classes {
		r[i] = ranked{
			key:   c.lensName + "@" + c.strategyName,
			score: float64(effectiveYield(c.lensName, langs)) * c.weight,
		}
	}
	sort.SliceStable(r, func(i, j int) bool {
		return r[i].score > r[j].score
	})
	keep := make(map[string]bool, degradedLensCount)
	for i, rc := range r {
		if i >= degradedLensCount {
			break
		}
		keep[rc.key] = true
	}
	return keep
}

// unitLabel returns the progress label for a finder unit. Default strategy
// (sweep-wide) units use the bare lens name to preserve existing output.
// Non-default strategy units use "lens@strategy" so they are distinguishable.
func unitLabel(lensName, strategyName string) string {
	if strategyName == sweepWide.Name {
		return lensName
	}
	return lensName + "@" + strategyName
}

// diffIntentLens returns the Lens descriptor for the diff-intent lens.
// It is defined as a package-level var (builtinDiffIntentLens in lens.go)
// so this lookup is zero-cost and cannot panic.
func diffIntentLens() Lens {
	return builtinDiffIntentLens
}

// buildDiffIntentTask constructs the finder task message for the diff-intent
// lens. It embeds the commit message (capped at diffIntentMsgCap), the unified
// diff (capped at diffIntentDiffCap bytes with an explicit truncation marker),
// the files changed in the commit, and the blast-radius dependents (targets
// beyond the changed set) so the agent can check call sites without extra tool
// calls. The task is self-contained: the agent still has read-only tools and
// can follow up with find_references if needed.
//
// targets is the full blast-radius file list as seen by hypothesize (already
// expanded and snapshot-intersected). The blast-radius dependent section is
// built by subtracting cc.ChangedFiles from targets, so the prompt correctly
// identifies "files that MAY DEPEND ON the changed code" rather than the changed
// files themselves.
func buildDiffIntentTask(cc *ChangeContext, targets []string) string {
	var b strings.Builder
	b.WriteString("Audit this commit for intent-vs-implementation mismatches and broken caller assumptions.\n\n")

	b.WriteString("COMMIT MESSAGE:\n")
	if cc.Message != "" {
		msg := cc.Message
		if len(msg) > diffIntentMsgCap {
			msg = msg[:diffIntentMsgCap] + "\n[message truncated at 4KB]"
		}
		b.WriteString(msg)
	} else {
		b.WriteString("(not available)")
	}
	b.WriteString("\n\n")

	b.WriteString("UNIFIED DIFF:\n")
	if len(cc.Diff) == 0 {
		b.WriteString("(not available)\n")
	} else if len(cc.Diff) > diffIntentDiffCap {
		b.Write(cc.Diff[:diffIntentDiffCap])
		b.WriteString("\n[diff truncated at 48KB]\n")
	} else {
		b.Write(cc.Diff)
		b.WriteByte('\n')
	}

	// Files changed directly in this commit.
	if len(cc.ChangedFiles) > 0 {
		b.WriteString("\nFILES CHANGED IN THIS COMMIT:\n")
		for _, f := range cc.ChangedFiles {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}

	// Blast-radius dependents: targets that are NOT in the changed set. These are
	// files in scope that may depend on the changed code and whose caller
	// assumptions the change might break.
	changedSet := make(map[string]bool, len(cc.ChangedFiles))
	for _, f := range cc.ChangedFiles {
		changedSet[f] = true
	}
	var dependents []string
	for _, t := range targets {
		if !changedSet[t] {
			dependents = append(dependents, t)
		}
	}
	if len(dependents) > 0 {
		b.WriteString("\nBLAST-RADIUS DEPENDENTS (files in scope that may depend on the changed code):\n")
		for _, f := range dependents {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}

	b.WriteString("\nFor each finding: read the relevant call sites with find_references before reporting.\n")
	b.WriteString("Finding nothing is the expected outcome for most commits.\n")
	return b.String()
}

// crossLanguageBoundaryLens returns the Lens descriptor for the
// cross-language-boundary lens. It is defined as a package-level var
// (builtinCrossLanguageBoundaryLens in lens.go) so this lookup is
// zero-cost and cannot panic.
func crossLanguageBoundaryLens() Lens {
	return builtinCrossLanguageBoundaryLens
}

// buildSeamTask constructs the finder task message for the cross-language-
// boundary lens. It names the seam kind/key and every side file with its
// language and line, so the agent can read both sides end-to-end and report
// contract mismatches. The task is self-contained: the agent has read-only
// tools and can follow up with find_references on either side.
//
// The seam is a contract surface, not a commit, so there is no diff, no
// message, and no leads: the input is the two-sides contract. "Finding
// nothing" is the expected outcome on the vast majority of seams; only
// genuine cross-language drift surfaces a candidate.
func buildSeamTask(s ingest.Seam) string {
	var b strings.Builder
	switch s.Kind {
	case ingest.SeamDataFile:
		fmt.Fprintf(&b, "Audit this shared data file for cross-language contract mismatches.\n\n")
		fmt.Fprintf(&b, "SHARED DATA FILE: %s\n\n", s.Key)
	case ingest.SeamEnvVar:
		fmt.Fprintf(&b, "Audit this shared environment variable for cross-language contract mismatches.\n\n")
		fmt.Fprintf(&b, "SHARED ENVIRONMENT VARIABLE: %s\n\n", s.Key)
	default:
		fmt.Fprintf(&b, "Audit this cross-language seam.\n\nKIND: %s\nKEY: %s\n\n", s.Kind, s.Key)
	}
	b.WriteString("SIDES (every participating file; both sides must be read end-to-end):\n")
	for _, side := range s.Sides {
		if side.Line > 0 {
			fmt.Fprintf(&b, "  - %s [%s] (first reference at line %d)\n", side.File, side.Language, side.Line)
		} else {
			fmt.Fprintf(&b, "  - %s [%s]\n", side.File, side.Language)
		}
	}
	b.WriteString("\nFor each finding: confirm the mismatch by reading BOTH sides end-to-end " +
		"(use read_file on each named file) before reporting. A mismatch you have " +
		"not verified on both sides is not a finding.\n")
	b.WriteString("Finding nothing is the expected outcome for the vast majority of seams: " +
		"report an empty list when the two sides agree on the contract.\n")
	return b.String()
}

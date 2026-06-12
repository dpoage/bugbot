package funnel

import (
	"strings"
)

// tierVerified is the confidence tier assigned to candidates that survive
// adversarial verification but have not been reproduced. Tier 1 requires a
// sandboxed failing test (a later stage); this stage tops out at Tier 2.
const tierVerified = 2

// tierSuspected is the tier for candidates that passed triage but never
// completed adversarial verification because the run hit its hard budget. They
// are persisted (not dropped) so a human can review them, but at lower
// confidence than a verified finding.
const tierSuspected = 3

// budgetStoppedReasoning is the verification trace recorded on a Tier 3
// suspected finding, making clear why it was not verified.
const budgetStoppedReasoning = "Verification skipped: the run reached its hard token budget before this candidate " +
	"could be challenged by refuters. It is recorded as Tier 3 suspected (unverified) so it is not silently " +
	"dropped. Re-run the scan with more budget to verify it."

// appendCorroboration appends a human-readable corroboration note to a
// verification reasoning trace when one or more other lenses independently
// reported the same defect (collapsed in triage's location-based dedup). It
// returns reasoning unchanged when there is no corroboration, so non-merged
// findings are unaffected. The note is informational only — corroboration does
// not raise confidence.
func appendCorroboration(reasoning string, lenses []string) string {
	if len(lenses) == 0 {
		return reasoning
	}
	note := "Corroborated by lenses: " + strings.Join(lenses, ", ")
	if reasoning == "" {
		return note
	}
	return strings.TrimRight(reasoning, "\n") + "\n" + note
}

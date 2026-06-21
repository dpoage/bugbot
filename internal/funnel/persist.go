package funnel

import (
	"strings"
)

// budgetStoppedReasoning is the verification trace recorded on a Tier 3
// suspected finding, making clear why it was not verified.
const budgetStoppedReasoning = "Verification skipped: the run reached its hard token budget before this candidate " +
	"could be challenged by refuters. It is recorded as Tier 3 suspected (unverified) so it is not silently " +
	"dropped. Re-run the scan with more budget to verify it."

// verifyFailedReasoning is the verification trace recorded on a Tier 3 suspected
// finding whose refuter panel reached NO genuine verdict — every seat either
// abstained or failed (infrastructure/parse failure), with at least one
// failure. The candidate was never actually challenged, so it is kept suspected
// (unverified) rather than promoted as a confident survivor. The next scan
// re-discovers and re-verifies it; a persistent failure (e.g. a misconfigured
// or unreachable verifier provider) keeps it suspected until verification can
// run (bugbot-8rd).
const verifyFailedReasoning = "Verification incomplete: every refuter in the panel failed to return a " +
	"verdict (infrastructure or parse failures), so this candidate was never actually challenged. It is recorded " +
	"as Tier 3 suspected (unverified) rather than reported as verified. Check that the verifier provider is " +
	"reachable and configured, then re-run the scan to verify it."

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

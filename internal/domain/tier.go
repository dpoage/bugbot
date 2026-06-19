package domain

// Tier is the confidence tier of a finding: how strongly the pipeline believes
// the bug is real, set by how far it advanced through verification.
//
// IMPORTANT: evidence strength is not the same as the numeric value. Tier 0
// (fix-witnessed) was added after the original 1..3 scale and is, semantically,
// the STRONGEST evidence — a generated patch made a failing test pass. The
// scores returned below are preserved verbatim from the pre-centralization code
// (store.findingConfidence, report.tierName, sarif.tierLevel) so that adopting
// this type changes no behavior. The known mismatch for tier 0 — it currently
// scores the WEAKEST BaseConfidence (0.20) and the "note" SARIF level — is left
// intact here on purpose; correcting it is a product decision tracked in
// bugbot-0nc.2.
type Tier uint8

const (
	TierFixWitnessed Tier = 0 // T0: a generated fix made a failing test pass.
	TierReproduced   Tier = 1 // T1: a sandboxed failing test was produced.
	TierVerified     Tier = 2 // T2: a refuter panel failed to disprove it.
	TierSuspected    Tier = 3 // T3: reported by a finder, not yet verified.
)

// Label is the human-facing tier label, e.g. "T2 Verified".
func (t Tier) Label() string {
	switch t {
	case TierFixWitnessed:
		return "T0 Fix-witnessed"
	case TierReproduced:
		return "T1 Reproduced"
	case TierVerified:
		return "T2 Verified"
	case TierSuspected:
		return "T3 Suspected"
	default:
		return "T? Unknown"
	}
}

// Level maps the tier to a SARIF result level. Preserved from sarif.tierLevel:
// reproduced -> error, verified -> warning, everything else -> note.
func (t Tier) Level() string {
	switch t {
	case TierReproduced:
		return "error"
	case TierVerified:
		return "warning"
	default:
		return "note"
	}
}

// BaseConfidence is the tier's contribution to a finding's confidence score in
// [0,1], before any severity or corroboration adjustment. Preserved from
// store.findingConfidence: reproduced -> 0.80, verified -> 0.50, everything
// else (including fix-witnessed and suspected) -> 0.20.
func (t Tier) BaseConfidence() float64 {
	switch t {
	case TierReproduced:
		return 0.80
	case TierVerified:
		return 0.50
	default:
		return 0.20
	}
}

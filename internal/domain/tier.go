package domain

// Tier is the confidence tier of a finding: how strongly the pipeline believes
// the bug is real, set by how far it advanced through verification.
//
// Evidence strength ordering (strongest first): TierFixWitnessed (0) is the
// STRONGEST evidence — a generated patch made a failing test pass — followed by
// TierReproduced (1), TierVerified (2), and TierSuspected (3). The numeric value
// is NOT the evidence rank; lower numbers happen to be stronger for 0..3, but
// callers MUST use the domain methods (BaseConfidence, Level) rather than
// comparing numeric values for strength ordering.
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

// Level maps the tier to a SARIF result level. Fix-witnessed and reproduced are
// the two strongest tiers and both map to "error"; verified maps to "warning";
// everything else (suspected, unknown) maps to "note".
func (t Tier) Level() string {
	switch t {
	case TierFixWitnessed:
		return "error"
	case TierReproduced:
		return "error"
	case TierVerified:
		return "warning"
	default:
		return "note"
	}
}

// BaseConfidence is the tier's contribution to a finding's confidence score in
// [0,1], before any severity or corroboration adjustment. Fix-witnessed is the
// strongest (0.90), then reproduced (0.80), verified (0.50), and suspected/unknown
// (0.20).
func (t Tier) BaseConfidence() float64 {
	switch t {
	case TierFixWitnessed:
		return 0.90
	case TierReproduced:
		return 0.80
	case TierVerified:
		return 0.50
	default:
		return 0.20
	}
}

package store

import "fmt"

// NeedsHumanReason is the explicit cause for the NeedsHuman flag. It replaces
// the prior flag-combination inference (NeedsHuman && ReproWitness != "" etc.)
// that downstream code branched on. Exactly one value applies when NeedsHuman
// is true; the zero value means NeedsHuman is false.
type NeedsHumanReason string

const (
	// NeedsHumanReasonNone means the finding does not need human review
	// (NeedsHuman is false). This is the zero value.
	NeedsHumanReasonNone NeedsHumanReason = ""

	// NeedsHumanReasonProverExhausted means the patch-prover used up its attempt
	// budget without finding a minimal fix. The finding is already Tier-1
	// (ReproPath set); a human must inspect the fix candidates.
	NeedsHumanReasonProverExhausted NeedsHumanReason = "prover_exhausted"

	// NeedsHumanReasonBelowQuorum means the verifier survivor fell below the
	// genuine-verdict quorum floor: too few verifier seats actually ruled on it.
	// The finding stays at its original tier (typically T2/T3) with no ReproPath;
	// it may have a ReproWitness bundle from the witness path.
	NeedsHumanReasonBelowQuorum NeedsHumanReason = "below_quorum"
)

// ErrIllegalTransition is returned when a state mutation would produce an
// invariant-violating combination of lifecycle fields.
var ErrIllegalTransition = fmt.Errorf("store: illegal finding state transition")

// ValidateFindingState enforces the co-variance invariants documented in the
// original audit comment (internal/repro/promote.go:60-65). Call it on any
// Finding before persisting via UpsertFinding; UpsertFinding calls it
// automatically at the chokepoint.
//
// Enforced invariants:
//
//	(a) T0 (fix-witnessed) requires ReproPath (the fix witness bundle implies a
//	    prior reproduction). Witness-only (ReproWitness without ReproPath) cannot
//	    be T0.
//
//	(b) T1 (reproduced) requires ReproPath.
//
//	(c) ReproWitness implies NeedsHuman (a witness bundle is only written for
//	    the below-quorum path; a non-NeedsHuman finding that somehow has a
//	    ReproWitness is a logic error).
//
//	(d) NeedsHuman requires a non-None NeedsHumanReason (the cause must be
//	    recorded explicitly).
//
//	(e) NeedsHumanReason != None implies NeedsHuman (the reason field must not
//	    be set when the flag is clear).
//
//	(f) ProverExhausted requires ReproPath (patch-prover only runs after T1
//	    promotion, which sets ReproPath).
//
//	(g) T0 conflicts with NeedsHuman: a fix-witnessed finding is fully resolved
//	    and must not be flagged for human review.
func ValidateFindingState(f Finding) error {
	tier := int(f.Tier)

	// (g) T0 + NeedsHuman is contradictory.
	if tier == 0 && f.NeedsHuman {
		return fmt.Errorf("%w: T0 (fix-witnessed) and NeedsHuman are mutually exclusive", ErrIllegalTransition)
	}

	// (a) T0 requires ReproPath.
	if tier == 0 && f.ReproPath == "" {
		return fmt.Errorf("%w: T0 requires ReproPath (no fix without a repro)", ErrIllegalTransition)
	}

	// (b) T1 requires ReproPath.
	if tier == 1 && f.ReproPath == "" {
		return fmt.Errorf("%w: T1 (reproduced) requires ReproPath", ErrIllegalTransition)
	}

	// (c) ReproWitness implies NeedsHuman.
	if f.ReproWitness != "" && !f.NeedsHuman {
		return fmt.Errorf("%w: ReproWitness set but NeedsHuman is false (witness path is only for below-quorum findings)", ErrIllegalTransition)
	}

	// (d) NeedsHuman requires a reason.
	if f.NeedsHuman && f.NeedsHumanReason == NeedsHumanReasonNone {
		return fmt.Errorf("%w: NeedsHuman=true requires a NeedsHumanReason", ErrIllegalTransition)
	}

	// (e) NeedsHumanReason != None requires NeedsHuman.
	if f.NeedsHumanReason != NeedsHumanReasonNone && !f.NeedsHuman {
		return fmt.Errorf("%w: NeedsHumanReason=%q set but NeedsHuman is false", ErrIllegalTransition, f.NeedsHumanReason)
	}

	// (f) ProverExhausted requires ReproPath.
	if f.NeedsHumanReason == NeedsHumanReasonProverExhausted && f.ReproPath == "" {
		return fmt.Errorf("%w: NeedsHumanReason=prover_exhausted requires ReproPath (prover runs after T1 promotion)", ErrIllegalTransition)
	}

	return nil
}

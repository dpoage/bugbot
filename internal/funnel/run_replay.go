// run_replay.go holds the candidate-reconstruction helpers for WAL/reverify replay extracted from run.go for readability.
// Pure code motion: no logic changes.

package funnel

import (
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/store"
)

// pendingToCandidate rebuilds a funnel Candidate from a persisted
// pending_candidates row for replay. Fingerprint is intentionally left unset:
// the triage consumer recomputes it (domain.Fingerprint) and re-runs the
// scope/dedup/suppression checks, so a replayed candidate is re-anchored to the
// current snapshot exactly like a fresh one. PendingID carries the WAL row's
// primary key so the terminal-fate handlers delete it.
func pendingToCandidate(pc store.PendingCandidate) Candidate {
	return Candidate{
		Lens:                pc.Lens,
		File:                pc.File,
		Line:                pc.Line,
		Title:               pc.Title,
		Description:         pc.Description,
		Severity:            normalizeSeverity(domain.Severity(pc.Severity)),
		Evidence:            pc.Evidence,
		Confidence:          normalizeConfidence(domain.Confidence(pc.Confidence)),
		CorroboratingLenses: pc.CorroboratingLenses,
		PendingID:           pc.ID,
	}
}

// findingToCandidate rebuilds a funnel Candidate from a durable OPEN finding
// for re-verification: a Tier-3 suspected orphan (ReverifySuspected) or an
// under-validated Tier-2 survivor (ReverifyUnderValidated). The candidate is
// routed through the same triage + verify pipeline as a WAL-replayed one, but
// is materially different: there is no pending_candidates WAL row (PendingID
// is ""), and the durable open finding row is the unit of state that must be
// transitioned when the verifier refutes the candidate (see Reverify and the
// verify-stream KILL region). PendingID is intentionally left ""; the
// triage consumer does not need to re-anchor the fingerprint (the finding
// already carries it), so Fingerprint is set from the finding to keep the
// verify-stage signals (e.g. SignalPersisted) consistent across the run.
//
// Confidence is forced to ConfidenceHigh: a low-confidence candidate is
// dropped at triage step 1 (triage_streaming.go:305), and a replayed durable
// finding is by definition worth re-judging — its prior triage passed, and
// only the verify stage was halted (T3) or degraded (under-validated T2), so
// we deliberately keep it out of the low band.
// The Sites slice is converted from []domain.Site to []Site to mirror the
// pendingToCandidate shape (Candidate uses the funnel package's Site type,
// pendingToCandidate's input already only carries locations as fields).
func findingToCandidate(fi domain.Finding) Candidate {
	sites := make([]Site, len(fi.Sites))
	for i, s := range fi.Sites {
		sites[i] = Site{File: s.File, Line: s.Line}
	}
	return Candidate{
		Lens:                fi.Lens,
		File:                fi.File,
		Line:                fi.Line,
		Title:               fi.Title,
		Description:         fi.Description,
		Severity:            normalizeSeverity(fi.Severity),
		Evidence:            fi.Reasoning,
		Confidence:          domain.ConfidenceHigh,
		Fingerprint:         fi.Fingerprint,
		LocusKey:            fi.LocusKey, // recomputed in triage process(); carried for round-trip fidelity
		DefectKind:          fi.DefectKind,
		Subject:             fi.Subject,
		CorroboratingLenses: fi.CorroboratingLenses,
		Sites:               sites,
		Reverify:            true,
	}
}

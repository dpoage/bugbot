package repro

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
)

// TestPromoteOne_NeedsHuman_WitnessesNotPromotes covers bugbot-w1bh: a
// below-quorum (NeedsHuman) finding whose repro DEMONSTRATES the bug records a
// non-promoting witness (ReproWitness) — it is NOT promoted to Tier-1, does NOT
// set ReproPath, and does NOT trigger the patch-prover cascade (even with
// PatchProver enabled). The human gate stands; the reviewer just gains a bundle.
func TestPromoteOne_NeedsHuman_WitnessesNotPromotes(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	repoDir := newRepoDir(t)

	// A below-quorum survivor: seeded Tier-2, then flagged NeedsHuman.
	finding := seedFinding(t, st)
	finding.NeedsHuman = true
	finding.NeedsHumanReason = domain.NeedsHumanReasonBelowQuorum
	finding, err := st.UpsertFinding(ctx, finding)
	if err != nil {
		t.Fatalf("flag needs_human: %v", err)
	}
	if !finding.NeedsHuman {
		t.Fatal("setup: finding is not NeedsHuman")
	}

	client := newScriptedClient(planBody(t, goodPlan()))
	// PatchProver enabled on purpose: the witness path must STILL skip the
	// patch-prover cascade for a NeedsHuman finding.
	r, err := New(client, demonstratingSandbox(), repoDir, Options{ArtifactDir: t.TempDir(), PatchProver: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	outcome, err := r.PromoteOne(ctx, st, finding)
	if err != nil {
		t.Fatalf("PromoteOne: %v", err)
	}

	// Outcome: witnessed, not promoted; patch-prover never ran.
	if !outcome.Witnessed {
		t.Errorf("outcome.Witnessed = false, want true (a below-quorum repro must witness)")
	}
	if outcome.Promoted {
		t.Errorf("outcome.Promoted = true, want false (a witness must not promote)")
	}
	if outcome.FixWitnessed {
		t.Errorf("outcome.FixWitnessed = true, want false (patch-prover must not run for a witness)")
	}
	if outcome.ArtifactPath == "" {
		t.Errorf("outcome.ArtifactPath empty, want the witness bundle path")
	}

	// Store: witness recorded, tier/repro_path untouched, still NeedsHuman.
	got, err := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ReproWitness == "" {
		t.Errorf("store repro_witness empty, want the bundle path")
	}
	if got.ReproWitness != outcome.ArtifactPath {
		t.Errorf("store repro_witness=%q != outcome.ArtifactPath=%q", got.ReproWitness, outcome.ArtifactPath)
	}
	if got.Tier != finding.Tier {
		t.Errorf("witness changed tier: got %d, want %d (no promotion)", got.Tier, finding.Tier)
	}
	if got.ReproPath != "" {
		t.Errorf("witness set repro_path=%q, want empty (no promotion)", got.ReproPath)
	}
	if !got.NeedsHuman {
		t.Errorf("witness cleared needs_human, want still true (human gate stands)")
	}
}

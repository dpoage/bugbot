package repro

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
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

// witnessOnlyPlan returns a plan whose command detects as
// ecosystem.EcosystemUnknown (direct script execution — "./repro.cjs", no
// recognized launcher token), which has no execution-witness coverage
// format: a demonstrated run comes back Promoted=true with
// WitnessOnly=true (bugbot-qb4r layer b). The script REQUIRES the finding's
// target module (./src/app) so the hardened static gate — which since
// bugbot-9fac applies the TARGET FILE's language edge rule even for
// launcher-less/unknown commands (targetGateEcosystem) — sees an executable
// edge and lets the plan reach the sandbox.
func witnessOnlyPlan() Plan {
	return Plan{
		Files: map[string]string{"repro.cjs": `
const { paginate } = require("./src/app");
const rows = paginate([1, 2, 3], 1);
if (rows.length !== 3) {
	console.log("BUGBOT_REPRO_DEMONSTRATED");
	process.exit(1);
}
`},
		Cmd:    []string{"./repro.cjs"},
		Expect: "demonstrates the pagination off-by-one",
	}
}

// witnessOnlySandbox demonstrates the bug: marker printed, exit 1.
func witnessOnlySandbox() *sandbox.Mock {
	return sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "BUGBOT_REPRO_DEMONSTRATED\n",
	}})
}

// TestPromoteOne_WitnessOnly_PersistsWitness covers bugbot-njb8: a demonstrated
// bug in a witness-blind ecosystem (att.WitnessOnly, NeedsHuman FALSE) must
// persist its ReproWitness. Before the fix, domain invariant (c) — written for
// the pre-qb4r below-quorum-only witness path — rejected the upsert
// ("ReproWitness set but NeedsHuman is false") and the evidence was lost.
func TestPromoteOne_WitnessOnly_PersistsWitness(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	repoDir := newRepoDir(t)

	finding := seedTSFinding(t, st)
	if finding.NeedsHuman {
		t.Fatal("setup: finding must NOT be NeedsHuman (that is the w1bh path, not qb4r layer b)")
	}

	client := newScriptedClient(planBody(t, witnessOnlyPlan()))
	// PatchProver enabled on purpose: witness-only must skip the cascade too.
	r, err := New(client, witnessOnlySandbox(), repoDir, Options{MaxAttempts: 1, ArtifactDir: t.TempDir(), PatchProver: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	outcome, err := r.PromoteOne(ctx, st, finding)
	if err != nil {
		t.Fatalf("PromoteOne: %v", err)
	}

	if !outcome.Witnessed {
		t.Errorf("outcome.Witnessed = false, want true (witness-only ecosystem must witness)")
	}
	if outcome.Promoted {
		t.Errorf("outcome.Promoted = true, want false (witness-only must not promote)")
	}
	if outcome.FixWitnessed {
		t.Errorf("outcome.FixWitnessed = true, want false (no patch-prover cascade for a witness)")
	}
	if outcome.ArtifactPath == "" {
		t.Errorf("outcome.ArtifactPath empty, want the witness bundle path")
	}

	got, err := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ReproWitness != outcome.ArtifactPath {
		t.Errorf("store repro_witness=%q != outcome.ArtifactPath=%q", got.ReproWitness, outcome.ArtifactPath)
	}
	if got.NeedsHuman {
		t.Errorf("witness set needs_human, want still false (qb4r layer b is not a human gate)")
	}
	if got.Tier != finding.Tier {
		t.Errorf("witness changed tier: got %d, want %d (no promotion)", got.Tier, finding.Tier)
	}
	if got.ReproPath != "" {
		t.Errorf("witness set repro_path=%q, want empty (no promotion)", got.ReproPath)
	}

	// Queue row is done: the attempt completed and its outcome persisted.
	ra, err := st.GetReproAttempt(ctx, finding.Fingerprint)
	if err != nil {
		t.Fatalf("GetReproAttempt: %v", err)
	}
	if ra.State != store.ReproStateDone {
		t.Errorf("queue state = %q, want done", ra.State)
	}
}

// TestPromoteOne_WitnessPersistFailure_RequeuesAttempt covers the loss mode
// bugbot-njb8 exposed: when the outcome persist fails AFTER a successful
// attempt, the queue row must be requeued (infra_retry, claimable) — not
// marked done, which is terminal and strands the finding with the artifact
// orphaned on disk. The persist failure is forced by never seeding the
// finding row: witnessFinding's read-back fails.
func TestPromoteOne_WitnessPersistFailure_RequeuesAttempt(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	repoDir := newRepoDir(t)

	// NOT upserted: promoteOne's persist step cannot find the row.
	finding := domain.Finding{
		ID:          "f-unpersisted",
		Fingerprint: domain.Fingerprint("concurrency", "src/app.ts", "42|unpersisted"),
		Title:       "unpersisted witness-only finding",
		Tier:        2,
		File:        "src/app.ts",
	}

	client := newScriptedClient(planBody(t, witnessOnlyPlan()))
	r, err := New(client, witnessOnlySandbox(), repoDir, Options{MaxAttempts: 1, ArtifactDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	outcome, err := r.PromoteOne(ctx, st, finding)
	if err == nil {
		t.Fatalf("PromoteOne err = nil, want persist failure (outcome=%+v)", outcome)
	}
	if outcome.Witnessed {
		t.Errorf("outcome.Witnessed = true, want false (nothing was persisted)")
	}

	// The attempt must be retryable: requeued as infra_retry, then claimable.
	ra, err := st.GetReproAttempt(ctx, finding.Fingerprint)
	if err != nil {
		t.Fatalf("GetReproAttempt: %v", err)
	}
	if ra.State != store.ReproStateInfraRetry {
		t.Errorf("queue state = %q, want infra_retry (a persist failure must not burn the row into done)", ra.State)
	}
	if _, err := st.ClaimReproAttempt(ctx, finding.Fingerprint); err != nil {
		t.Errorf("ClaimReproAttempt after persist failure: %v, want claimable", err)
	}
}

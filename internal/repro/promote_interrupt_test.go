package repro

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// cancellingClient simulates an operator interrupt landing mid-attempt: the
// first Complete call cancels the run context (as signal.NotifyContext does on
// Ctrl-C) and surfaces the cancellation, so promoteOne's error path executes
// with ctx already dead — the exact shape that used to leak the queue lease.
type cancellingClient struct {
	cancel context.CancelFunc
}

func (c *cancellingClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *cancellingClient) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	c.cancel()
	return llm.Response{}, ctx.Err()
}

// TestPromoteOne_InterruptReleasesClaim pins the interrupt-vs-crash split: a
// context cancellation during the attempt must release the queue row on a
// cancel-detached write (state back to 'pending', attempt refunded) instead of
// leaking it in 'running' — where every dispatch for the next
// ReproStaleLeaseDuration would report "skipped: already claimed" and three
// such cycles would abandon the row permanently.
func TestPromoteOne_InterruptReleasesClaim(t *testing.T) {
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok"}})
	r, err := New(&cancellingClient{cancel: cancel}, sb, repoDir, Options{ArtifactDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := promoteOne(ctx, r, st, finding)
	if err == nil {
		t.Fatal("promoteOne returned nil error for a cancelled attempt, want the cancellation surfaced")
	}
	if outcome.Skipped {
		t.Error("outcome.Skipped = true, want false (the attempt was claimed and interrupted, not skipped)")
	}

	// The queue row must be released, not leaked in 'running'.
	row, err := st.GetReproAttempt(context.Background(), finding.Fingerprint)
	if err != nil {
		t.Fatalf("GetReproAttempt: %v", err)
	}
	if row.State != store.ReproStatePending {
		t.Fatalf("state after interrupt = %s, want pending (leaked lease reports 'already claimed' for %s)",
			row.State, store.ReproStaleLeaseDuration)
	}
	if row.AttemptCount != 0 {
		t.Errorf("attempt_count after interrupt = %d, want 0 (interrupts must not burn the retry budget)", row.AttemptCount)
	}

	// A fresh dispatch immediately re-claims and completes — no
	// ErrReproAlreadyClaimed, no stale-lease wait.
	client := newScriptedClient(planBody(t, goodPlan()))
	r2, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	outcome2, err := promoteOne(context.Background(), r2, st, finding)
	if err != nil {
		t.Fatalf("promoteOne after interrupt: %v", err)
	}
	if outcome2.Skipped {
		t.Errorf("outcome.Skipped = true on redispatch after interrupt, want a genuine attempt (reason=%q)", outcome2.Reason)
	}
}

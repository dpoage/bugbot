package funnel

import (
	"context"
	"testing"

	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/agent"
)

// TestLedgeredRunner_ChargesEachPoolOnce: a ledgered finder client run by a
// pool-gated Runner charges the total pool and the finder sub-pool exactly
// once per completion (the Runner charges its own pool, the ledger the rest),
// and ledgers the completion under the finder role; a bare Complete on the
// same client ledgers through llmkit.Observe.
func TestLedgeredRunner_ChargesEachPoolOnce(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	rec := &spendRecorder{ctx: ctx, store: st, scanRunID: "run-ledger"}
	b := newBudgetState(10_000, rec, 1.0)
	b.reserveForDownstream(0.5)
	client := rec.ledgered(newScriptedClient(), roleFinder) // 100 in (60 cached) / 50 out => 150 chargeable at weight 1

	var out candidateList
	runner := f.newAgentRunner(client, nil, "sys", b.finderRunnerLimits(agent.Limits{}))
	if _, err := runner.RunJSON(ctx, "audit", candidatesSchema, &out); err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if got := b.finderPool.Spent(); got != 150 {
		t.Errorf("finder pool charged %d, want 150 (one completion, once)", got)
	}
	if got := b.pool.Spent(); got != 150 {
		t.Errorf("total pool charged %d, want 150", got)
	}
	if got := b.verifyPool.Spent(); got != 0 {
		t.Errorf("verify pool charged %d by a finder run, want 0", got)
	}

	if _, err := client.Complete(ctx, llmkit.Request{Messages: []llmkit.Message{llmkit.TextMessage(llmkit.RoleUser, "bare")}}); err != nil {
		t.Fatalf("bare Complete: %v", err)
	}
	if got := b.finderPool.Spent(); got != 300 {
		t.Errorf("finder pool after a bare completion = %d, want 300", got)
	}

	var role string
	var n int
	row := st.DB().QueryRowContext(ctx, `SELECT role, COUNT(*) FROM spend WHERE scan_run_id = ? GROUP BY role`, "run-ledger")
	if err := row.Scan(&role, &n); err != nil {
		t.Fatal(err)
	}
	if role != roleFinder || n != 2 {
		t.Errorf("ledger rows = %d under role %q, want 2 under %q", n, role, roleFinder)
	}
}

package engine

import (
	"context"
	"testing"

	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/agent"
)

// TestLedgerRecorder_IgnoresNonLedgerEvents: only successful completions are
// spend; failed completions and every other kind write no row.
func TestLedgerRecorder_IgnoresNonLedgerEvents(t *testing.T) {
	ctx := context.Background()
	st := openSpendStore(t)
	rec := newLedgerRecorder(ctx, st)
	rec.SetScanRun("run-i")

	failed := llmkit.NewEvent(ctx, llmkit.KindCompletion)
	failed.Completion = &llmkit.CompletionEvent{Err: "boom", Response: llmkit.Response{Usage: llmkit.Usage{InputTokens: 9}}}
	rec.Observe(ctx, failed)
	fin := llmkit.NewEvent(ctx, llmkit.KindFinalize)
	fin.Finalize = &llmkit.FinalizeEvent{Usage: llmkit.Usage{InputTokens: 9}}
	rec.Observe(ctx, fin)

	got, err := st.TotalsForScanRun(ctx, "run-i")
	if err != nil {
		t.Fatal(err)
	}
	if got.InputTokens != 0 {
		t.Errorf("totals = %+v, want none", got)
	}
}

type fakeUsageClient struct{ usage llmkit.Usage }

func (f fakeUsageClient) Capabilities() llmkit.Capabilities { return llmkit.Capabilities{} }
func (f fakeUsageClient) Identity() llmkit.Identity {
	return llmkit.Identity{Provider: "prov", Model: "model-m"}
}
func (f fakeUsageClient) Complete(_ context.Context, _ llmkit.Request) (llmkit.Response, error) {
	return llmkit.Response{Text: "{}", StopReason: llmkit.StopEndTurn, Usage: f.usage}, nil
}

// TestLedgerRecorder_LedgersRunnerCompletions pins the end-to-end seam: a
// Runner observed by the recorder ledgers each completion once, tagged with
// the repro role and the client's identity.
func TestLedgerRecorder_LedgersRunnerCompletions(t *testing.T) {
	ctx := context.Background()
	st := openSpendStore(t)
	rec := newLedgerRecorder(ctx, st)
	rec.SetScanRun("run-x")

	runner := agent.NewRunner(fakeUsageClient{usage: llmkit.Usage{InputTokens: 42, OutputTokens: 7}}, nil, "sys", agent.WithObserver(rec))
	if _, err := runner.Run(ctx, "task"); err != nil {
		t.Fatal(err)
	}

	var role, provider, model string
	var in, out int64
	row := st.DB().QueryRowContext(ctx, `SELECT role, provider, model, input_tokens, output_tokens FROM spend WHERE scan_run_id = ?`, "run-x")
	if err := row.Scan(&role, &provider, &model, &in, &out); err != nil {
		t.Fatal(err)
	}
	if role != reproLedgerRole || provider != "prov" || model != "model-m" || in != 42 || out != 7 {
		t.Errorf("ledger row = (%q, %q, %q, %d, %d), want (%q, prov, model-m, 42, 7)", role, provider, model, in, out, reproLedgerRole)
	}
}

package engine

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// TestLedgerRecorder_RecordsToStore pins bugbot-58c's fix: usage events from
// the repro-stage client land in the spend ledger with role and run id, and
// are visible to day-budget accounting (TotalsSince).
func TestLedgerRecorder_RecordsToStore(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	rec := newLedgerRecorder(ctx, st)
	rec.SetScanRun("run-1")
	rec.Record(llm.UsageEvent{
		Role: "reproducer", Provider: "minimax", Model: "MiniMax-M3",
		Usage: llm.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadInputTokens: 800},
	})
	// Retag mid-stream (the daemon path) and record again.
	rec.SetScanRun("run-2")
	rec.Record(llm.UsageEvent{
		Role: "reproducer", Provider: "minimax", Model: "MiniMax-M3",
		Usage: llm.Usage{InputTokens: 500, OutputTokens: 100},
	})

	run1, err := st.TotalsForScanRun(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if run1.InputTokens != 1000 || run1.OutputTokens != 200 || run1.CacheReadTokens != 800 {
		t.Errorf("run-1 totals = %+v", run1)
	}
	run2, err := st.TotalsForScanRun(ctx, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if run2.InputTokens != 500 {
		t.Errorf("run-2 totals = %+v", run2)
	}

	day, err := st.TotalsSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if day.InputTokens != 1500 || day.OutputTokens != 300 {
		t.Errorf("day totals = %+v, want in=1500 out=300 (repro spend must count toward day budget)", day)
	}
}

// TestLedgerRecorder_WrapsClient pins the end-to-end seam: a client resolved
// with the recorder ledgers each completion (role/provider/model tags applied
// by the llm layer).
func TestLedgerRecorder_WrapsClient(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	rec := newLedgerRecorder(ctx, st)
	rec.SetScanRun("run-x")

	inner := fakeUsageClient{usage: llm.Usage{InputTokens: 42, OutputTokens: 7}}
	client := llm.WithRecorder(inner, rec, "reproducer", "prov", "model-m")
	if _, err := client.Complete(ctx, llm.Request{}); err != nil {
		t.Fatal(err)
	}

	got, err := st.TotalsForScanRun(ctx, "run-x")
	if err != nil {
		t.Fatal(err)
	}
	if got.InputTokens != 42 || got.OutputTokens != 7 {
		t.Errorf("ledgered totals = %+v, want in=42 out=7", got)
	}
}

type fakeUsageClient struct{ usage llm.Usage }

func (f fakeUsageClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (f fakeUsageClient) Complete(_ context.Context, _ llm.Request) (llm.Response, error) {
	return llm.Response{Text: "{}", StopReason: llm.StopEndTurn, Usage: f.usage}, nil
}

// TestLedgerRecorder_ConcurrentRecords pins concurrency safety: PromoteAll
// drives the shared client from parallel workers.
func TestLedgerRecorder_ConcurrentRecords(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	rec := newLedgerRecorder(ctx, st)
	rec.SetScanRun("run-c")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec.Record(llm.UsageEvent{Role: "reproducer", Usage: llm.Usage{InputTokens: 10, OutputTokens: 1}})
		}()
	}
	wg.Wait()

	got, err := st.TotalsForScanRun(ctx, "run-c")
	if err != nil {
		t.Fatal(err)
	}
	if got.InputTokens != 80 || got.OutputTokens != 8 {
		t.Errorf("concurrent totals = %+v, want in=80 out=8", got)
	}
}

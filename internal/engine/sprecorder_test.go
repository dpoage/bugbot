package engine

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/store"
	llmkit "github.com/dpoage/llmkit"
)

func openSpendStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// observeCompletion delivers one successful completion event to rec.
func observeCompletion(rec *ledgerRecorder, u llmkit.Usage) {
	ev := llmkit.NewEvent(context.Background(), llmkit.KindCompletion)
	ev.Completion = &llmkit.CompletionEvent{Response: llmkit.Response{Usage: u}, Provider: "minimax", Model: "MiniMax-M3"}
	rec.Observe(context.Background(), ev)
}

// TestLedgerRecorder_RecordsToStore pins bugbot-58c's fix: usage from the
// repro stage lands in the spend ledger with run id and counts toward
// day-budget accounting (TotalsSince).
func TestLedgerRecorder_RecordsToStore(t *testing.T) {
	ctx := context.Background()
	st := openSpendStore(t)

	rec := newLedgerRecorder(ctx, st)
	rec.SetScanRun("run-1")
	observeCompletion(rec, llmkit.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadInputTokens: 800})
	// Retag mid-stream (the daemon path) and record again.
	rec.SetScanRun("run-2")
	observeCompletion(rec, llmkit.Usage{InputTokens: 500, OutputTokens: 100})

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

// TestLedgerRecorder_ConcurrentRecords pins concurrency safety: PromoteAll
// drives the shared client from parallel workers.
func TestLedgerRecorder_ConcurrentRecords(t *testing.T) {
	ctx := context.Background()
	st := openSpendStore(t)
	rec := newLedgerRecorder(ctx, st)
	rec.SetScanRun("run-c")

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			observeCompletion(rec, llmkit.Usage{InputTokens: 10, OutputTokens: 1})
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

// TestLedgerRecorder_OnRecordEmitsCumulativeTotals pins bugbot-psva's fix:
// each recorded completion invokes onRecord with run-cumulative totals (the
// KindSpendTick contract), so repro spend surfaces live.
func TestLedgerRecorder_OnRecordEmitsCumulativeTotals(t *testing.T) {
	st := openSpendStore(t)
	rec := newLedgerRecorder(context.Background(), st)
	type tick struct{ in, out, cached int64 }
	var ticks []tick
	rec.onRecord = func(in, out, cached int64) {
		ticks = append(ticks, tick{in, out, cached})
	}

	observeCompletion(rec, llmkit.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadInputTokens: 800})
	observeCompletion(rec, llmkit.Usage{InputTokens: 500, OutputTokens: 100})

	want := []tick{{1000, 200, 800}, {1500, 300, 800}}
	if len(ticks) != len(want) {
		t.Fatalf("onRecord called %d times, want %d", len(ticks), len(want))
	}
	for i := range want {
		if ticks[i] != want[i] {
			t.Errorf("tick[%d] = %+v, want %+v (totals must be cumulative)", i, ticks[i], want[i])
		}
	}
}

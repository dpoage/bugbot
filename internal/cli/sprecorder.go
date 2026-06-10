package cli

import (
	"context"
	"sync"

	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// ledgerRecorder is an llm.Recorder that writes every completion's usage to
// the store's spend ledger. It exists for the repro stage (reproducer +
// patch-prover agents), whose client lives OUTSIDE the funnel and therefore
// misses the funnel's per-run spendRecorder — before this, repro spend never
// reached the ledger and day-budget accounting undercounted exactly when the
// expensive sandbox stages ran (bugbot-58c).
//
// The scan-run id is settable because the daemon constructs the reproducer
// client once at startup and reuses it across cycles, each with a fresh run
// id. SetScanRun must be called before each PromoteAll; recording with an
// empty id is still correct for day-budget totals (TotalsSince does not
// filter by run), it merely loses per-run attribution.
type ledgerRecorder struct {
	ctx   context.Context
	store *store.Store

	mu        sync.Mutex
	scanRunID string
}

// newLedgerRecorder builds a recorder bound to the store. ctx is used for the
// ledger writes; pass the long-lived command/daemon context, not a per-call
// one.
func newLedgerRecorder(ctx context.Context, st *store.Store) *ledgerRecorder {
	return &ledgerRecorder{ctx: ctx, store: st}
}

// SetScanRun attributes subsequent records to the given scan run.
func (r *ledgerRecorder) SetScanRun(id string) {
	r.mu.Lock()
	r.scanRunID = id
	r.mu.Unlock()
}

// Record implements llm.Recorder. Ledger failures are swallowed: spend
// recording must never abort a repro run; the totals are best-effort
// accounting, not control flow.
func (r *ledgerRecorder) Record(ev llm.UsageEvent) {
	r.mu.Lock()
	id := r.scanRunID
	r.mu.Unlock()
	_, _ = r.store.RecordSpend(r.ctx, store.Spend{
		ScanRunID:           id,
		Role:                ev.Role,
		Provider:            ev.Provider,
		Model:               ev.Model,
		InputTokens:         ev.Usage.InputTokens,
		OutputTokens:        ev.Usage.OutputTokens,
		CacheReadTokens:     ev.Usage.CacheReadInputTokens,
		CacheCreationTokens: ev.Usage.CacheCreationInputTokens,
	})
}

var _ llm.Recorder = (*ledgerRecorder)(nil)

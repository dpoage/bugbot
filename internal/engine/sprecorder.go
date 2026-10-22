package engine

import (
	"context"
	"sync"

	"github.com/dpoage/bugbot/internal/store"
	llmkit "github.com/dpoage/llmkit"
)

// ledgerRecorder is an llmkit.Observer that writes every completion's usage
// to the store's spend ledger under the "reproducer" role. It exists for the
// repro stage (reproducer + patch-prover agents), whose client lives OUTSIDE
// the funnel and therefore misses the funnel's per-run spendRecorder — before
// this, repro spend never reached the ledger and day-budget accounting
// undercounted exactly when the expensive sandbox stages ran (bugbot-58c). It
// is installed on every reproducer/patch-prover Runner via repro.Options.Observer.
//
// The scan-run id is settable because the daemon constructs the reproducer
// client once at startup and reuses it across cycles, each with a fresh run
// id. SetScanRun must be called before each PromoteAll; recording with an
// empty id is still correct for day-budget totals (TotalsSince does not
// filter by run), it merely loses per-run attribution.
//
// Relocated from internal/cli/sprecorder.go: every call site (the scan repro
// hook and the daemon's reproducer builder) moved into this package, so this
// cobra-free, store-only helper moved with them rather than being left
// orphaned in internal/cli. daemon.Deps.ReproTagger is a structural
// interface (ScanRunTagger{SetScanRun(id string)}), so *ledgerRecorder still
// satisfies it from here without either package needing to name the other's
// type.
type ledgerRecorder struct {
	ctx   context.Context
	store *store.Store

	mu        sync.Mutex
	scanRunID string
	// in/out/cacheRead accumulate every recorded completion's usage, mirroring
	// the funnel spendRecorder's counters, so onRecord can report
	// run-cumulative totals suitable for KindSpendTick.
	in, out, cacheRead int64

	// onRecord, when non-nil, is called after each recorded completion with
	// the new cumulative input/output/cache-read totals (progress spend ticks,
	// Role=RoleReproducer). It must be cheap and non-blocking.
	onRecord func(in, out, cached int64)
}

// reproLedgerRole is the spend-ledger role of every repro-stage completion.
const reproLedgerRole = "reproducer"

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

// Observe ledgers each successful completion event. Ledger failures are
// swallowed: spend recording must never abort a repro run.
func (r *ledgerRecorder) Observe(_ context.Context, ev llmkit.Event) {
	if ev.Kind != llmkit.KindCompletion || ev.Completion == nil || ev.Completion.Err != "" {
		return
	}
	c := ev.Completion
	u := c.Response.Usage
	r.mu.Lock()
	id := r.scanRunID
	r.in += u.InputTokens
	r.out += u.OutputTokens
	r.cacheRead += u.CacheReadInputTokens
	in, out, cached := r.in, r.out, r.cacheRead
	cb := r.onRecord
	r.mu.Unlock()
	if cb != nil {
		cb(in, out, cached)
	}
	_, _ = r.store.RecordSpend(r.ctx, store.Spend{
		ScanRunID:           id,
		Role:                reproLedgerRole,
		Provider:            c.Provider,
		Model:               c.Model,
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		CacheReadTokens:     u.CacheReadInputTokens,
		CacheCreationTokens: u.CacheCreationInputTokens,
	})
}

var _ llmkit.Observer = (*ledgerRecorder)(nil)

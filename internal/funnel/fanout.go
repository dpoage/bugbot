package funnel

import (
	"context"
	"sync"
)

// fanout is the funnel's one bounded-fan-out concurrency primitive. Four stages
// (the finder launch loop, the cartographer's per-package summaries, the
// streaming verifier dispatcher, and — historically — the in-run reproducer)
// independently hand-rolled the same scaffold: spawn one goroutine per unit,
// acquire a slot from the funnel-wide pool, defer its release, run the unit, and
// join everything on a WaitGroup, all under a derived cancellable context so a
// goroutine still queued on slot acquisition unblocks the moment the run is
// stopped. fanout owns exactly that scaffold and nothing else.
//
// What it owns:
//   - a derived, cancellable runCtx (child of the caller's ctx), so stop()
//     unblocks any unit still parked in slots.acquire;
//   - one goroutine per spawned unit;
//   - slot acquire (on runCtx, for the configured class) / deferred release;
//   - the "acquire returned an error => the unit never runs" guard;
//   - the shared WaitGroup that wait() joins.
//
// What it deliberately does NOT own — these stay in the caller's unit closure,
// because they differ at every site:
//   - the iteration itself (a slice-index loop or a channel range) and any
//     pre-launch break (the finder's firstErr/breaker check, the cartographer's
//     budget check);
//   - post-acquire budget gating (finderOverHard/Soft, verifyOverHard/Soft);
//   - the actual agent run;
//   - result handling under the caller's own mutex, and the precise placement of
//     emit / onResult / append / persist relative to that mutex.
//
// fanout also does NOT recheck runCtx after a successful acquire. The slot
// pool's fast path intentionally hands out a free slot without a ctx check so a
// unit proceeds even on an already-cancelled ctx (load-bearing for the funnel's
// interrupted-run classification — see slotPool.acquire). A site that genuinely
// wants a post-acquire recheck (the cartographer, which must not start a summary
// once the run is cancelled) performs it itself as the first line of its unit.
type fanout struct {
	pool   *slotPool
	class  slotClass
	runCtx context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newFanout derives a cancellable child of ctx and returns a fanout that draws
// `class` slots from the funnel's shared pool. The caller drives spawning and
// must call wait exactly once.
func (f *Funnel) newFanout(ctx context.Context, class slotClass) *fanout {
	runCtx, cancel := context.WithCancel(ctx)
	return &fanout{pool: f.slots, class: class, runCtx: runCtx, cancel: cancel}
}

// spawn launches unit in its own goroutine. The goroutine first acquires a slot
// on the derived runCtx (so stop unblocks it); if acquisition fails — the run
// was stopped, or the caller's ctx was cancelled, while it waited — unit never
// runs. On success the slot is held for the whole of unit and released when unit
// returns. unit receives the derived runCtx and must use it for the run so that
// stop can cancel an in-flight unit.
func (fo *fanout) spawn(unit func(ctx context.Context)) {
	fo.wg.Add(1)
	go func() {
		defer fo.wg.Done()
		if err := fo.pool.acquire(fo.runCtx, fo.class); err != nil {
			return
		}
		defer fo.pool.release()
		unit(fo.runCtx)
	}()
}

// stop cancels the run: any unit still queued on slot acquisition returns at
// once, and an in-flight unit observes runCtx cancellation. It is idempotent and
// safe to call from any goroutine (e.g. a unit tripping a circuit breaker).
func (fo *fanout) stop() { fo.cancel() }

// wait blocks until every spawned unit has finished, then cancels the derived
// context. It must be called exactly once; the fanout is spent afterward.
func (fo *fanout) wait() {
	fo.wg.Wait()
	fo.cancel()
}
